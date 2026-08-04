//go:build !windows

package enroll

import (
	"os"
	"syscall"
)

// chownToDirOwner sets the uid/gid of `path` to match the uid/gid of its
// parent directory.
//
// Rationale: enrollment is typically run via `sudo musallahboard-agent enroll`,
// which writes files as root:root. But the daemon runs as the service user
// (`musallahdaemon`), and would fail to read its own key. The install scripts
// pre-create /etc/musallahboard owned by the service user, so chowning new
// files to match the directory's owner gives the daemon read access without
// the caller needing to know which user that is.
//
// Best-effort: returns nil if `path` doesn't exist, if we can't stat the dir,
// or if we lack CAP_CHOWN (e.g. enroll was run as a non-root user that
// already owns the dir — in which case the file is already owned correctly).
func chownToDirOwner(path, dir string) error {
	di, err := os.Stat(dir)
	if err != nil {
		return nil
	}
	st, ok := di.Sys().(*syscall.Stat_t)
	if !ok {
		return nil
	}
	// If we're not root, chown will fail with EPERM unless we already own
	// the file — in which case it's a no-op anyway. Swallow the error.
	_ = os.Chown(path, int(st.Uid), int(st.Gid))
	return nil
}
