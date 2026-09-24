//go:build windows

package offline

// The agent only runs on Linux. These stubs let the package build and its
// tests run on a Windows dev box, where directories cannot be opened for
// fsync and flock does not exist.

func syncDir(string) error { return nil }

func lockDir(string) (func(), error) { return func() {}, nil }
