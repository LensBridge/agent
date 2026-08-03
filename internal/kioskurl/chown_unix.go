//go:build !windows

package kioskurl

import (
	"os"
	"syscall"
)

// chownToDirOwner sets the uid/gid of `path` to match its parent directory's
// owner. Same rationale as internal/enroll.chownToDirOwner: enrollment may run
// as root via sudo, but the daemon runs as the service user; matching the
// pre-created config dir's owner keeps the file readable without the caller
// needing to know which user that is. Best-effort — EPERM when we already own
// the file is harmless.
func chownToDirOwner(path, dir string) error {
	di, err := os.Stat(dir)
	if err != nil {
		return nil
	}
	st, ok := di.Sys().(*syscall.Stat_t)
	if !ok {
		return nil
	}
	_ = os.Chown(path, int(st.Uid), int(st.Gid))
	return nil
}
