package updates

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/LensBridge/agent/internal/importer"
	"github.com/LensBridge/agent/internal/mbu"
	"github.com/LensBridge/agent/internal/state"
	"github.com/LensBridge/agent/internal/store"
	"github.com/LensBridge/agent/internal/trust"
)

var releaseKey = ed25519.NewKeyFromSeed(bytes.Repeat([]byte{2}, 32))

type env struct {
	l   store.Layout
	im  *importer.Importer
	s   *Scheduler
	now time.Time
	got []Info
}

// newEnv is a scheduler over a temp store with app 2.0.0 installed, agent
// 0.3.0 running and the install time at 23:00 UTC (no content, so the zone
// is the system's; tests pin it to UTC).
func newEnv(t *testing.T) *env {
	t.Helper()
	time.Local = time.UTC
	e := &env{l: store.Layout{Root: t.TempDir()}, now: time.Date(2026, 9, 25, 14, 0, 0, 0, time.UTC)}
	ring := trust.NewRing(nil, []ed25519.PublicKey{releaseKey.Public().(ed25519.PublicKey)})
	e.im = importer.New(importer.Deps{
		Layout: e.l, DeviceID: "board", AgentVersion: "0.3.0",
		Ring: func() (*trust.Ring, error) { return ring, nil },
		Now:  func() time.Time { return e.now },
	})
	setApp(t, e.l, "2.0.0")
	e.s = e.newScheduler()
	return e
}

func (e *env) newScheduler() *Scheduler {
	return New(Deps{
		Layout: e.l, Importer: e.im, Hour: 23,
		Changed: func(i Info) { e.got = append(e.got, i) },
		Now:     func() time.Time { return e.now },
	})
}

func setApp(t *testing.T, l store.Layout, v string) {
	t.Helper()
	if _, err := l.State().Update(func(s *state.State) error { s.AppVersion = v; return nil }); err != nil {
		t.Fatal(err)
	}
}

// write builds a package into the inbox, where the release channels download.
func (e *env) write(t *testing.T, m mbu.Manifest, src []mbu.Source) string {
	t.Helper()
	var buf bytes.Buffer
	if err := mbu.Build(&buf, m, src, []ed25519.PrivateKey{releaseKey}); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(e.l.Inbox(), ".download")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, string(m.Type)+"-"+m.Version+".mbu")
	if err := os.WriteFile(p, buf.Bytes(), 0o640); err != nil {
		t.Fatal(err)
	}
	return p
}

func (e *env) app(t *testing.T, version string, localAPI int) string {
	return e.write(t, mbu.Manifest{Type: mbu.TypeApp, Version: version, App: &mbu.AppInfo{LocalAPI: localAPI}},
		[]mbu.Source{{Path: "index.html", Data: []byte("<html>" + version)}})
}

func (e *env) agent(t *testing.T, version string) string {
	return e.write(t, mbu.Manifest{Type: mbu.TypeAgent, Version: version,
		Agent: &mbu.AgentInfo{Arch: runtime.GOARCH, Binary: "musallahboard-agent"}},
		[]mbu.Source{{Path: "musallahboard-agent", Data: []byte("#!/bin/sh\necho " + version)}})
}

func TestOfferWaitsForTheWindow(t *testing.T) {
	e := newEnv(t)
	if err := e.s.Offer(e.app(t, "2.1.0", 2)); err != nil {
		t.Fatal(err)
	}
	info := e.s.Info()
	if len(info.Available) != 1 || info.Available[0].Version != "2.1.0" || info.Available[0].Description != "board app 2.1.0" {
		t.Fatalf("available = %+v", info.Available)
	}
	if info.InstallAt == nil || *info.InstallAt != "2026-09-25T23:00:00Z" {
		t.Fatalf("installAt = %v, want tonight at 23:00", info.InstallAt)
	}
	if len(e.got) == 0 {
		t.Error("Changed was not told about the new update")
	}
	if e.s.due() {
		t.Fatal("due at 14:00")
	}
	if st, _ := e.l.State().Load(); st.AppVersion != "2.0.0" {
		t.Fatalf("installed %s before the window", st.AppVersion)
	}

	e.now = time.Date(2026, 9, 25, 23, 0, 30, 0, time.UTC)
	if !e.s.due() {
		t.Fatal("not due at 23:00")
	}
	if _, ok := e.s.InstallNow(context.Background()); !ok {
		t.Fatal("nothing installed")
	}
	if st, _ := e.l.State().Load(); st.AppVersion != "2.1.0" {
		t.Fatalf("app = %s after the window opened", st.AppVersion)
	}
	if info := e.s.Info(); len(info.Available) != 0 || info.InstallAt != nil {
		t.Fatalf("still waiting after install: %+v", info)
	}
	if _, err := os.Stat(e.s.path(mbu.TypeApp)); err == nil {
		t.Error("the installed package was left in the updates directory")
	}
}

func TestWindow(t *testing.T) {
	e := newEnv(t)
	cases := []struct {
		at   time.Time
		open bool
		next string
	}{
		{time.Date(2026, 9, 25, 22, 59, 0, 0, time.UTC), false, "2026-09-25T23:00:00Z"},
		{time.Date(2026, 9, 25, 23, 0, 0, 0, time.UTC), true, ""},
		{time.Date(2026, 9, 26, 2, 59, 0, 0, time.UTC), true, ""},
		{time.Date(2026, 9, 26, 3, 0, 0, 0, time.UTC), false, "2026-09-26T23:00:00Z"},
		{time.Date(2026, 9, 26, 8, 0, 0, 0, time.UTC), false, "2026-09-26T23:00:00Z"},
	}
	for _, tc := range cases {
		open, next := e.s.window(tc.at)
		if open != tc.open {
			t.Errorf("%s: open = %v, want %v", tc.at, open, tc.open)
		}
		if !open && next.Format(time.RFC3339) != tc.next {
			t.Errorf("%s: next = %s, want %s", tc.at, next.Format(time.RFC3339), tc.next)
		}
	}
}

func TestFirstAppInstallsAtOnce(t *testing.T) {
	e := newEnv(t)
	setApp(t, e.l, "")
	if err := e.s.Offer(e.app(t, "2.1.0", 2)); err != nil {
		t.Fatal(err)
	}
	if !e.s.due() {
		t.Fatal("a board with no app waits for the window")
	}
	if info := e.s.Info(); info.InstallAt == nil || *info.InstallAt != "2026-09-25T14:00:00Z" {
		t.Fatalf("installAt = %v, want now", info.InstallAt)
	}
}

func TestOfferRefusesWhatWouldNotInstall(t *testing.T) {
	e := newEnv(t)
	if err := e.s.Offer(e.app(t, "2.0.0", 2)); err == nil {
		t.Error("offered the installed version")
	}
	if err := e.s.Offer(e.app(t, "1.9.0", 2)); err == nil {
		t.Error("offered an older version")
	}
	if err := e.s.Offer(e.app(t, "3.0.0", importer.LocalAPIVersion+1)); err == nil {
		t.Error("offered an app that needs a newer agent, with no agent waiting")
	}
	if len(e.s.Info().Available) != 0 {
		t.Fatal("something is waiting")
	}

	// With a new agent waiting, the app that needs it may wait too: the
	// importer installs the agent first and queues the app behind it.
	if err := e.s.Offer(e.agent(t, "0.4.0")); err != nil {
		t.Fatal(err)
	}
	if err := e.s.Offer(e.app(t, "3.0.0", importer.LocalAPIVersion+1)); err != nil {
		t.Fatal(err)
	}
	if n := len(e.s.Info().Available); n != 2 {
		t.Fatalf("%d waiting, want 2", n)
	}
}

func TestInstallNowStagesAgentAndQueuesApp(t *testing.T) {
	e := newEnv(t)
	if err := e.s.Offer(e.agent(t, "0.4.0")); err != nil {
		t.Fatal(err)
	}
	if err := e.s.Offer(e.app(t, "2.1.0", 2)); err != nil {
		t.Fatal(err)
	}
	b, ok := e.s.InstallNow(context.Background())
	if !ok || !b.AgentStaged {
		t.Fatalf("agent not staged: %+v", b.Results)
	}
	if _, err := os.Stat(e.l.StagedReady()); err != nil {
		t.Fatal("no staged agent")
	}
	queued, _ := filepath.Glob(filepath.Join(e.l.Inbox(), "*.mbu"))
	if len(queued) != 1 {
		t.Fatalf("queued behind the agent: %v", queued)
	}
}

func TestRestartKeepsWaitingUpdates(t *testing.T) {
	e := newEnv(t)
	if err := e.s.Offer(e.app(t, "2.1.0", 2)); err != nil {
		t.Fatal(err)
	}
	if got := e.newScheduler().Pending(mbu.TypeApp); got != "2.1.0" {
		t.Fatalf("after a restart waiting = %q", got)
	}

	// Installed from a USB stick meanwhile: dropped at the next start.
	setApp(t, e.l, "2.1.0")
	if got := e.newScheduler().Pending(mbu.TypeApp); got != "" {
		t.Fatalf("still waiting for an installed version: %q", got)
	}
	if _, err := os.Stat(e.s.path(mbu.TypeApp)); err == nil {
		t.Error("the stale package was kept")
	}
}

func TestCheckAndInstall(t *testing.T) {
	e := newEnv(t)
	e.s.d.Check = func(context.Context) error { return e.s.Offer(e.app(t, "2.1.0", 2)) }
	out := e.s.CheckAndInstall(context.Background())
	if out.CheckError != "" || len(out.Results) != 1 || out.Results[0].Action != importer.ActionInstalled {
		t.Fatalf("outcome = %+v", out)
	}
	if st, _ := e.l.State().Load(); st.AppVersion != "2.1.0" {
		t.Fatalf("app = %s", st.AppVersion)
	}
}
