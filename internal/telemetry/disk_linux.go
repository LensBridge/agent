//go:build linux

package telemetry

import "syscall"

type statfsResult struct {
	Blocks uint64
	Bavail uint64
}

func statfs(path string, s *statfsResult) error {
	var sys syscall.Statfs_t
	if err := syscall.Statfs(path, &sys); err != nil {
		return err
	}
	s.Blocks = sys.Blocks
	s.Bavail = sys.Bavail
	return nil
}
