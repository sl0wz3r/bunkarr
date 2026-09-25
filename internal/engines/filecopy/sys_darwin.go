//go:build darwin

package filecopy

import (
	"errors"
	"io/fs"
	"os"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// sysStat extracts the Unix fields of fi.
func sysStat(fi fs.FileInfo) (dev, ino, nlink uint64, atime time.Time, ok bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, 0, time.Time{}, false
	}
	return uint64(st.Dev), uint64(st.Ino), uint64(st.Nlink), time.Unix(st.Atimespec.Unix()), true
}

// dropCache turns data caching off for f (fcntl F_NOCACHE), the closest macOS has to
// posix_fadvise DONTNEED. Best effort: errors are ignored. macOS is a development platform only.
func dropCache(f *os.File) {
	sc, err := f.SyscallConn()
	if err != nil {
		return
	}
	_ = sc.Control(func(fd uintptr) {
		_, _ = unix.FcntlInt(fd, unix.F_NOCACHE, 1)
	})
}

// renameNoReplaceAt renames oldName in oldDir to newName in newDir, failing with EEXIST when
// newName exists (renameatx_np RENAME_EXCL). supported is false when the filesystem does not
// implement the flag, in which case nothing was done.
func renameNoReplaceAt(oldDir *os.File, oldName string, newDir *os.File, newName string) (supported bool, err error) {
	err = withFD2(oldDir, newDir, func(ofd, nfd int) error {
		return ignoringEINTR(func() error {
			return unix.RenameatxNp(ofd, oldName, nfd, newName, unix.RENAME_EXCL)
		})
	})
	if errors.Is(err, syscall.EINVAL) || errors.Is(err, syscall.ENOSYS) || errors.Is(err, syscall.ENOTSUP) || errors.Is(err, syscall.EOPNOTSUPP) {
		return false, nil
	}
	return true, err
}

// statfsFile runs fstatfs on f. The type is the kernel's file system name (apfs, smbfs, nfs, ...).
func statfsFile(f *os.File) (FSStat, error) {
	var st unix.Statfs_t
	err := withFD(f, func(fd int) error {
		return ignoringEINTR(func() error { return unix.Fstatfs(fd, &st) })
	})
	if err != nil {
		return FSStat{}, err
	}
	name := unix.ByteSliceToString(st.Fstypename[:])
	return FSStat{
		Type:  name,
		Free:  st.Bavail * uint64(st.Bsize),
		Total: st.Blocks * uint64(st.Bsize),
	}, nil
}
