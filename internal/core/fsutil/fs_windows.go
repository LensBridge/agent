//go:build windows

package fsutil

// The agent only runs on Linux. These stubs let the package build and its
// tests run on a Windows dev box, where directories cannot be opened for
// fsync and flock does not exist.

func SyncDir(string) error { return nil }

func LockDir(string) (func(), error) { return func() {}, nil }
