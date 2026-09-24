package mbu

import (
	"archive/zip"
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/LensBridge/agent/internal/trust"
)

// ErrNotAuthentic means no signature on the package verified under a key
// trusted for its type.
var ErrNotAuthentic = errors.New("the package is not signed by a key this board trusts")

// KeyRing supplies the keys a package may be signed with, by role.
type KeyRing interface {
	Keys(role trust.Role) map[string]ed25519.PublicKey
}

// RoleFor is the key role that may sign packages of type t.
func RoleFor(t Type) trust.Role {
	if t == TypeContent {
		return trust.RoleContent
	}
	return trust.RoleRelease
}

// OpenOptions adjusts verification.
type OpenOptions struct {
	// HaveMedia reports whether the board's media store already holds the
	// media file with this sha256. A content package may omit such files
	// (online delta sync); any other listed file must be in the zip.
	HaveMedia func(sha256 string) bool
}

// Package is an opened, authenticated package. Its files are not yet read;
// ReadFile and CopyFile check each one's hash as they read it.
type Package struct {
	Manifest    *Manifest
	RawManifest []byte
	RawSig      []byte
	// SignedBy is the id of the key whose signature verified.
	SignedBy string

	zr      *zip.ReadCloser
	entries map[string]*zip.File
}

// Close releases the zip.
func (p *Package) Close() error { return p.zr.Close() }

// Has reports whether the zip carries the listed file at path (it may be
// omitted media).
func (p *Package) Has(path string) bool { return p.entries[path] != nil }

// Open opens the package at path, checks its structure, verifies its
// signature against ring, and checks that its entries are exactly the files
// its manifest lists. Only then is a Package returned. Board-specific checks
// (device id, monotonicity, arch) are the importer's.
func Open(path string, ring KeyRing, opt OpenOptions) (*Package, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", filepath.Base(path))
	}
	if fi.Size() > MaxPackageBytes {
		return nil, fmt.Errorf("%s is %d MiB; packages over %d MiB are refused",
			filepath.Base(path), fi.Size()>>20, MaxPackageBytes>>20)
	}
	zr, err := zip.OpenReader(path)
	if err != nil {
		return nil, fmt.Errorf("%s is not a readable package (zip): %w", filepath.Base(path), err)
	}
	p, err := open(zr, ring, opt)
	if err != nil {
		zr.Close()
		return nil, err
	}
	return p, nil
}

func open(zr *zip.ReadCloser, ring KeyRing, opt OpenOptions) (*Package, error) {
	entries, err := classify(zr.File)
	if err != nil {
		return nil, err
	}
	mf, sf := entries[ManifestName], entries[SignatureName]
	if mf == nil {
		return nil, fmt.Errorf("package has no %s (is this a MusallahBoard update package?)", ManifestName)
	}
	if sf == nil {
		return nil, fmt.Errorf("package has no %s: it is unsigned", SignatureName)
	}
	rawM, err := readAll(mf, MaxManifestBytes)
	if err != nil {
		return nil, err
	}
	rawS, err := readAll(sf, MaxSignatureBytes)
	if err != nil {
		return nil, err
	}

	// The type decides which keys may sign, so it is read before the
	// signature is checked. That is safe: the type is inside the signed
	// bytes, so a content key cannot vouch for a manifest claiming "app".
	var head struct {
		Type Type `json:"type"`
	}
	if err := json.Unmarshal(rawM, &head); err != nil {
		return nil, fmt.Errorf("%s is not valid JSON: %w", ManifestName, err)
	}
	signedBy, err := VerifySignature(rawM, rawS, ring.Keys(RoleFor(head.Type)))
	if err != nil {
		return nil, err
	}
	m, err := ParseManifest(rawM)
	if err != nil {
		return nil, err
	}

	delete(entries, ManifestName)
	delete(entries, SignatureName)
	for _, name := range sortedKeys(entries) {
		f, listed := m.File(name)
		if !listed {
			return nil, fmt.Errorf("package contains %s, which its manifest does not list", name)
		}
		if int64(entries[name].UncompressedSize64) != f.Bytes {
			return nil, fmt.Errorf("%s is %d bytes in the zip but %d in the manifest", name, entries[name].UncompressedSize64, f.Bytes)
		}
	}
	for _, f := range m.Files {
		if entries[f.Path] != nil {
			continue
		}
		omittable := m.Type == TypeContent && strings.HasPrefix(f.Path, "media/") &&
			opt.HaveMedia != nil && opt.HaveMedia(f.SHA256)
		if !omittable {
			return nil, fmt.Errorf("package is missing %s, which its manifest lists", f.Path)
		}
	}
	return &Package{
		Manifest:    m,
		RawManifest: rawM,
		RawSig:      rawS,
		SignedBy:    signedBy,
		zr:          zr,
		entries:     entries,
	}, nil
}

// classify indexes the zip's entries by name, rejecting anything that is not
// a plain file with an acceptable name. Directory entries are skipped.
func classify(files []*zip.File) (map[string]*zip.File, error) {
	out := make(map[string]*zip.File, len(files))
	if len(files) > MaxFiles+2 {
		return nil, fmt.Errorf("package has %d entries; at most %d are allowed", len(files), MaxFiles+2)
	}
	for _, f := range files {
		name := f.Name
		if strings.HasSuffix(name, "/") && f.UncompressedSize64 == 0 {
			if err := ValidPath(strings.TrimSuffix(name, "/")); err != nil {
				return nil, fmt.Errorf("package entry: %w", err)
			}
			continue
		}
		if err := ValidPath(name); err != nil {
			return nil, fmt.Errorf("package entry: %w", err)
		}
		if !f.Mode().IsRegular() {
			return nil, fmt.Errorf("package entry %q is not a regular file", name)
		}
		if out[name] != nil {
			return nil, fmt.Errorf("package contains %s twice", name)
		}
		out[name] = f
	}
	return out, nil
}

// sigFile is mbu.sig.
type sigFile struct {
	Signatures []struct {
		KeyID string `json:"keyId"`
		Sig   string `json:"sig"`
	} `json:"signatures"`
}

// VerifySignature checks rawSig (mbu.sig) over rawManifest against keys, and
// returns the id of the first key whose signature verifies. Signatures by
// keys not in keys are skipped: a package may be signed by an old and a new
// key during a rotation.
func VerifySignature(rawManifest, rawSig []byte, keys map[string]ed25519.PublicKey) (string, error) {
	var s sigFile
	if err := json.Unmarshal(rawSig, &s); err != nil {
		return "", fmt.Errorf("%s is not valid JSON: %w", SignatureName, err)
	}
	if len(s.Signatures) == 0 {
		return "", fmt.Errorf("%s holds no signatures", SignatureName)
	}
	if len(keys) == 0 {
		return "", fmt.Errorf("%w (no keys of the required kind are installed; see `musallahboard-agent trust show`)", ErrNotAuthentic)
	}
	msg := SignedMessage(rawManifest)
	var ids []string
	for _, sig := range s.Signatures {
		ids = append(ids, sig.KeyID)
		pub, ok := keys[sig.KeyID]
		if !ok {
			continue
		}
		raw, err := base64.StdEncoding.DecodeString(sig.Sig)
		if err != nil || len(raw) != ed25519.SignatureSize {
			continue
		}
		if ed25519.Verify(pub, msg, raw) {
			return sig.KeyID, nil
		}
	}
	return "", fmt.Errorf("%w (signed by %s)", ErrNotAuthentic, strings.Join(ids, ", "))
}

// SignedMessage is the byte sequence a signature covers.
func SignedMessage(rawManifest []byte) []byte {
	msg := make([]byte, 0, len(SignaturePrefix)+len(rawManifest))
	msg = append(msg, SignaturePrefix...)
	return append(msg, rawManifest...)
}

// ReadFile reads the listed file at path in full, verifying its size and
// hash. It refuses files larger than limit.
func (p *Package) ReadFile(path string, limit int64) ([]byte, error) {
	f, ok := p.Manifest.File(path)
	if !ok {
		return nil, fmt.Errorf("%s is not listed in the package", path)
	}
	if f.Bytes > limit {
		return nil, fmt.Errorf("%s is larger than %d MiB", path, limit>>20)
	}
	var buf bytes.Buffer
	if err := p.CopyFile(path, &buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// CopyFile streams the listed file at path to w, hashing as it goes. On error
// w has received unverified bytes and the caller must discard them; callers
// only ever write into staging.
func (p *Package) CopyFile(path string, w io.Writer) error {
	f, ok := p.Manifest.File(path)
	if !ok {
		return fmt.Errorf("%s is not listed in the package", path)
	}
	ze := p.entries[path]
	if ze == nil {
		return fmt.Errorf("%s is not in the package", path)
	}
	rc, err := ze.Open()
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	defer rc.Close()
	h := sha256.New()
	// One byte past the declared size tells "too big" apart; reading to EOF
	// is also what makes archive/zip check the CRC.
	n, err := io.Copy(io.MultiWriter(w, h), io.LimitReader(rc, f.Bytes+1))
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	if n != f.Bytes {
		return fmt.Errorf("%s is %d bytes but the manifest says %d", path, n, f.Bytes)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != f.SHA256 {
		return fmt.Errorf("%s is corrupt: its SHA-256 does not match the signed manifest", path)
	}
	return nil
}

func readAll(f *zip.File, limit int64) ([]byte, error) {
	rc, err := f.Open()
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", f.Name, err)
	}
	defer rc.Close()
	raw, err := io.ReadAll(io.LimitReader(rc, limit+1))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", f.Name, err)
	}
	if int64(len(raw)) > limit {
		return nil, fmt.Errorf("%s is larger than %d KiB", f.Name, limit>>10)
	}
	return raw, nil
}

// Peek reads the manifest of the package at path without verifying anything.
// It is for ordering a batch and for the screen, never for a decision about
// what to install.
func Peek(path string) (*Manifest, error) {
	zr, err := zip.OpenReader(path)
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	for _, f := range zr.File {
		if f.Name == ManifestName {
			raw, err := readAll(f, MaxManifestBytes)
			if err != nil {
				return nil, err
			}
			var m Manifest
			if err := json.Unmarshal(raw, &m); err != nil {
				return nil, err
			}
			return &m, nil
		}
	}
	return nil, fmt.Errorf("no %s", ManifestName)
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
