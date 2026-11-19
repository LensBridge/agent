//go:build !linux

package telemetry

// Stub for non-Linux dev environments (Windows/macOS) so the package
// compiles. Pi builds use disk_linux.go via build tag.

type statfsResult struct {
	Blocks uint64
	Bavail uint64
}

func statfs(path string, s *statfsResult) error {
	return nil
}
