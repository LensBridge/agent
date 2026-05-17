//go:build windows

package enroll

// chownToDirOwner is a no-op on Windows; the daemon does not target Windows
// hosts, and Unix uid/gid semantics don't apply.
func chownToDirOwner(_, _ string) error { return nil }
