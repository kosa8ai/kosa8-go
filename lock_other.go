//go:build !unix

package kosa8

// lockFile is a no-op where there is no flock; the in-process mutex still
// serialises updates from this program (as pkg/checkpoint lock_other.go).
func lockFile(string) (func(), error) { return func() {}, nil }
