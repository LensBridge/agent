//go:build windows

package config

// Ownership is a no-op on Windows; the daemon only targets Linux. These stubs
// exist so the package builds for dev tooling on Windows.
type owner struct{}

func fileOwner(string) (owner, bool) { return owner{}, false }

func (owner) apply(string) {}
