//go:build !unix

package p2p

// openFiles is how many files the system lets this program have open at
// once, where it says: here it does not, and zero has the caller take a
// usual number.
func openFiles() int { return 0 }
