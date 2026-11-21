package keystore

import (
	"bytes"
	"crypto/ed25519"
	"os"
	"path/filepath"
	"testing"
)

func TestSaveLoad_RoundTrip(t *testing.T) {
	pub, priv, err := Generate()
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "agent.key")
	if err := Save(path, priv); err != nil {
		t.Fatalf("Save: %v", err)
	}

	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !bytes.Equal(priv.Seed(), loaded.Seed()) {
		t.Fatalf("seed mismatch after round-trip")
	}
	if !bytes.Equal(pub, loaded.Public().(ed25519.PublicKey)) {
		t.Fatalf("public key mismatch after round-trip")
	}
}

func TestSave_WritesMode0600(t *testing.T) {
	if os.Geteuid() == -1 {
		t.Skip("not POSIX")
	}
	_, priv, _ := Generate()
	path := filepath.Join(t.TempDir(), "agent.key")
	if err := Save(path, priv); err != nil {
		t.Fatalf("Save: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	// On Windows mode bits don't apply; only check on POSIX.
	if info.Mode().Perm() != 0o600 && os.Getenv("OS") != "Windows_NT" {
		t.Fatalf("mode = %o, want 600", info.Mode().Perm())
	}
}

func TestLoad_RejectsWrongSize(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.key")
	if err := os.WriteFile(path, []byte("too-short"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("expected error for short key, got nil")
	}
}
