//go:build unix

package kosa8

import (
	"os"
	"syscall"
)

// lockFile takes an exclusive flock on path, as pkg/checkpoint does, so the SDK
// and the CLI never interleave updates to the checkpoint store.
func lockFile(path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return func() { _ = f.Close() }, nil // closing drops the lock
}
