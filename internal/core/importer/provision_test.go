package importer

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/LensBridge/agent/internal/core/config"
	"github.com/LensBridge/agent/internal/core/mbu"
	"github.com/LensBridge/agent/internal/core/store"
	"github.com/LensBridge/agent/internal/core/trust"
)

const otherDev = "00000000-0000-0000-0000-0000000000aa"

var secret = base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32))

// setupFile builds a provision package for device at sequence seq, signed
// by k. backend "" makes an offline-only one.
func setupFile(t *testing.T, device string, seq int64, name, backend string, k ed25519.PrivateKey) string {
	t.Helper()
	p := mbu.Provision{Format: 1, DeviceID: device, DisplayName: name, Sequence: seq, IssuedAt: "2026-11-19T12:00:00Z"}
	if backend != "" {
		p.BackendURL, p.ClaimSecret = backend, secret
	}
	raw, _ := json.Marshal(p)
	return build(t, mbu.Manifest{Type: mbu.TypeProvision, DeviceID: device, Sequence: seq, CreatedAt: "2026-11-19T12:00:00Z"},
		[]mbu.Source{{Path: mbu.ProvisionFile, Data: raw}}, k)
}

// newImporter is an importer over a temp store whose agent.toml starts as
// cfg, saved at a real path so the test sees what is written.
func newImporter(t *testing.T, cfg config.Config) (*Importer, *config.Live, store.Layout, string) {
	t.Helper()
	l := store.Layout{Root: t.TempDir()}
	path := filepath.Join(t.TempDir(), "agent.toml")
	if cfg != (config.Config{}) {
		if err := config.Save(path, &cfg); err != nil {
			t.Fatal(err)
		}
	}
	live, err := config.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ring := trust.NewRing([]ed25519.PublicKey{contentKey.Public().(ed25519.PublicKey)},
		[]ed25519.PublicKey{releaseKey.Public().(ed25519.PublicKey)})
	im := New(Deps{
		Layout: l, Config: live, AgentVersion: "0.2.0",
		Ring:       func() (*trust.Ring, error) { return ring, nil },
		Now:        func() time.Time { return time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC) },
		TrustedNow: func() (time.Time, bool) { return time.Time{}, false },
	})
	return im, live, l, path
}

func TestProvisionMatrix(t *testing.T) {
	provisioned := config.Config{DeviceID: dev, DisplayName: "Old name", ProvisionSequence: 100}
	unsigned := func(t *testing.T) string {
		// A valid package with its signature file emptied of signatures.
		p := setupFile(t, dev, 200, "N", "", contentKey)
		return withoutSignatures(t, p)
	}
	cases := []struct {
		name    string
		cfg     config.Config
		file    func(t *testing.T) string
		action  string
		wantDev string
		wantSeq int64
	}{
		{"unprovisioned takes any device", config.Config{},
			func(t *testing.T) string { return setupFile(t, otherDev, 5, "North", "", contentKey) },
			ActionInstalled, otherDev, 5},
		{"unprovisioned takes an older sequence too", config.Config{},
			func(t *testing.T) string { return setupFile(t, dev, 1, "North", "", contentKey) },
			ActionInstalled, dev, 1},
		{"own device, newer", provisioned,
			func(t *testing.T) string { return setupFile(t, dev, 101, "New name", "", contentKey) },
			ActionInstalled, dev, 101},
		{"own device, same", provisioned,
			func(t *testing.T) string { return setupFile(t, dev, 100, "New name", "", contentKey) },
			ActionUnchanged, dev, 100},
		{"own device, older", provisioned,
			func(t *testing.T) string { return setupFile(t, dev, 99, "New name", "", contentKey) },
			ActionOutdated, dev, 100},
		{"other device", provisioned,
			func(t *testing.T) string { return setupFile(t, otherDev, 999, "Theirs", "", contentKey) },
			ActionSkipped, dev, 100},
		{"wrong key (release key)", config.Config{},
			func(t *testing.T) string { return setupFile(t, dev, 5, "N", "", releaseKey) },
			ActionRejected, "", 0},
		{"wrong key, provisioned", provisioned,
			func(t *testing.T) string { return setupFile(t, dev, 101, "N", "", releaseKey) },
			ActionRejected, dev, 100},
		{"unsigned", config.Config{}, unsigned, ActionRejected, "", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			im, live, _, path := newImporter(t, tc.cfg)
			r := one(t, im.Import(context.Background(), SourceUSB, []string{tc.file(t)}))
			if r.Action != tc.action {
				t.Fatalf("action = %s (%s: %s), want %s", r.Action, r.Message, r.Detail, tc.action)
			}
			got := live.Get()
			if got.DeviceID != tc.wantDev || got.ProvisionSequence != tc.wantSeq {
				t.Fatalf("config = %s seq %d, want %s seq %d", got.DeviceID, got.ProvisionSequence, tc.wantDev, tc.wantSeq)
			}
			if saved, _ := config.Load(path); saved.DeviceID != tc.wantDev {
				t.Fatalf("saved device id = %q, want %q", saved.DeviceID, tc.wantDev)
			}
		})
	}
}

// withoutSignatures copies the package at path without its mbu.sig.
func withoutSignatures(t *testing.T, path string) string {
	t.Helper()
	zr, err := zip.OpenReader(path)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	out := filepath.Join(t.TempDir(), "unsigned.mbu")
	f, _ := os.Create(out)
	zw := zip.NewWriter(f)
	for _, e := range zr.File {
		if e.Name == mbu.SignatureName {
			continue
		}
		w, _ := zw.Create(e.Name)
		rc, _ := e.Open()
		io.Copy(w, rc)
		rc.Close()
	}
	zw.Close()
	f.Close()
	return out
}

// The setup stick for an unprovisioned board: its setup file goes first, so
// its content (and only its content) installs in the same batch.
func TestUnprovisionedBoardTakesSetupThenContent(t *testing.T) {
	im, live, l, _ := newImporter(t, config.Config{})
	poster := "/" + imgPath()
	b := im.Import(context.Background(), SourceUSB, []string{
		content(t, 300, dev, poster, false),
		content(t, 301, otherDev, poster, false),
		setupFile(t, dev, 5, "North Musallah", "", contentKey),
	})
	byType := map[string]string{}
	for _, r := range b.Results {
		byType[string(r.Type)+" "+r.Action] = r.Message
	}
	if _, ok := byType["provision installed"]; !ok {
		t.Fatalf("results = %+v", b.Results)
	}
	if _, ok := byType["content installed"]; !ok {
		t.Fatalf("own content not installed: %+v", b.Results)
	}
	if _, ok := byType["content skipped"]; !ok {
		t.Fatalf("other board's content not skipped: %+v", b.Results)
	}
	if c := live.Get(); c.DeviceID != dev || c.DisplayName != "North Musallah" {
		t.Fatalf("config = %+v", c)
	}
	if c, err := l.CurrentContent(); err != nil || c.Manifest.Sequence != 300 {
		t.Fatalf("current content = %+v, %v", c, err)
	}
	if b.Notice.Lines[0] != "Set up as North Musallah" {
		t.Fatalf("notice = %+v", b.Notice)
	}
}

// A USB kit for several boards set up nothing on an unprovisioned board:
// which of them it is cannot be guessed.
func TestUnprovisionedBoardRefusesSeveralSetupFiles(t *testing.T) {
	im, live, _, _ := newImporter(t, config.Config{})
	b := im.Import(context.Background(), SourceUSB, []string{
		setupFile(t, dev, 5, "A", "", contentKey),
		setupFile(t, otherDev, 5, "B", "", contentKey),
	})
	for _, r := range b.Results {
		if r.Action != ActionRejected {
			t.Fatalf("results = %+v", b.Results)
		}
	}
	if live.Get().Provisioned() {
		t.Fatal("provisioned from an ambiguous stick")
	}
}

// A provisioned board on a USB kit takes its own setup file and skips the
// others'.
func TestProvisionedBoardOnAKit(t *testing.T) {
	im, live, _, _ := newImporter(t, config.Config{DeviceID: dev, ProvisionSequence: 1})
	b := im.Import(context.Background(), SourceUSB, []string{
		setupFile(t, dev, 5, "Mine", "", contentKey),
		setupFile(t, otherDev, 5, "Theirs", "", contentKey),
	})
	if !b.OK() || live.Get().DisplayName != "Mine" {
		t.Fatalf("results = %+v, config %+v", b.Results, live.Get())
	}
}

func TestProvisionOnlineSettings(t *testing.T) {
	const be = "https://api.example.org"
	// First setup file with a backend: the board must claim.
	im, live, _, _ := newImporter(t, config.Config{})
	one(t, im.Import(context.Background(), SourceUSB, []string{setupFile(t, dev, 5, "N", be+"/", contentKey)}))
	c := live.Get()
	if c.BackendURL != be || c.ClaimSecret != secret || c.WebSocketURL != "" || !c.Online() {
		t.Fatalf("config = %+v", c)
	}

	// An enrolled board, same backend: its socket and key stay, no secret.
	im, live, _, _ = newImporter(t, config.Config{DeviceID: dev, BackendURL: be, WebSocketURL: "wss://api.example.org/api/agent/ws"})
	one(t, im.Import(context.Background(), SourceUSB, []string{setupFile(t, dev, 5, "N", be, contentKey)}))
	if c := live.Get(); c.WebSocketURL == "" || c.ClaimSecret != "" || c.DisplayName != "N" {
		t.Fatalf("config = %+v", c)
	}

	// A new backend: claim there.
	one(t, im.Import(context.Background(), SourceUSB, []string{setupFile(t, dev, 6, "N", "https://other.example.org", contentKey)}))
	if c := live.Get(); c.WebSocketURL != "" || c.ClaimSecret != secret || c.BackendURL != "https://other.example.org" {
		t.Fatalf("config = %+v", c)
	}

	// An offline setup file never takes the backend away.
	one(t, im.Import(context.Background(), SourceUSB, []string{setupFile(t, dev, 7, "Renamed", "", contentKey)}))
	if c := live.Get(); c.BackendURL != "https://other.example.org" || c.DisplayName != "Renamed" {
		t.Fatalf("config = %+v", c)
	}
}

// provision.json must agree with the signed manifest it travels under.
func TestProvisionFileMustMatchManifest(t *testing.T) {
	im, live, _, _ := newImporter(t, config.Config{})
	raw, _ := json.Marshal(mbu.Provision{Format: 1, DeviceID: otherDev, Sequence: 5})
	p := build(t, mbu.Manifest{Type: mbu.TypeProvision, DeviceID: dev, Sequence: 5},
		[]mbu.Source{{Path: mbu.ProvisionFile, Data: raw}}, contentKey)
	if r := one(t, im.Import(context.Background(), SourceUSB, []string{p})); r.Action != ActionRejected {
		t.Fatalf("result = %+v", r)
	}
	if live.Get().Provisioned() {
		t.Fatal("provisioned")
	}
}

// A setup file made by the backend (LensBridgeBackend's packager, its test
// content key: seed bytes 0x01..0x20) goes through the real verification and
// provisioning path. If this fails, the two sides disagree on the format.
func TestBackendSampleSetupFile(t *testing.T) {
	pub, _ := base64.StdEncoding.DecodeString("ebVWLo/mVPlAeLES6KmLp5AfhTrmlb7X4OORC60ElmQ=")
	if trust.KeyID(pub) != "65b60673d6ed884b" {
		t.Fatalf("key id = %s", trust.KeyID(pub))
	}
	path := filepath.Join(t.TempDir(), "agent.toml")
	live, _ := config.Open(path)
	ring := trust.NewRing([]ed25519.PublicKey{pub}, nil)
	im := New(Deps{Layout: store.Layout{Root: t.TempDir()}, Config: live,
		Ring:       func() (*trust.Ring, error) { return ring, nil },
		TrustedNow: func() (time.Time, bool) { return time.Time{}, false }})
	r := one(t, im.Import(context.Background(), SourceUSB, []string{"testdata/sample-provision.mbu"}))
	if r.Action != ActionInstalled || r.Message != "Set up as North Musallah" {
		t.Fatalf("result = %+v", r)
	}
	c, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	want := config.Config{DeviceID: "00000000-0000-0000-0000-000000000001", DisplayName: "North Musallah",
		ProvisionSequence: 1795089600250, BackendURL: "https://api.example.org",
		ClaimSecret: "AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE"}
	if *c != want {
		t.Fatalf("agent.toml = %+v\nwant %+v", *c, want)
	}

	// The same file signed by a key the board does not trust is refused.
	other := New(Deps{Layout: store.Layout{Root: t.TempDir()}, Config: config.InMemory(config.Config{}),
		Ring: func() (*trust.Ring, error) { return trust.NewRing([]ed25519.PublicKey{contentKey.Public().(ed25519.PublicKey)}, nil), nil }})
	if r := one(t, other.Import(context.Background(), SourceUSB, []string{"testdata/sample-provision.mbu"})); r.Action != ActionRejected {
		t.Fatalf("untrusted key: %+v", r)
	}
}
