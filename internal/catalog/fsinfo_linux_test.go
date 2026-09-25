//go:build linux

package catalog

import (
	"testing"

	"golang.org/x/sys/unix"
)

func TestAnonymousDevLinux(t *testing.T) {
	for _, tc := range []struct {
		fsType string
		dev    uint64
		want   bool
	}{
		{"xfs", unix.Mkdev(9, 1), false},    // an Unraid array disk (/dev/md1p1)
		{"ext4", unix.Mkdev(8, 17), false},  // /dev/sdb1
		{"xfs", unix.Mkdev(259, 3), false},  // an NVMe pool
		{"tmpfs", unix.Mkdev(0, 63), true},  // anonymous: major 0
		{"fuse", unix.Mkdev(0, 70), true},   // Unraid's /mnt/user (shfs)
		{"0x1234", unix.Mkdev(0, 81), true}, // an unknown filesystem on an anonymous device
		{"nfs", unix.Mkdev(0, 52), true},
		{"cifs", unix.Mkdev(0, 53), true},
		{"smb2", unix.Mkdev(0, 54), true},
		{"btrfs", unix.Mkdev(0, 40), true},
		{"zfs", unix.Mkdev(0, 41), true},
		{"overlay", unix.Mkdev(0, 42), true},
		// By name even with a real major: fuseblk (ntfs-3g) reports its block device, btrfs a
		// per-subvolume number.
		{"fuse", unix.Mkdev(8, 33), true},
		{"btrfs", unix.Mkdev(8, 2), true},
		{"zfs", unix.Mkdev(230, 1), true},
	} {
		if got := anonymousDev(tc.fsType, tc.dev); got != tc.want {
			t.Errorf("anonymousDev(%s, %d:%d) = %v, want %v", tc.fsType, unix.Major(tc.dev), unix.Minor(tc.dev), got, tc.want)
		}
	}
}
