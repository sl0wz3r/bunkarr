//go:build linux

package filecopy

import (
	"errors"
	"fmt"
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
	return uint64(st.Dev), uint64(st.Ino), uint64(st.Nlink), time.Unix(st.Atim.Unix()), true
}

// dropCache asks the kernel to drop f's cached pages (posix_fadvise DONTNEED), so the next read
// comes from the storage (or, on NFS/SMB, from the server). Best effort: errors are ignored.
func dropCache(f *os.File) {
	sc, err := f.SyscallConn()
	if err != nil {
		return
	}
	_ = sc.Control(func(fd uintptr) {
		_ = unix.Fadvise(int(fd), 0, 0, unix.FADV_DONTNEED)
	})
}

// renameNoReplaceAt renames oldName in oldDir to newName in newDir, failing with EEXIST when
// newName exists (renameat2 RENAME_NOREPLACE). supported is false when the kernel or the
// filesystem does not implement the flag (NFS, older kernels), in which case nothing was done.
func renameNoReplaceAt(oldDir *os.File, oldName string, newDir *os.File, newName string) (supported bool, err error) {
	err = withFD2(oldDir, newDir, func(ofd, nfd int) error {
		return ignoringEINTR(func() error {
			return unix.Renameat2(ofd, oldName, nfd, newName, unix.RENAME_NOREPLACE)
		})
	})
	if errors.Is(err, syscall.EINVAL) || errors.Is(err, syscall.ENOSYS) || errors.Is(err, syscall.EOPNOTSUPP) || errors.Is(err, syscall.ENOTSUP) {
		return false, nil
	}
	return true, err
}

// fsMagicNames maps statfs f_type magic numbers to names (see statfs(2)).
var fsMagicNames = map[uint32]string{
	0xFF534D42: "cifs",
	0xFE534D42: "smb2",
	0x0000517B: "smb",
	0x00006969: "nfs",
	0x0000EF53: "ext4",
	0x58465342: "xfs",
	0x9123683E: "btrfs",
	0x2FC12FC1: "zfs",
	0x01021994: "tmpfs",
	0x794C7630: "overlay",
	0x65735546: "fuse",
	0x858458F6: "ramfs",
	0x00004D44: "vfat",
	0x2011BAB0: "exfat",
	0x5346544E: "ntfs",
	0x7366746E: "ntfs3",
	0xF2F52010: "f2fs",
}

// fsTypeName names a statfs magic number; unknown ones are rendered in hex.
func fsTypeName(magic uint32) string {
	if n, ok := fsMagicNames[magic]; ok {
		return n
	}
	return fmt.Sprintf("0x%x", magic)
}

// statfsFile runs fstatfs on f.
func statfsFile(f *os.File) (FSStat, error) {
	var st unix.Statfs_t
	err := withFD(f, func(fd int) error {
		return ignoringEINTR(func() error { return unix.Fstatfs(fd, &st) })
	})
	if err != nil {
		return FSStat{}, err
	}
	bsize := uint64(st.Frsize)
	if bsize == 0 {
		bsize = uint64(st.Bsize)
	}
	return FSStat{
		Type:  fsTypeName(uint32(st.Type)),
		Free:  st.Bavail * bsize,
		Total: st.Blocks * bsize,
	}, nil
}
