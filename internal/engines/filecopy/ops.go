package filecopy

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/faultinject"
)

// maxRetentionSuffix bounds the numeric suffixes Retain tries.
const maxRetentionSuffix = 9999

// Link creates newRel as a hardlink to the regular file existingRel (design §4.2 link, and the
// update sequence's link into retention). newRel's parent directories are created. An existing
// newRel is never replaced (ErrExists).
func Link(dst *os.Root, existingRel, newRel string) error {
	if err := checkRel(existingRel); err != nil {
		return dstErr("link", existingRel, err)
	}
	if err := checkWritable(newRel); err != nil {
		return dstErr("link", newRel, err)
	}
	fi, err := dst.Lstat(existingRel)
	if err != nil {
		return dstErr("link", existingRel, err)
	}
	if !fi.Mode().IsRegular() {
		return dstErr("link", existingRel, ErrNotRegular)
	}
	if dir := parentDir(newRel); dir != "." {
		if err := dst.MkdirAll(dir, DefaultDirPerm); err != nil {
			return dstErr("mkdir", dir, err)
		}
	}
	if err := dst.Link(existingRel, newRel); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return dstErr("link", newRel, ErrExists)
		}
		return dstErr("link", existingRel+" → "+newRel, err)
	}
	faultinject.Point("link.afterLink")
	if err := syncDir(dst, parentDir(newRel)); err != nil {
		return dstErr("fsync", parentDir(newRel), err)
	}
	return nil
}

// Move renames the regular file fromRel to toRel inside the destination (a same-filesystem
// rename: design §4.2 move, promote, retain, displace), creating toRel's parent directories and
// fsyncing both directories. With noReplace an existing toRel is never replaced (ErrExists; see
// Commit for how that is enforced).
func Move(dst *os.Root, fromRel, toRel string, noReplace bool) error {
	if err := checkWritable(fromRel); err != nil {
		return dstErr("move", fromRel, err)
	}
	if err := checkWritable(toRel); err != nil {
		return dstErr("move", toRel, err)
	}
	fi, err := dst.Lstat(fromRel)
	if err != nil {
		return dstErr("move", fromRel, err)
	}
	if !fi.Mode().IsRegular() {
		return dstErr("move", fromRel, ErrNotRegular)
	}
	if dir := parentDir(toRel); dir != "." {
		if err := dst.MkdirAll(dir, DefaultDirPerm); err != nil {
			return dstErr("mkdir", dir, err)
		}
	}
	if err := rename(dst, fromRel, toRel, noReplace); err != nil {
		return err
	}
	faultinject.Point("move.afterRename")
	return syncDirs(dst, fromRel, toRel)
}

// RenameDir renames the directory fromRel to toRel inside the destination (a Plex DB backup's
// ".partial-job<id>" directory to its timestamp, design §5), creating toRel's parent
// directories and fsyncing both parents. An existing toRel, even an empty directory that
// rename(2) would silently replace, is never replaced (ErrExists).
func RenameDir(dst *os.Root, fromRel, toRel string) error {
	if err := checkWritable(fromRel); err != nil {
		return dstErr("rename", fromRel, err)
	}
	if err := checkWritable(toRel); err != nil {
		return dstErr("rename", toRel, err)
	}
	if fromRel == toRel || strings.HasPrefix(toRel, fromRel+"/") {
		return dstErr("rename", toRel, fmt.Errorf("%w: cannot move a directory into itself", ErrInvalidPath))
	}
	fi, err := dst.Lstat(fromRel)
	if err != nil {
		return dstErr("rename", fromRel, err)
	}
	if !fi.IsDir() {
		return dstErr("rename", fromRel, fmt.Errorf("%w: not a directory", ErrInvalidPath))
	}
	if dir := parentDir(toRel); dir != "." {
		if err := dst.MkdirAll(dir, DefaultDirPerm); err != nil {
			return dstErr("mkdir", dir, err)
		}
	}
	if err := rename(dst, fromRel, toRel, true); err != nil {
		return err
	}
	faultinject.Point("move.afterRename")
	return syncDirs(dst, fromRel, toRel)
}

// RetentionDir returns the retention directory of a job:
// ".bunkarr/retention/<queuedAt as yyyymmddThhmmssZ>-job<id>". It depends only on the job, so a
// resumed job reuses it.
func RetentionDir(queuedAt time.Time, jobID int64) string {
	return RetentionRoot + "/" + queuedAt.UTC().Format("20060102T150405Z") + "-job" + strconv.FormatInt(jobID, 10)
}

// checkRetentionDir validates a directory made by RetentionDir.
func checkRetentionDir(dir string) error {
	if err := checkRel(dir); err != nil {
		return err
	}
	rest, ok := strings.CutPrefix(dir, RetentionRoot+"/")
	if !ok || rest == "" || strings.Contains(rest, "/") {
		return fmt.Errorf("%w: %q is not a job retention directory", ErrOutsideRetention, dir)
	}
	return nil
}

// RetentionTarget returns the path rel would get inside retentionDir: retentionDir/rel, or, when
// that exists (the same path retained twice by one job), the first free name with a numeric
// suffix before the extension ("movie.1.mkv", "movie.2.mkv", ...). Callers that must record the
// retained path before moving (resume) use it with Move or Link; Retain does both.
func RetentionTarget(dst *os.Root, retentionDir, rel string) (string, error) {
	if err := checkRetentionDir(retentionDir); err != nil {
		return "", dstErr("retain", retentionDir, err)
	}
	if err := checkRel(rel); err != nil {
		return "", dstErr("retain", rel, err)
	}
	base := path.Join(retentionDir, rel)
	for i := 0; i <= maxRetentionSuffix; i++ {
		cand := suffixed(base, i)
		_, err := dst.Lstat(cand)
		if errors.Is(err, fs.ErrNotExist) {
			return cand, nil
		}
		if err != nil {
			return "", dstErr("retain", cand, err)
		}
	}
	return "", dstErr("retain", base, fmt.Errorf("%w: %d retained versions", ErrExists, maxRetentionSuffix+1))
}

// suffixed returns p with ".<n>" inserted before the extension (p itself for n == 0).
func suffixed(p string, n int) string {
	if n == 0 {
		return p
	}
	dir, base := path.Split(p)
	ext := path.Ext(base)
	if ext == base { // ".hidden" has no extension
		ext = ""
	}
	return dir + strings.TrimSuffix(base, ext) + "." + strconv.Itoa(n) + ext
}

// Retain moves the regular file rel into retentionDir (made by RetentionDir), keeping its
// relative path, and returns where it went (design S5). An existing retained file is never
// replaced: a numeric suffix is added instead (see RetentionTarget).
func Retain(dst *os.Root, rel, retentionDir string) (string, error) {
	if err := checkWritable(rel); err != nil {
		return "", dstErr("retain", rel, err)
	}
	for range maxRetentionSuffix + 1 {
		target, err := RetentionTarget(dst, retentionDir, rel)
		if err != nil {
			return "", err
		}
		err = Move(dst, rel, target, true)
		if errors.Is(err, ErrExists) {
			continue // taken between the check and the rename: pick the next name
		}
		if err != nil {
			return "", err
		}
		return target, nil
	}
	return "", dstErr("retain", rel, ErrExists)
}

// Expire deletes a retained file whose retention period is over (design §4.2 expire). It refuses
// any path that is not strictly inside a job directory under .bunkarr/retention (including any
// path with ".." or a symlinked directory on the way), anything but a regular file, and a file
// whose size is not wantSize (ErrMismatch: it is not the file that was retained). Empty parent
// directories are removed up to, not including, .bunkarr/retention. A file that is already gone
// returns an error matching fs.ErrNotExist (the caller treats the expiry as done); its empty
// parents are pruned all the same.
func Expire(dst *os.Root, retainedRel string, wantSize int64) error {
	if err := checkRel(retainedRel); err != nil {
		return dstErr("expire", retainedRel, err)
	}
	rest, ok := strings.CutPrefix(retainedRel, RetentionRoot+"/")
	if !ok || !strings.Contains(rest, "/") {
		return dstErr("expire", retainedRel, ErrOutsideRetention)
	}
	// Every directory on the way must be a real directory, so a symlink planted inside the
	// retention tree cannot point the removal at a live file.
	for dir := parentDir(retainedRel); dir != "."; dir = parentDir(dir) {
		fi, err := dst.Lstat(dir)
		if errors.Is(err, fs.ErrNotExist) {
			continue // a missing directory: the file is gone too (reported below)
		}
		if err != nil {
			return dstErr("expire", dir, err)
		}
		if !fi.IsDir() {
			return dstErr("expire", dir, fmt.Errorf("%w: %s is not a directory", ErrOutsideRetention, dir))
		}
	}
	fi, err := dst.Lstat(retainedRel)
	if errors.Is(err, fs.ErrNotExist) {
		pruneRetention(dst, parentDir(retainedRel))
		return dstErr("expire", retainedRel, err)
	}
	if err != nil {
		return dstErr("expire", retainedRel, err)
	}
	if !fi.Mode().IsRegular() {
		return dstErr("expire", retainedRel, ErrNotRegular)
	}
	if fi.Size() != wantSize {
		return dstErr("expire", retainedRel, fmt.Errorf("%w: size %d, recorded %d", ErrMismatch, fi.Size(), wantSize))
	}
	if err := dst.Remove(retainedRel); err != nil {
		return dstErr("expire", retainedRel, err)
	}
	faultinject.Point("expire.afterRemove")
	pruneRetention(dst, parentDir(retainedRel))
	return nil
}

// pruneRetention removes dir and its parents while they are empty directories below
// RetentionRoot. Best effort: it stops at the first directory it cannot remove.
func pruneRetention(dst *os.Root, dir string) {
	for strings.HasPrefix(dir, RetentionRoot+"/") {
		fi, err := dst.Lstat(dir)
		if err != nil || !fi.IsDir() {
			return
		}
		if err := dst.Remove(dir); err != nil {
			return // not empty (ENOTEMPTY/EEXIST) or not removable
		}
		dir = parentDir(dir)
	}
}

// MtimeMatch reports whether modification times a and b (Unix ns) are equal as far as a
// destination with the given granularity can tell (they differ by less than granularityNs;
// covers both truncating and rounding filesystems), or differ by at most windowSec seconds (like
// rsync --modify-window).
func MtimeMatch(a, b, granularityNs int64, windowSec int) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	if granularityNs < 1 {
		granularityNs = 1
	}
	return d < granularityNs || d <= int64(windowSec)*int64(time.Second)
}

// AdoptCheck looks at rel before a copy (design §4.4): exists reports whether anything is there;
// match whether it is a regular file of the given size whose mtime matches mtimeNs (MtimeMatch).
// st describes what is there when exists. A path whose parent is not a directory returns an
// error.
func AdoptCheck(dst *os.Root, rel string, size, mtimeNs, granularityNs int64, windowSec int) (exists, match bool, st Stat, err error) {
	st, err = Lstat(dst, rel)
	if errors.Is(err, fs.ErrNotExist) {
		return false, false, Stat{}, nil
	}
	if err != nil {
		return false, false, Stat{}, dstErr("stat", rel, err)
	}
	match = st.Regular() && st.Size == size && MtimeMatch(st.MtimeNs, mtimeNs, granularityNs, windowSec)
	return true, match, st, nil
}

// SetMtime sets the modification time of the regular file rel (adoption by size+hash sets the
// destination mtime to the source's); the access time is left unchanged.
func SetMtime(dst *os.Root, rel string, mtimeNs int64) error {
	if err := checkWritable(rel); err != nil {
		return dstErr("chtimes", rel, err)
	}
	fi, err := dst.Lstat(rel)
	if err != nil {
		return dstErr("chtimes", rel, err)
	}
	if !fi.Mode().IsRegular() {
		return dstErr("chtimes", rel, ErrNotRegular)
	}
	if err := dst.Chtimes(rel, time.Time{}, time.Unix(0, mtimeNs)); err != nil {
		return dstErr("chtimes", rel, err)
	}
	return nil
}
