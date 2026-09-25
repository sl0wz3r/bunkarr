package catalog

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"syscall"
)

// devIno identifies an inode within one scan.
type devIno struct{ dev, ino uint64 }

// linkCand is a file seen with nlink >= 2.
type linkCand struct {
	rel string
	m   Meta
}

// planGroups applies design §4.3 to the candidates of one scan: names with equal (dev, ino, size,
// mtime, ctime, nlink) and nlink >= 2 form a group; a bucket whose members disagree, or that has
// more members than nlink (inode reuse during the walk), is split into single files with a
// warning. Buckets with one member are single files. The result is ordered by the first member's
// path, members in path order, so group numbers are deterministic.
func planGroups(cands map[devIno][]linkCand) (groups [][]linkCand, warnings []string) {
	for key, members := range cands {
		if len(members) < 2 {
			continue
		}
		first := members[0].m
		agree := true
		for _, c := range members[1:] {
			if c.m != first {
				agree = false
				break
			}
		}
		sorted := slices.Clone(members)
		slices.SortFunc(sorted, func(a, b linkCand) int { return cmp.Compare(a.rel, b.rel) })
		switch {
		case !agree:
			warnings = append(warnings, fmt.Sprintf("hardlinks not grouped: the %d names of inode %d on device %d (%s, ...) changed during the scan; they are treated as separate files until the next scan",
				len(sorted), key.ino, key.dev, sorted[0].rel))
		case uint64(len(sorted)) > first.Nlink:
			warnings = append(warnings, fmt.Sprintf("hardlinks not grouped: %d names share inode %d on device %d but its link count is %d (%s, ...); they are treated as separate files",
				len(sorted), key.ino, key.dev, first.Nlink, sorted[0].rel))
		default:
			groups = append(groups, sorted)
		}
	}
	slices.SortFunc(groups, func(a, b []linkCand) int { return cmp.Compare(a[0].rel, b[0].rel) })
	slices.Sort(warnings)
	return groups, warnings
}

// headTailSize is how much of each end of a file the FUSE hardlink check hashes.
const headTailSize = 1 << 20

// errChangedWhileHashing means the file at a path is no longer the one the walk saw.
var errChangedWhileHashing = errors.New("file changed since it was scanned")

// headTailHash returns the sha256 of the first and last MiB of the regular file rel (the whole
// file when it is at most 2 MiB). The file is opened read-only through the source's os.Root with
// O_NOFOLLOW|O_NONBLOCK, and must still be the inode the walk saw (want): os.Root may resolve a
// symlink that stays inside the root, so the identity check is what guarantees no link is
// followed and no FIFO is read.
func headTailHash(root *os.Root, rel string, want Meta) (string, error) {
	f, err := root.OpenFile(rel, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return "", err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return "", err
	}
	m, ok := MetaOf(fi)
	if !ok || !fi.Mode().IsRegular() || m.Dev != want.Dev || m.Inode != want.Inode || m.Size != want.Size || m.MtimeNs != want.MtimeNs {
		return "", errChangedWhileHashing
	}
	h := sha256.New()
	head := min(m.Size, headTailSize)
	if _, err := io.CopyN(h, f, head); err != nil {
		return "", fmt.Errorf("read %s: %w", rel, err)
	}
	if m.Size > head {
		start := max(head, m.Size-headTailSize)
		if _, err := f.Seek(start, io.SeekStart); err != nil {
			return "", fmt.Errorf("read %s: %w", rel, err)
		}
		if _, err := io.CopyN(h, f, m.Size-start); err != nil {
			return "", fmt.Errorf("read %s: %w", rel, err)
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
