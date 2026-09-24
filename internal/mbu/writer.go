package mbu

import (
	"archive/zip"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/LensBridge/agent/internal/trust"
)

// Source is one file to put in a package.
type Source struct {
	// Path is the file's path inside the package.
	Path string
	// Exactly one of Data and FromFile is set.
	Data     []byte
	FromFile string
	// Omit lists the file in the manifest but leaves it out of the zip
	// (delta content packages). Its hash and size are still computed.
	Omit bool
}

func (s Source) open() (io.ReadCloser, error) {
	if s.FromFile != "" {
		return os.Open(s.FromFile)
	}
	return io.NopCloser(strings.NewReader(string(s.Data))), nil
}

// Build writes a signed package to w. m supplies everything but Files and
// Format/FormatVersion, which Build fills in from sources. createdAt is set to
// now (UTC, whole seconds) if m.CreatedAt is empty. The manifest is checked
// with ParseManifest before anything is written, so Build never produces a
// package the agent would refuse on structure.
func Build(w io.Writer, m Manifest, sources []Source, signers []ed25519.PrivateKey) error {
	if len(signers) == 0 {
		return fmt.Errorf("no signing key given")
	}
	m.Format, m.FormatVersion = Format, FormatVersion
	if m.CreatedAt == "" {
		m.CreatedAt = time.Now().UTC().Truncate(time.Second).Format(time.RFC3339)
	}
	sort.Slice(sources, func(i, j int) bool { return sources[i].Path < sources[j].Path })
	m.Files = make([]File, 0, len(sources))
	for _, s := range sources {
		f, err := hashSource(s)
		if err != nil {
			return err
		}
		m.Files = append(m.Files, f)
	}

	rawM, err := json.MarshalIndent(&m, "", "  ")
	if err != nil {
		return err
	}
	if _, err := ParseManifest(rawM); err != nil {
		return fmt.Errorf("refusing to build an invalid package: %w", err)
	}
	rawS, err := Sign(rawM, signers)
	if err != nil {
		return err
	}

	zw := zip.NewWriter(w)
	if err := writeEntry(zw, ManifestName, strings.NewReader(string(rawM)), true); err != nil {
		return err
	}
	if err := writeEntry(zw, SignatureName, strings.NewReader(string(rawS)), true); err != nil {
		return err
	}
	for _, s := range sources {
		if s.Omit {
			continue
		}
		rc, err := s.open()
		if err != nil {
			return err
		}
		// Media is already compressed; deflating it again only costs CPU on
		// the board.
		deflate := !strings.HasPrefix(s.Path, "media/")
		err = writeEntry(zw, s.Path, rc, deflate)
		rc.Close()
		if err != nil {
			return err
		}
	}
	return zw.Close()
}

// Sign produces mbu.sig for rawManifest.
func Sign(rawManifest []byte, signers []ed25519.PrivateKey) ([]byte, error) {
	type sig struct {
		KeyID string `json:"keyId"`
		Sig   string `json:"sig"`
	}
	var out struct {
		Signatures []sig `json:"signatures"`
	}
	msg := SignedMessage(rawManifest)
	for _, k := range signers {
		pub := k.Public().(ed25519.PublicKey)
		out.Signatures = append(out.Signatures, sig{
			KeyID: trust.KeyID(pub),
			Sig:   base64.StdEncoding.EncodeToString(ed25519.Sign(k, msg)),
		})
	}
	return json.MarshalIndent(out, "", "  ")
}

func hashSource(s Source) (File, error) {
	if err := ValidPath(s.Path); err != nil {
		return File{}, err
	}
	rc, err := s.open()
	if err != nil {
		return File{}, err
	}
	defer rc.Close()
	h := sha256.New()
	n, err := io.Copy(h, rc)
	if err != nil {
		return File{}, fmt.Errorf("read %s: %w", s.Path, err)
	}
	return File{Path: s.Path, SHA256: hex.EncodeToString(h.Sum(nil)), Bytes: n}, nil
}

func writeEntry(zw *zip.Writer, name string, r io.Reader, deflate bool) error {
	method := zip.Store
	if deflate {
		method = zip.Deflate
	}
	w, err := zw.CreateHeader(&zip.FileHeader{Name: name, Method: method, Modified: fixedModTime})
	if err != nil {
		return err
	}
	_, err = io.Copy(w, r)
	return err
}

// fixedModTime makes builds of the same inputs byte-identical. DOS time cannot
// go before 1980, so not the Unix epoch.
var fixedModTime = time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
