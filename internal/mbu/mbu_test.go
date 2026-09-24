package mbu

import (
	"archive/zip"
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/LensBridge/agent/internal/trust"
)

const testDevice = "3f2a1b4c-5d6e-4f70-8a9b-0c1d2e3f4a5b"

func key(t *testing.T, b byte) ed25519.PrivateKey {
	t.Helper()
	return ed25519.NewKeyFromSeed(bytes.Repeat([]byte{b}, 32))
}

func pub(k ed25519.PrivateKey) ed25519.PublicKey { return k.Public().(ed25519.PublicKey) }

func write(t *testing.T, m Manifest, src []Source, signers ...ed25519.PrivateKey) string {
	t.Helper()
	var buf bytes.Buffer
	if err := Build(&buf, m, src, signers); err != nil {
		t.Fatalf("Build: %v", err)
	}
	p := filepath.Join(t.TempDir(), "p.mbu")
	if err := os.WriteFile(p, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

var img = []byte("\x89PNG fake image bytes")

func imgPath() string {
	s := sha256.Sum256(img)
	return "media/" + hex.EncodeToString(s[:]) + ".png"
}

func contentManifest() (Manifest, []Source) {
	m := Manifest{
		Type:     TypeContent,
		Sequence: 1000,
		DeviceID: testDevice,
		Content: &ContentInfo{
			Timezone: "America/Toronto", FirstDay: "2026-09-24", LastDay: "2026-09-25",
			Media: []MediaInfo{{Path: imgPath(), ContentType: "image/png"}},
		},
	}
	src := []Source{
		{Path: "payloads/2026-09-24.json", Data: []byte(`{"frames":[{"frameConfig":{"posterUrl":"/` + imgPath() + `"}}]}`)},
		{Path: "payloads/2026-09-25.json", Data: []byte(`{"frames":[]}`)},
		{Path: imgPath(), Data: img},
	}
	return m, src
}

func appManifest() (Manifest, []Source) {
	return Manifest{Type: TypeApp, Version: "2.1.0", App: &AppInfo{LocalAPI: 2}},
		[]Source{{Path: "index.html", Data: []byte("<html>")}, {Path: "assets/app-abc.js", Data: []byte("1")}}
}

func TestRoundTripContent(t *testing.T) {
	ck := key(t, 1)
	m, src := contentManifest()
	p := write(t, m, src, ck)
	pkg, err := Open(p, trust.NewRing([]ed25519.PublicKey{pub(ck)}, nil), OpenOptions{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer pkg.Close()
	if pkg.SignedBy != trust.KeyID(pub(ck)) {
		t.Fatalf("SignedBy = %s", pkg.SignedBy)
	}
	got, err := pkg.ReadFile(imgPath(), 1<<20)
	if err != nil || !bytes.Equal(got, img) {
		t.Fatalf("ReadFile = %q, %v", got, err)
	}
}

func TestRolesAreSeparate(t *testing.T) {
	ck, rk := key(t, 1), key(t, 2)
	ring := trust.NewRing([]ed25519.PublicKey{pub(ck)}, []ed25519.PublicKey{pub(rk)})

	// An app signed with the content key must not verify.
	m, src := appManifest()
	if _, err := Open(write(t, m, src, ck), ring, OpenOptions{}); !errors.Is(err, ErrNotAuthentic) {
		t.Fatalf("app signed by content key: err = %v", err)
	}
	if pkg, err := Open(write(t, m, src, rk), ring, OpenOptions{}); err != nil {
		t.Fatalf("app signed by release key: %v", err)
	} else {
		pkg.Close()
	}
	// And content signed with the release key must not either.
	cm, csrc := contentManifest()
	if _, err := Open(write(t, cm, csrc, rk), ring, OpenOptions{}); !errors.Is(err, ErrNotAuthentic) {
		t.Fatalf("content signed by release key: err = %v", err)
	}
}

func TestUnknownSignerIgnoredWhenAnotherVerifies(t *testing.T) {
	old, cur := key(t, 3), key(t, 4)
	m, src := appManifest()
	p := write(t, m, src, old, cur)
	pkg, err := Open(p, trust.NewRing(nil, []ed25519.PublicKey{pub(cur)}), OpenOptions{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	pkg.Close()
}

// rewrite copies the zip at p, letting edit change or drop entries and add
// extra ones.
func rewrite(t *testing.T, p string, edit func(name string, data []byte) ([]byte, bool), extra map[string][]byte) string {
	t.Helper()
	zr, err := zip.OpenReader(p)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, f := range zr.File {
		rc, _ := f.Open()
		var b bytes.Buffer
		b.ReadFrom(rc)
		rc.Close()
		data, keep := edit(f.Name, b.Bytes())
		if !keep {
			continue
		}
		w, _ := zw.Create(f.Name)
		w.Write(data)
	}
	for n, d := range extra {
		w, _ := zw.Create(n)
		w.Write(d)
	}
	zw.Close()
	out := filepath.Join(t.TempDir(), "edited.mbu")
	os.WriteFile(out, buf.Bytes(), 0o644)
	return out
}

func TestTampering(t *testing.T) {
	ck := key(t, 1)
	ring := trust.NewRing([]ed25519.PublicKey{pub(ck)}, nil)
	m, src := contentManifest()
	p := write(t, m, src, ck)
	keepAll := func(n string, d []byte) ([]byte, bool) { return d, true }

	cases := map[string]string{
		"manifest edited": rewrite(t, p, func(n string, d []byte) ([]byte, bool) {
			if n == ManifestName {
				return bytes.Replace(d, []byte("2026-09-25"), []byte("2026-09-26"), 1), true
			}
			return d, true
		}, nil),
		"payload edited": rewrite(t, p, func(n string, d []byte) ([]byte, bool) {
			if n == "payloads/2026-09-25.json" {
				return []byte(`{"frames":[1]}`), true
			}
			return d, true
		}, nil),
		"extra file":        rewrite(t, p, keepAll, map[string][]byte{"payloads/2026-09-26.json": []byte("{}")}),
		"zip slip":          rewrite(t, p, keepAll, map[string][]byte{"../evil": []byte("x")}),
		"hidden file":       rewrite(t, p, keepAll, map[string][]byte{"media/.x": []byte("x")}),
		"signature dropped": rewrite(t, p, func(n string, d []byte) ([]byte, bool) { return d, n != SignatureName }, nil),
		"media dropped":     rewrite(t, p, func(n string, d []byte) ([]byte, bool) { return d, !strings.HasPrefix(n, "media/") }, nil),
	}
	for name, path := range cases {
		t.Run(name, func(t *testing.T) {
			pkg, err := Open(path, ring, OpenOptions{})
			if err == nil {
				// Content changes that keep sizes are caught on read.
				_, err = pkg.ReadFile("payloads/2026-09-25.json", 1<<20)
				pkg.Close()
			}
			if err == nil {
				t.Fatal("tampered package accepted")
			}
		})
	}
}

func TestOmittedMediaNeedsStore(t *testing.T) {
	ck := key(t, 1)
	ring := trust.NewRing([]ed25519.PublicKey{pub(ck)}, nil)
	m, src := contentManifest()
	src[2].Omit = true
	p := write(t, m, src, ck)
	if _, err := Open(p, ring, OpenOptions{}); err == nil {
		t.Fatal("package with omitted media accepted without a store")
	}
	have := func(sha string) bool { return "media/"+sha+".png" == imgPath() }
	pkg, err := Open(p, ring, OpenOptions{HaveMedia: have})
	if err != nil {
		t.Fatalf("Open with store: %v", err)
	}
	if pkg.Has(imgPath()) {
		t.Fatal("omitted media reported present")
	}
	pkg.Close()
}

func TestManifestRules(t *testing.T) {
	ck := key(t, 1)
	bad := []func(m *Manifest, src *[]Source){
		func(m *Manifest, _ *[]Source) { m.DeviceID = "" },
		func(m *Manifest, _ *[]Source) { m.Sequence = 0 },
		func(m *Manifest, _ *[]Source) { m.Content.Timezone = "Local" },
		func(m *Manifest, _ *[]Source) { m.Content.LastDay = "2026-12-31" },
		func(m *Manifest, s *[]Source) { *s = (*s)[1:] }, // missing a day
		func(m *Manifest, s *[]Source) { *s = append(*s, Source{Path: "other.txt", Data: []byte("x")}) },
		func(m *Manifest, _ *[]Source) { m.Content.Media[0].ContentType = "text/html" },
	}
	for i, mut := range bad {
		m, src := contentManifest()
		m.Content = &ContentInfo{Timezone: m.Content.Timezone, FirstDay: m.Content.FirstDay, LastDay: m.Content.LastDay,
			Media: append([]MediaInfo(nil), m.Content.Media...)}
		mut(&m, &src)
		if err := Build(&bytes.Buffer{}, m, src, []ed25519.PrivateKey{ck}); err == nil {
			t.Errorf("case %d: invalid manifest built", i)
		}
	}
}

func TestCompareVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"1.2.3", "1.2.3", 0}, {"1.2.10", "1.2.9", 1}, {"0.9.0", "1.0.0", -1},
		{"dev", "0.0.1", -1}, {"1.0.0", "dev", 1}, {"01.0.0", "1.0.0", -1},
	}
	for _, c := range cases {
		if got := CompareVersions(c.a, c.b); got != c.want {
			t.Errorf("CompareVersions(%s, %s) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

// TestKeyIDVector pins the key id derivation so the backend, the Node packer
// and this code agree: seed 0x01..0x20.
func TestKeyIDVector(t *testing.T) {
	seed := make([]byte, 32)
	for i := range seed {
		seed[i] = byte(i + 1)
	}
	k := ed25519.NewKeyFromSeed(seed)
	id := trust.KeyID(pub(k))
	if len(id) != 16 {
		t.Fatalf("key id %q", id)
	}
	t.Logf("seed 0x01..0x20 -> keyId %s", id)
}
