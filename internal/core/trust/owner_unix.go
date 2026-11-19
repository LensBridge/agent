//go:build !windows

package trust

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"syscall"
)

// checkRootOwned refuses a trust store the daemon could have planted.
// /etc/musallahboard is writable by the daemon (it writes kiosk-url), so it
// could unlink trust.json and put its own file there; that file would be
// owned by the daemon, not root. Hard links to someone else's file are
// caught by the link count. Run unprivileged (tests, a dev box) there is no
// root to protect, so ownership is not checked.
func checkRootOwned(path string) error {
	fi, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	switch {
	case !fi.Mode().IsRegular():
		return fmt.Errorf("%s is not a regular file", path)
	case !ok:
		return nil
	case st.Uid != 0 && os.Geteuid() == 0:
		return fmt.Errorf("%s is owned by uid %d, not root; refusing to trust it (restore it with `sudo musallahboard-agent trust fetch` after deleting it)", path, st.Uid)
	case st.Nlink != 1:
		return fmt.Errorf("%s has %d hard links; refusing to trust it", path, st.Nlink)
	case fi.Mode().Perm()&0o022 != 0:
		return fmt.Errorf("%s is writable by others than root; refusing to trust it", path)
	}
	return nil
}
