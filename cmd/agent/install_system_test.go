package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/LensBridge/agent/packaging"
)

func TestInstallSystemIsIdempotent(t *testing.T) {
	root := t.TempDir()
	old := filepath.Join(root, "etc/musallahboard/kiosk-url")
	if err := os.MkdirAll(filepath.Dir(old), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(old, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	checked := 0
	check := func(string) error { checked++; return nil }

	changed, err := installSystem(root, packaging.System, []string{"/etc/musallahboard/kiosk-url"}, check)
	if err != nil {
		t.Fatal(err)
	}
	if len(changed) != len(packaging.System)+1 || checked != 1 {
		t.Fatalf("first run changed %d (want %d), sudoers checked %d times", len(changed), len(packaging.System)+1, checked)
	}
	for _, f := range packaging.System {
		fi, err := os.Stat(filepath.Join(root, f.Path))
		if err != nil || fi.Mode().Perm() != f.Mode {
			t.Errorf("%s: %v, mode %v want %v", f.Path, err, fi.Mode().Perm(), f.Mode)
		}
	}
	if _, err := os.Stat(old); !errors.Is(err, os.ErrNotExist) {
		t.Error("obsolete file kept")
	}

	changed, err = installSystem(root, packaging.System, []string{"/etc/musallahboard/kiosk-url"}, check)
	if err != nil || len(changed) != 0 {
		t.Fatalf("second run changed %v, %v", changed, err)
	}
}

// A sudoers file that fails validation is never put in place.
func TestInstallSystemRefusesBadSudoers(t *testing.T) {
	root := t.TempDir()
	_, err := installSystem(root, packaging.System, nil, func(string) error { return errors.New("syntax error") })
	if err == nil {
		t.Fatal("no error")
	}
	if _, err := os.Stat(filepath.Join(root, "etc/sudoers.d/musallahboard-agent")); err == nil {
		t.Fatal("unvalidated sudoers installed")
	}
	if m, _ := filepath.Glob(filepath.Join(root, "etc/sudoers.d/*")); len(m) != 0 {
		t.Fatalf("left behind: %v", m)
	}
}
