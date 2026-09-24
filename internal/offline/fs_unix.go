//go:build !windows

package offline

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// syncDir fsyncs a directory, making renames and creations inside it durable.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("fsync %s: %w", dir, err)
	}
	return nil
}

// lockDir takes an exclusive, non-blocking flock on dir/.lock so two installs
// cannot interleave their staging, swap and prune. The kernel drops the lock
// when the process exits, so a crashed install never wedges the next one.
func lockDir(dir string) (unlock func(), err error) {
	f, err := os.OpenFile(filepath.Join(dir, ".lock"), os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("another install is already running")
		}
		return nil, fmt.Errorf("lock %s: %w", dir, err)
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}
