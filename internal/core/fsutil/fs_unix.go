//go:build !windows

package fsutil

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// SyncDir fsyncs a directory, making renames and creations inside it durable.
func SyncDir(dir string) error {
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

// LockDir takes an exclusive, non-blocking flock on dir/.lock so two installs
// cannot interleave their staging, swap and prune. The kernel drops the lock
// when the process exits, so a crashed install never wedges the next one.
func LockDir(dir string) (unlock func(), err error) {
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

// ChownToDirOwner gives path the owner and group of dir. Root tools (the USB
// helper, `enroll`, `service-port`) write files the unprivileged daemon must
// read, into directories the daemon owns. Best effort: not root, it fails
// harmlessly, and then the caller owns the file already.
func ChownToDirOwner(path, dir string) {
	fi, err := os.Stat(dir)
	if err != nil {
		return
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		_ = os.Chown(path, int(st.Uid), int(st.Gid))
	}
}
