//go:build !windows

package fsutil

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func ownerOf(t *testing.T, path string) int {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return int(fi.Sys().(*syscall.Stat_t).Uid)
}

// Root replacing a file the daemon owns must leave it the daemon's; a new
// file root writes is root's; WriteAtomicOwnedByDir follows the directory.
// Needs root to chown; skipped otherwise.
func TestWriteAtomicOwners(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root")
	}
	const daemon = 4242
	dir := t.TempDir()
	os.Chown(dir, daemon, daemon)

	kept := filepath.Join(dir, "kept.json")
	os.WriteFile(kept, []byte("old"), 0o640)
	os.Chown(kept, daemon, daemon)
	if err := WriteAtomic(kept, []byte("new"), 0o640); err != nil {
		t.Fatal(err)
	}
	if got := ownerOf(t, kept); got != daemon {
		t.Errorf("replaced file owner = %d, want %d", got, daemon)
	}

	fresh := filepath.Join(dir, "trust.json")
	if err := WriteAtomic(fresh, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := ownerOf(t, fresh); got != 0 {
		t.Errorf("new file owner = %d, want root", got)
	}

	owned := filepath.Join(dir, "state.json")
	if err := WriteAtomicOwnedByDir(owned, []byte("{}"), 0o640); err != nil {
		t.Fatal(err)
	}
	if got := ownerOf(t, owned); got != daemon {
		t.Errorf("dir-owned new file owner = %d, want %d", got, daemon)
	}
}
