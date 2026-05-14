// Package keystore manages the per-device Ed25519 private key.
//
// The key lives at /etc/musallahboard/agent.key as a raw 32-byte seed, mode
// 0600, root-owned. This is symmetric with the backend, which stores the raw
// 32-byte public key in device.public_key. The backend never sees the private
// key — it's generated locally on the Pi at first enroll.
package keystore

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// SeedSize is the size in bytes of the Ed25519 private-key seed.
const SeedSize = ed25519.SeedSize // 32

// Generate creates a fresh Ed25519 keypair using crypto/rand.
func Generate() (ed25519.PublicKey, ed25519.PrivateKey, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("ed25519 keygen: %w", err)
	}
	return pub, priv, nil
}

// Save writes the seed (first 32 bytes of priv) atomically to path with mode 0600.
// Atomicity comes from a tmp-file + fsync + rename, so a power cut mid-write
// either leaves the old key intact or completes the new one.
func Save(path string, priv ed25519.PrivateKey) error {
	if len(priv) != ed25519.PrivateKeySize {
		return fmt.Errorf("invalid private key size: %d (want %d)", len(priv), ed25519.PrivateKeySize)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("mkdir: %w", err)
	}

	seed := priv.Seed()
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("create %s: %w", tmp, err)
	}
	if _, err := f.Write(seed); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("write: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("fsync: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("close: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("rename: %w", err)
	}
	return nil
}

// Load reads a 32-byte seed from path and returns the derived Ed25519 private key.
// Returns an error if the file is missing, the wrong size, or unreadable.
func Load(path string) (ed25519.PrivateKey, error) {
	seed, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	if len(seed) != SeedSize {
		return nil, fmt.Errorf("key file %s has %d bytes, want %d", path, len(seed), SeedSize)
	}
	return ed25519.NewKeyFromSeed(seed), nil
}

// PublicKeyOf returns the 32-byte raw public key for a private key.
func PublicKeyOf(priv ed25519.PrivateKey) (ed25519.PublicKey, error) {
	if len(priv) != ed25519.PrivateKeySize {
		return nil, errors.New("invalid private key")
	}
	return priv.Public().(ed25519.PublicKey), nil
}
