package proc

import "golang.org/x/sys/unix"

// statFSType returns the filesystem type of path (statfs f_type).
func statFSType(path string) (int64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0, err
	}
	return int64(st.Type), nil
}
