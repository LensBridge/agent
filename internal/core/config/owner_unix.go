//go:build !windows

package config

import (
	"os"
	"syscall"
)

type owner struct{ uid, gid int }

// fileOwner returns the uid/gid of path, or false if it cannot be read.
func fileOwner(path string) (owner, bool) {
	fi, err := os.Stat(path)
	if err != nil {
		return owner{}, false
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return owner{}, false
	}
	return owner{uid: int(st.Uid), gid: int(st.Gid)}, true
}

// apply is best-effort: EPERM when not root and already the owner is harmless.
func (o owner) apply(path string) { _ = os.Chown(path, o.uid, o.gid) }
