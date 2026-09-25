//go:build darwin

package catalog

import (
	"fmt"
	"io/fs"
	"os"
	"strings"
	"syscall"
)

// metaOf reads the catalog metadata from an lstat/stat result.
func metaOf(fi fs.FileInfo) (Meta, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || st == nil {
		return Meta{}, false
	}
	return Meta{
		Size:    fi.Size(),
		MtimeNs: st.Mtimespec.Nano(),
		CtimeNs: st.Ctimespec.Nano(),
		// dev_t is a signed 32-bit value on darwin; keep its bit pattern, never sign-extend.
		Dev:   uint64(uint32(st.Dev)),
		Inode: st.Ino,
		Nlink: uint64(st.Nlink),
	}, true
}

// fsTypeString converts statfs f_fstypename.
func fsTypeString(raw [16]int8) (name string, fuse bool) {
	b := make([]byte, 0, len(raw))
	for _, c := range raw {
		if c == 0 {
			break
		}
		b = append(b, byte(c))
	}
	name = string(b)
	return name, strings.Contains(name, "fuse")
}

// fsTypeOfFile returns the filesystem type of an open file or directory (fstatfs).
func fsTypeOfFile(f *os.File) (name string, fuse bool, err error) {
	rc, err := f.SyscallConn()
	if err != nil {
		return "", false, fmt.Errorf("statfs %s: %w", f.Name(), err)
	}
	var st syscall.Statfs_t
	var serr error
	if err := rc.Control(func(fd uintptr) { serr = syscall.Fstatfs(int(fd), &st) }); err != nil {
		return "", false, fmt.Errorf("statfs %s: %w", f.Name(), err)
	}
	if serr != nil {
		return "", false, &os.PathError{Op: "fstatfs", Path: f.Name(), Err: serr}
	}
	name, fuse = fsTypeString(st.Fstypename)
	return name, fuse, nil
}

// fsTypeOfPath returns the filesystem type of path (statfs).
func fsTypeOfPath(path string) (name string, fuse bool, err error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return "", false, &os.PathError{Op: "statfs", Path: path, Err: err}
	}
	name, fuse = fsTypeString(st.Fstypename)
	return name, fuse, nil
}

// anonDevFS are network filesystems whose st_dev is assigned at mount time.
var anonDevFS = map[string]bool{"nfs": true, "smbfs": true, "afpfs": true, "webdav": true, "cifs": true}

// anonymousDev reports whether the st_dev of a directory on fsType is not stable across mounts: a
// network filesystem or FUSE (macFUSE, FUSE-T).
func anonymousDev(fsType string, _ uint64) bool {
	return anonDevFS[fsType] || strings.Contains(fsType, "fuse")
}

// runTimeInoFS are network filesystems whose inode numbers the client may assign.
var runTimeInoFS = map[string]bool{"smbfs": true, "cifs": true, "afpfs": true, "webdav": true}

// stableRootIno reports whether a directory on fsType keeps its inode number across a remount: not
// on runTimeInoFS or FUSE, whose daemons number what they are asked about.
func stableRootIno(fsType string) bool {
	return !runTimeInoFS[fsType] && !strings.Contains(fsType, "fuse")
}
