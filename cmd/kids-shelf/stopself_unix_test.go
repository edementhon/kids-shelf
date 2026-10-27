//go:build !windows

package main

import (
	"os"
	"syscall"
)

// stopSelf sends this process the stop signal the OS would send the
// launcher, so the in-process server stops through the same signal path a
// real launcher does.
func stopSelf() error {
	return syscall.Kill(os.Getpid(), syscall.SIGTERM)
}
