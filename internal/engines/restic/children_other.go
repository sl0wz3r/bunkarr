//go:build !linux

package restic

// resticChildren finds no children without /proc: GuardedUnlock applies §6.7's rule unchanged.
func resticChildren() []int { return nil }
