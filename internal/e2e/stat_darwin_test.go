//go:build e2e

package e2e

import "syscall"

// ctimeNs returns the inode change time of st in nanoseconds.
func ctimeNs(st *syscall.Stat_t) int64 { return st.Ctimespec.Nano() }
