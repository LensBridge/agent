// Package trust holds the public keys a board accepts packages from
// (docs/architecture.md, section 5).
//
// There are two roles. Content keys belong to the LensBridge backend and may
// only sign content packages; they are pinned at enrollment. Release keys sign
// software (the board app and the agent itself) and never live on the backend;
// they are compiled into the agent and may be extended in trust.json. Keeping
// the roles apart is what limits a compromised backend to the wrong posters on
// a screen rather than code running on every board.
//
// The file is root-owned and the daemon only reads it. The root self-updater
// trusts nothing but this file and the keys compiled into its own binary.
package trust

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"

	"github.com/LensBridge/agent/internal/fsutil"
)

// DefaultPath is the trust store on a board.
const DefaultPath = "/etc/musallahboard/trust.json"

// BuiltinReleaseKeys is a comma-separated list of base64 release public keys,
// set at build time:
//
//	-ldflags "-X github.com/LensBridge/agent/internal/trust.BuiltinReleaseKeys=<b64>[,<b64>]"
//
// A build without it (a developer build) accepts only release keys listed in
// trust.json.
var BuiltinReleaseKeys = ""

// Role is which kind of package a key may sign.
type Role string

const (
	RoleContent Role = "content"
	RoleRelease Role = "release"
)

// ParseRole accepts "content" or "release".
func ParseRole(s string) (Role, error) {
	switch Role(s) {
	case RoleContent, RoleRelease:
		return Role(s), nil
	}
	return "", fmt.Errorf("unknown key role %q (want %q or %q)", s, RoleContent, RoleRelease)
}

// Key is one trusted public key as stored in trust.json.
type Key struct {
	KeyID     string `json:"keyId"`
	PublicKey string `json:"publicKey"`
	Note      string `json:"note,omitempty"`
}

// Store is trust.json.
type Store struct {
	Content []Key `json:"content"`
	Release []Key `json:"release"`
}

// KeyID is the lowercase hex of the first 8 bytes of SHA-256 of the raw
// public key. The backend, mbpack and the frontend packer derive it the same
// way.
func KeyID(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	return hex.EncodeToString(sum[:8])
}

// ParsePublicKey decodes a base64 raw 32-byte Ed25519 public key.
func ParsePublicKey(b64 string) (ed25519.PublicKey, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(b64))
	if err != nil {
		return nil, fmt.Errorf("public key is not base64: %w", err)
	}
	if len(raw) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("public key is %d bytes, want %d", len(raw), ed25519.PublicKeySize)
	}
	return ed25519.PublicKey(raw), nil
}

// ParsePrivateSeed decodes a base64 32-byte Ed25519 seed into a private key.
func ParsePrivateSeed(b64 string) (ed25519.PrivateKey, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(b64))
	if err != nil {
		return nil, fmt.Errorf("signing key is not base64: %w", err)
	}
	if len(raw) != ed25519.SeedSize {
		return nil, fmt.Errorf("signing key is %d bytes, want a %d-byte Ed25519 seed", len(raw), ed25519.SeedSize)
	}
	return ed25519.NewKeyFromSeed(raw), nil
}

// NewKey describes pub for storage.
func NewKey(pub ed25519.PublicKey, note string) Key {
	return Key{KeyID: KeyID(pub), PublicKey: base64.StdEncoding.EncodeToString(pub), Note: note}
}

// Load reads the store at path. A missing file is an empty store, not an
// error: a board enrolled before v2 has none until `trust fetch` runs.
func Load(path string) (*Store, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return &Store{}, nil
	}
	if err != nil {
		return nil, err
	}
	var s Store
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("%s is not valid JSON: %w", path, err)
	}
	for _, list := range [][]Key{s.Content, s.Release} {
		for _, k := range list {
			pub, err := ParsePublicKey(k.PublicKey)
			if err != nil {
				return nil, fmt.Errorf("%s: key %s: %w", path, k.KeyID, err)
			}
			if KeyID(pub) != k.KeyID {
				return nil, fmt.Errorf("%s: key id %s does not match its public key (%s)", path, k.KeyID, KeyID(pub))
			}
		}
	}
	return &s, nil
}

// Save writes the store atomically, world-readable (it holds public keys
// only) and owned by whoever runs this, which is root.
func (s *Store) Save(path string) error {
	raw, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return fsutil.WriteAtomic(path, append(raw, '\n'), 0o644)
}

// Add trusts pub for role. It reports false if it was already trusted.
func (s *Store) Add(role Role, pub ed25519.PublicKey, note string) bool {
	list := s.list(role)
	id := KeyID(pub)
	for _, k := range *list {
		if k.KeyID == id {
			return false
		}
	}
	*list = append(*list, NewKey(pub, note))
	return true
}

// Remove drops the key with keyID from either role, reporting whether it
// was there.
func (s *Store) Remove(keyID string) bool {
	found := false
	for _, role := range []Role{RoleContent, RoleRelease} {
		list := s.list(role)
		kept := (*list)[:0]
		for _, k := range *list {
			if k.KeyID == keyID {
				found = true
				continue
			}
			kept = append(kept, k)
		}
		*list = kept
	}
	return found
}

func (s *Store) list(role Role) *[]Key {
	if role == RoleContent {
		return &s.Content
	}
	return &s.Release
}

// Ring is the set of keys packages are verified against: the store plus the
// release keys compiled into this binary.
type Ring struct {
	keys map[Role]map[string]ed25519.PublicKey
}

// Ring builds the verification key ring. Builtin release keys that fail to
// parse are a build mistake, reported as an error rather than ignored.
func (s *Store) Ring() (*Ring, error) {
	r := &Ring{keys: map[Role]map[string]ed25519.PublicKey{
		RoleContent: {},
		RoleRelease: {},
	}}
	for _, role := range []Role{RoleContent, RoleRelease} {
		for _, k := range *s.list(role) {
			pub, err := ParsePublicKey(k.PublicKey)
			if err != nil {
				return nil, err
			}
			r.keys[role][KeyID(pub)] = pub
		}
	}
	builtin, err := Builtin()
	if err != nil {
		return nil, err
	}
	for _, pub := range builtin {
		r.keys[RoleRelease][KeyID(pub)] = pub
	}
	return r, nil
}

// Builtin returns the release keys compiled into this binary.
func Builtin() ([]ed25519.PublicKey, error) {
	var out []ed25519.PublicKey
	for _, b64 := range strings.Split(BuiltinReleaseKeys, ",") {
		if strings.TrimSpace(b64) == "" {
			continue
		}
		pub, err := ParsePublicKey(b64)
		if err != nil {
			return nil, fmt.Errorf("builtin release key: %w", err)
		}
		out = append(out, pub)
	}
	return out, nil
}

// NewRing builds a ring from explicit keys, for tools and tests.
func NewRing(content, release []ed25519.PublicKey) *Ring {
	r := &Ring{keys: map[Role]map[string]ed25519.PublicKey{RoleContent: {}, RoleRelease: {}}}
	for _, k := range content {
		r.keys[RoleContent][KeyID(k)] = k
	}
	for _, k := range release {
		r.keys[RoleRelease][KeyID(k)] = k
	}
	return r
}

// Keys returns the keys trusted for role, by key id.
func (r *Ring) Keys(role Role) map[string]ed25519.PublicKey {
	if r == nil {
		return nil
	}
	return r.keys[role]
}

// Empty reports whether no key is trusted for role.
func (r *Ring) Empty(role Role) bool { return len(r.Keys(role)) == 0 }

// LoadRing loads the store at path and builds its ring.
func LoadRing(path string) (*Ring, error) {
	s, err := Load(path)
	if err != nil {
		return nil, err
	}
	return s.Ring()
}
