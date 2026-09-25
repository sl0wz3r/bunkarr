//go:build linux

package catalog

import (
	"fmt"
	"io/fs"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// fuseSuperMagic is statfs f_type of every FUSE filesystem (Unraid's /mnt/user shfs included).
const fuseSuperMagic = 0x65735546

// fsNames maps statfs f_type magic numbers (linux/magic.h) to the names Bunkarr stores and shows.
var fsNames = map[uint32]string{
	0xEF53:     "ext4", // ext2, ext3 and ext4 share the magic
	0x58465342: "xfs",
	0x9123683E: "btrfs",
	0x2FC12FC1: "zfs",
	0xF2F52010: "f2fs",
	0x52654973: "reiserfs",
	0x5346544E: "ntfs",
	0x2011BAB0: "exfat",
	0x4D44:     "vfat",
	0x65735546: "fuse",
	0x6969:     "nfs",
	0xFF534D42: "cifs",
	0xFE534D42: "smb2",
	0x517B:     "smb",
	0x01021994: "tmpfs",
	0x858458F6: "ramfs",
	0x794C7630: "overlay",
	0x73717368: "squashfs",
	0x9660:     "iso9660",
	0x9FA0:     "proc",
	0x62656572: "sysfs",
	0x01021997: "9p",
	0x00C36400: "ceph",
	0x65735543: "fusectl",
	0x3153464A: "jfs",
	0xCAFE4A11: "bpf",
}

// fsTypeName renders a statfs f_type for storage: a known name or the magic as 0x%x.
func fsTypeName(magic uint32) string {
	if n, ok := fsNames[magic]; ok {
		return n
	}
	return fmt.Sprintf("0x%x", magic)
}

// metaOf reads the catalog metadata from an lstat/stat result.
func metaOf(fi fs.FileInfo) (Meta, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || st == nil {
		return Meta{}, false
	}
	return Meta{
		Size:    fi.Size(),
		MtimeNs: st.Mtim.Nano(),
		CtimeNs: st.Ctim.Nano(),
		// The conversions matter on architectures where these fields are narrower.
		Dev:   uint64(st.Dev),
		Inode: uint64(st.Ino),
		Nlink: uint64(st.Nlink),
	}, true
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
	magic := uint32(st.Type) // a 32-bit magic stored in a wider, sometimes signed, field
	return fsTypeName(magic), magic == fuseSuperMagic, nil
}

// fsTypeOfPath returns the filesystem type of path (statfs).
func fsTypeOfPath(path string) (name string, fuse bool, err error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return "", false, &os.PathError{Op: "statfs", Path: path, Err: err}
	}
	magic := uint32(st.Type)
	return fsTypeName(magic), magic == fuseSuperMagic, nil
}

// anonDevFS are filesystems whose st_dev can change after a reboot or remount: the kernel assigns
// an anonymous device at mount time (FUSE, NFS, CIFS, ZFS, overlay) or one per btrfs subvolume, and
// fuseblk reports whichever block device it was mounted from.
var anonDevFS = map[string]bool{
	"fuse": true, "nfs": true, "cifs": true, "smb2": true, "smb": true, "btrfs": true, "zfs": true, "overlay": true,
}

// anonymousDev reports whether dev, the st_dev of a directory on fsType, is not stable across
// mounts: an anonymous device (major 0: FUSE, NFS, CIFS, tmpfs, ...) or one of anonDevFS.
func anonymousDev(fsType string, dev uint64) bool {
	return unix.Major(dev) == 0 || anonDevFS[fsType]
}

// runTimeInoFS are filesystems whose inode numbers are assigned at run time, so a directory can
// have another one after a remount: FUSE daemons number what they are asked about, and CIFS/SMB
// generates them when the server does not provide them (noserverino).
var runTimeInoFS = map[string]bool{"fuse": true, "cifs": true, "smb2": true, "smb": true}

// stableRootIno reports whether a directory on fsType keeps its inode number across a remount
// (NFS, btrfs, ZFS, tmpfs and disk filesystems do).
func stableRootIno(fsType string) bool {
	return !runTimeInoFS[fsType]
}
