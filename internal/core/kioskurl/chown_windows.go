//go:build windows

package kioskurl

// chownToDirOwner is a no-op on Windows; the daemon only targets Linux Pis.
// This stub exists so the package builds for dev tooling on Windows.
func chownToDirOwner(_, _ string) error { return nil }
