package selfupdate

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LensBridge/agent/internal/mbu"
	"github.com/LensBridge/agent/internal/state"
	"github.com/LensBridge/agent/internal/store"
	"github.com/LensBridge/agent/internal/trust"
)

var (
	releaseKey = ed25519.NewKeyFromSeed(bytes.Repeat([]byte{2}, 32))
	otherKey   = ed25519.NewKeyFromSeed(bytes.Repeat([]byte{9}, 32))
)

const oldBinary = "old agent binary"

type harness struct {
	u        *Updater
	cmds     []string
	versionO string
	healthy  error
}

func newHarness(t *testing.T, withContentKey bool) *harness {
	t.Helper()
	dir := t.TempDir()
	h := &harness{}
	l := store.Layout{Root: filepath.Join(dir, "var")}
	os.MkdirAll(l.AgentStagedDir(), 0o750)
	bin := filepath.Join(dir, "usr", "bin", "musallahboard-agent")
	os.MkdirAll(filepath.Dir(bin), 0o755)
	os.WriteFile(bin, []byte(oldBinary), 0o755)

	ts := &trust.Store{}
	ts.Add(trust.RoleRelease, releaseKey.Public().(ed25519.PublicKey), "ci")
	if withContentKey {
		ts.Add(trust.RoleContent, otherKey.Public().(ed25519.PublicKey), "lensbridge")
	}
	trustPath := filepath.Join(dir, "trust.json")
	if err := ts.Save(trustPath); err != nil {
		t.Fatal(err)
	}

	h.u = &Updater{
		Layout:         l,
		TrustPath:      trustPath,
		Binary:         bin,
		NewBinary:      filepath.Join(filepath.Dir(bin), ".musallahboard-agent.new"),
		Prev:           filepath.Join(dir, "usr", "lib", "musallahboard", "agent.prev"),
		TempRoot:       dir,
		Arch:           "arm64",
		CurrentVersion: "0.3.0",
		Run: func(ctx context.Context, name string, args ...string) (string, error) {
			h.cmds = append(h.cmds, strings.TrimSpace(filepath.Base(name)+" "+strings.Join(args, " ")))
			if len(args) == 1 && args[0] == "version" {
				return h.versionO, nil
			}
			return "", nil
		},
		Healthy: func(ctx context.Context, v string) error { return h.healthy },
		Now:     func() time.Time { return time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC) },
		Logf:    t.Logf,
	}
	return h
}

// stage writes a signed agent package and the ready marker.
func (h *harness) stage(t *testing.T, version, arch string, key ed25519.PrivateKey) {
	t.Helper()
	m := mbu.Manifest{Type: mbu.TypeAgent, Version: version, Agent: &mbu.AgentInfo{Arch: arch, Binary: "musallahboard-agent"}}
	var buf bytes.Buffer
	if err := mbu.Build(&buf, m, []mbu.Source{{Path: "musallahboard-agent", Data: []byte("new agent " + version)}},
		[]ed25519.PrivateKey{key}); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(h.u.Layout.StagedPackage(), buf.Bytes(), 0o640)
	os.WriteFile(h.u.Layout.StagedReady(), []byte("now\n"), 0o640)
	h.versionO = "musallahboard-agent " + version
}

func read(t *testing.T, p string) string {
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read %s: %v", p, err)
	}
	return string(b)
}

func lastUpdate(t *testing.T, h *harness) Outcome {
	var o Outcome
	if err := json.Unmarshal([]byte(read(t, h.u.Layout.AgentLastUpdate())), &o); err != nil {
		t.Fatal(err)
	}
	return o
}

func TestApplyOK(t *testing.T) {
	h := newHarness(t, false)
	h.stage(t, "0.4.0", "arm64", releaseKey)
	out, err := h.u.Apply(context.Background())
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if out.Status != StatusOK || out.From != "0.3.0" || out.To != "0.4.0" {
		t.Fatalf("outcome = %+v", out)
	}
	if got := read(t, h.u.Binary); got != "new agent 0.4.0" {
		t.Errorf("binary = %q", got)
	}
	if fi, _ := os.Stat(h.u.Binary); fi.Mode().Perm() != 0o755 {
		t.Errorf("binary mode = %v", fi.Mode())
	}
	if got := read(t, h.u.Prev); got != oldBinary {
		t.Errorf("prev = %q", got)
	}
	for _, p := range []string{h.u.Layout.StagedReady(), h.u.Layout.StagedPackage(), h.u.NewBinary} {
		if _, err := os.Stat(p); err == nil {
			t.Errorf("%s left behind", p)
		}
	}
	want := []string{".musallahboard-agent.new version", "musallahboard-agent trust fetch", "systemctl restart musallahboard-agent.service"}
	if fmt.Sprint(h.cmds) != fmt.Sprint(want) {
		t.Errorf("commands = %q, want %q", h.cmds, want)
	}
	if o := lastUpdate(t, h); o.Status != StatusOK || o.At == "" {
		t.Errorf("last-update = %+v", o)
	}
	// No temp directories left in TempRoot.
	if m, _ := filepath.Glob(filepath.Join(h.u.TempRoot, "musallahboard-selfupdate-*")); len(m) != 0 {
		t.Errorf("work dirs left: %v", m)
	}
}

func TestApplyNoTrustFetchWithContentKey(t *testing.T) {
	h := newHarness(t, true)
	h.stage(t, "0.4.0", "arm64", releaseKey)
	if _, err := h.u.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, c := range h.cmds {
		if strings.Contains(c, "trust fetch") {
			t.Fatalf("trust fetch run although a content key is trusted: %q", h.cmds)
		}
	}
}

func TestApplyRollsBack(t *testing.T) {
	h := newHarness(t, true)
	h.stage(t, "0.4.0", "arm64", releaseKey)
	h.healthy = errors.New("it reports version 0.3.0")
	out, err := h.u.Apply(context.Background())
	if err == nil || out.Status != StatusRolledBack {
		t.Fatalf("outcome = %+v, err = %v", out, err)
	}
	if got := read(t, h.u.Binary); got != oldBinary {
		t.Errorf("binary after rollback = %q", got)
	}
	st, _ := h.u.Layout.State().Load()
	if !st.AgentRejected("0.4.0") {
		t.Errorf("0.4.0 not recorded as rejected: %+v", st)
	}
	restarts := 0
	for _, c := range h.cmds {
		if c == "systemctl restart musallahboard-agent.service" {
			restarts++
		}
	}
	if restarts != 2 {
		t.Errorf("restarts = %d, want 2 (%q)", restarts, h.cmds)
	}
	if o := lastUpdate(t, h); o.Status != StatusRolledBack || !strings.Contains(o.Message, "rolled back") {
		t.Errorf("last-update = %+v", o)
	}
}

func TestApplyRefuses(t *testing.T) {
	cases := []struct {
		name    string
		setup   func(t *testing.T, h *harness)
		wantMsg string
	}{
		{"older", func(t *testing.T, h *harness) { h.stage(t, "0.2.0", "arm64", releaseKey) }, "not newer"},
		{"equal", func(t *testing.T, h *harness) { h.stage(t, "0.3.0", "arm64", releaseKey) }, "not newer"},
		{"wrong arch", func(t *testing.T, h *harness) { h.stage(t, "0.4.0", "amd64", releaseKey) }, "built for amd64"},
		{"untrusted key", func(t *testing.T, h *harness) { h.stage(t, "0.4.0", "arm64", otherKey) }, "did not verify"},
		{"rejected before", func(t *testing.T, h *harness) {
			h.stage(t, "0.4.0", "arm64", releaseKey)
			h.u.Layout.State().Update(func(s *state.State) error { s.AgentVersionsRejected = []string{"0.4.0"}; return nil })
		}, "failed to start"},
		{"binary does not run", func(t *testing.T, h *harness) {
			h.stage(t, "0.4.0", "arm64", releaseKey)
			h.versionO = "exec format error"
		}, "does not run"},
		{"symlinked package", func(t *testing.T, h *harness) {
			h.stage(t, "0.4.0", "arm64", releaseKey)
			real := h.u.Layout.StagedPackage() + ".real"
			os.Rename(h.u.Layout.StagedPackage(), real)
			os.Symlink(real, h.u.Layout.StagedPackage())
		}, "cannot read the staged package"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, true)
			tc.setup(t, h)
			out, err := h.u.Apply(context.Background())
			if err == nil || out.Status != StatusRefused || !strings.Contains(out.Message, tc.wantMsg) {
				t.Fatalf("outcome = %+v, err = %v; want refused with %q", out, err, tc.wantMsg)
			}
			if got := read(t, h.u.Binary); got != oldBinary {
				t.Errorf("binary changed on refusal: %q", got)
			}
			for _, c := range h.cmds {
				if strings.HasPrefix(c, "systemctl") {
					t.Errorf("service restarted on refusal: %q", h.cmds)
				}
			}
			if _, err := os.Stat(h.u.NewBinary); err == nil {
				t.Error("new binary left behind")
			}
			if o := lastUpdate(t, h); o.Status != StatusRefused {
				t.Errorf("last-update = %+v", o)
			}
		})
	}
}

func TestApplyNothingStaged(t *testing.T) {
	h := newHarness(t, true)
	if _, err := h.u.Apply(context.Background()); !errors.Is(err, ErrNothingStaged) {
		t.Fatalf("err = %v", err)
	}
	if _, err := os.Stat(h.u.Layout.AgentLastUpdate()); err == nil {
		t.Fatal("last-update written with nothing staged")
	}
}

func TestWaitHealthy(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		v := "0.3.0"
		if calls.Add(1) >= 2 {
			v = "0.4.0"
		}
		fmt.Fprintf(w, `{"localApi":2,"agentVersion":%q}`, v)
	}))
	defer srv.Close()
	if err := WaitHealthy(context.Background(), srv.URL, "0.4.0", 10*time.Second); err != nil {
		t.Fatal(err)
	}
	err := WaitHealthy(context.Background(), srv.URL, "9.9.9", 100*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "reports version 0.4.0") {
		t.Fatalf("err = %v", err)
	}
}
