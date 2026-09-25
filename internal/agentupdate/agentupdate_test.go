package agentupdate

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/LensBridge/agent/internal/notice"
	"github.com/LensBridge/agent/internal/selfupdate"
	"github.com/LensBridge/agent/internal/store"
)

type fakeScreen struct {
	onScreen bool
	finished []notice.Notice
	held     []notice.Notice
}

func (f *fakeScreen) OnScreen(context.Context) bool { return f.onScreen }
func (f *fakeScreen) Finish(_ context.Context, n notice.Notice) {
	f.finished = append(f.finished, n)
}
func (f *fakeScreen) Hold(_ context.Context, n notice.Notice) { f.held = append(f.held, n) }

type env struct {
	l       store.Layout
	screen  *fakeScreen
	banners []notice.Notice
	resumed int
	r       *Reporter
}

func newEnv(t *testing.T, version string) *env {
	t.Helper()
	e := &env{l: store.Layout{Root: t.TempDir()}, screen: &fakeScreen{}}
	os.MkdirAll(filepath.Join(e.l.Root, "agent"), 0o750)
	e.r = New(Deps{
		Layout: e.l, Version: version, Screen: e.screen,
		Banner: func(n notice.Notice) { e.banners = append(e.banners, n) },
		Resume: func() { e.resumed++ },
	})
	e.r.watchTimeout, e.r.watchPoll = 2*time.Second, 10*time.Millisecond
	return e
}

func (e *env) outcome(t *testing.T, o selfupdate.Outcome) {
	t.Helper()
	raw, _ := json.Marshal(o)
	if err := os.WriteFile(e.l.AgentLastUpdate(), raw, 0o644); err != nil {
		t.Fatal(err)
	}
}

// staged is what the agent that staged the update left behind.
func staged(t *testing.T, e *env) {
	t.Helper()
	raw, _ := json.Marshal(notice.New(notice.OK, "Update complete", "Agent 0.3.0", "Board app 2.1.0"))
	if err := os.WriteFile(e.l.AgentPendingNotice(), raw, 0o640); err != nil {
		t.Fatal(err)
	}
}

func TestNewAgentShowsTheBatch(t *testing.T) {
	e := newEnv(t, "0.3.0")
	e.screen.onScreen = true
	staged(t, e)
	e.r.Startup(context.Background())
	if len(e.screen.finished) != 1 {
		t.Fatalf("finished = %+v", e.screen.finished)
	}
	n := e.screen.finished[0]
	if n.Tone != notice.OK || strings.Join(n.Lines, "|") != "Agent 0.3.0|Board app 2.1.0" {
		t.Fatalf("outcome = %+v", n)
	}
	if _, err := os.Stat(e.l.AgentPendingNotice()); err == nil {
		t.Fatal("the pending notice was kept; it would show again")
	}
}

func TestRollbackIsShownOnce(t *testing.T) {
	e := newEnv(t, "0.2.1")
	e.screen.onScreen = true
	staged(t, e)
	e.outcome(t, selfupdate.Outcome{From: "0.2.1", To: "0.3.0", At: "2026-09-25T23:01:00Z",
		Status: selfupdate.StatusRolledBack, Message: "agent 0.3.0 did not come up healthy"})
	e.r.Startup(context.Background())
	if len(e.screen.finished) != 1 {
		t.Fatalf("finished = %+v", e.screen.finished)
	}
	n := e.screen.finished[0]
	want := "Agent 0.3.0 did not start correctly, so the board went back to 0.2.1|Board app 2.1.0"
	if n.Tone != notice.Problem || n.Headline != "Update not installed" || strings.Join(n.Lines, "|") != want {
		t.Fatalf("outcome = %+v", n)
	}

	// The next start has nothing to say.
	e.screen.onScreen = false
	e.r.Startup(context.Background())
	if len(e.screen.finished) != 1 || len(e.banners) != 0 {
		t.Fatalf("shown again: %+v %+v", e.screen.finished, e.banners)
	}
}

func TestRollbackOffScreenIsABanner(t *testing.T) {
	e := newEnv(t, "0.2.1")
	e.outcome(t, selfupdate.Outcome{From: "0.2.1", To: "0.3.0", At: "2026-09-25T23:01:00Z", Status: selfupdate.StatusRolledBack})
	e.r.Startup(context.Background())
	if len(e.banners) != 1 || e.banners[0].Tone != notice.Problem || len(e.screen.finished) != 0 {
		t.Fatalf("banners %+v, finished %+v", e.banners, e.screen.finished)
	}
}

func TestQueuedBatchHoldsTheScreen(t *testing.T) {
	e := newEnv(t, "0.3.0")
	e.screen.onScreen = true
	staged(t, e)
	os.MkdirAll(e.l.Inbox(), 0o770)
	os.WriteFile(filepath.Join(e.l.Inbox(), "usb-sda1-2-content.mbu"), []byte("x"), 0o640)
	e.r.Startup(context.Background())
	if len(e.screen.held) != 1 || len(e.screen.finished) != 0 {
		t.Fatalf("held %+v, finished %+v", e.screen.held, e.screen.finished)
	}
}

func TestPlainStartupSaysNothing(t *testing.T) {
	e := newEnv(t, "0.3.0")
	e.outcome(t, selfupdate.Outcome{From: "0.2.1", To: "0.3.0", At: "2026-09-25T23:01:00Z", Status: selfupdate.StatusOK})
	e.r.Startup(context.Background())
	if len(e.banners)+len(e.screen.finished)+len(e.screen.held) != 0 {
		t.Fatal("a successful update from long ago was announced")
	}
}

func TestRefusalIsNoticed(t *testing.T) {
	e := newEnv(t, "0.2.1")
	e.screen.onScreen = true
	e.r.d.Now = func() time.Time { return time.Date(2026, 9, 25, 23, 0, 0, 0, time.UTC) }
	resumed := make(chan struct{})
	e.r.d.Resume = func() { close(resumed) }
	// The outcome is in place before the watch starts, so the watch's reads
	// and this write do not overlap.
	e.outcome(t, selfupdate.Outcome{From: "0.2.1", To: "0.3.0", At: "2026-09-25T23:00:03Z",
		Status: selfupdate.StatusRefused, Message: "the staged package did not verify"})
	e.r.Staged(notice.New(notice.OK, "Update complete", "Agent 0.3.0", "Board app 2.1.0"))
	select {
	case <-resumed:
	case <-time.After(time.Second):
		t.Fatal("the inbox was not resumed")
	}
	if len(e.screen.finished) != 1 {
		t.Fatalf("finished %+v", e.screen.finished)
	}
	n := e.screen.finished[0]
	if n.Tone != notice.Problem || strings.Join(n.Lines, "|") != "Agent 0.3.0 could not be installed on this board|Board app 2.1.0" {
		t.Fatalf("outcome = %+v", n)
	}
}
