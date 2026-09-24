package trust

import (
	"crypto/ed25519"
	"os"
	"path/filepath"
	"testing"
)

func TestStoreRoundTripAndRootOwnedChecks(t *testing.T) {
	p := filepath.Join(t.TempDir(), "trust.json")
	s, err := LoadRootOwned(p)
	if err != nil || len(s.Content) != 0 {
		t.Fatalf("missing file: %v", err)
	}
	pub := ed25519.NewKeyFromSeed(make([]byte, 32)).Public().(ed25519.PublicKey)
	if !s.Add(RoleContent, pub, "t") || s.Add(RoleContent, pub, "t") {
		t.Fatal("Add dedup")
	}
	if err := s.Save(p); err != nil {
		t.Fatal(err)
	}
	r, err := LoadRingRootOwned(p)
	if err != nil || len(r.Keys(RoleContent)) != 1 {
		t.Fatalf("ring: %v", err)
	}
	// A second hard link is refused.
	if err := os.Link(p, p+".2"); err == nil {
		if _, err := LoadRootOwned(p); err == nil {
			t.Fatal("hard-linked store accepted")
		}
		os.Remove(p + ".2")
	}
	// Group-writable is refused.
	os.Chmod(p, 0o664)
	if _, err := LoadRootOwned(p); err == nil {
		t.Fatal("group-writable store accepted")
	}
}
