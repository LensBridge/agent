//go:build !windows

package usbimport

import (
	"fmt"
	"os"
	"syscall"
)

// openRegularNoFollow opens path without following a final symlink and
// without blocking on a FIFO, then insists on a regular file.
func openRegularNoFollow(path string) (*os.File, error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open: %w", err)
	}
	f := os.NewFile(uintptr(fd), path)
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		f.Close()
		return nil, fmt.Errorf("not a regular file")
	}
	return f, nil
}

// chownToDirOwner gives path the owner and group of dir. Best effort: the
// helper runs as root, and the inbox belongs to the daemon's user.
func chownToDirOwner(path, dir string) {
	fi, err := os.Stat(dir)
	if err != nil {
		return
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		_ = os.Chown(path, int(st.Uid), int(st.Gid))
	}
}
