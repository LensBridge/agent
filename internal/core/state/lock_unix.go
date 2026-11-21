//go:build !windows

package state

import (
	"os"
	"path/filepath"
	"syscall"

	"github.com/LensBridge/agent/internal/core/fsutil"
)

// lockBlocking takes an exclusive flock on path, waiting for it. The kernel
// releases it if the process dies.
func lockBlocking(path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o640)
	if err != nil {
		return nil, err
	}
	// Created by a root tool (the updater), the lock must still be the
	// daemon's to open; this also repairs one a root tool left behind.
	fsutil.ChownToDirOwner(path, filepath.Dir(path))
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); f.Close() }, nil
}
