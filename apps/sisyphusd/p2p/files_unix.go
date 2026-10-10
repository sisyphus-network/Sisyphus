//go:build unix

package p2p

import "syscall"

// openFiles is how many files, connections among them, the system lets
// this program have open at once.
func openFiles() int {
	var limit syscall.Rlimit
	syscall.Getrlimit(syscall.RLIMIT_NOFILE, &limit) // cannot fail for a limit every system has
	// A system that sets no limit says so with a number too large to mean.
	return int(min(limit.Cur, 1<<20))
}
