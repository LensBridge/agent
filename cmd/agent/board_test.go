package main

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/LensBridge/agent/internal/core/cdp"
	"github.com/LensBridge/agent/internal/core/config"
)

// fakeKiosk scripts what the browser reports and records what the healer did.
type fakeKiosk struct {
	url       string
	targetErr error
	health    error
	reloads   int
	restarts  int
	navigated int
}

func newTestHealer(k *fakeKiosk, now *time.Time, updating *bool) *kioskHealer {
	return &kioskHealer{
		log:          slog.New(slog.DiscardHandler),
		now:          func() time.Time { return *now },
		updateActive: func() bool { return *updating },
		target: func(context.Context) (cdp.Target, error) {
			return cdp.Target{Type: "page", URL: k.url}, k.targetErr
		},
		navigate: func(context.Context, cdp.Target, string) error { k.navigated++; return nil },
		health:   func(context.Context) error { return k.health },
		reload:   func(context.Context) error { k.reloads++; return nil },
		restart:  func(context.Context) error { k.restarts++; return nil },
	}
}

var (
	errCrashed = fmt.Errorf("%w (http://127.0.0.1:8080/)", cdp.ErrPageCrashed)
	errHung    = fmt.Errorf("%w: read: context deadline exceeded", cdp.ErrPageUnresponsive)
)

type healerRig struct {
	t        *testing.T
	k        *fakeKiosk
	h        *kioskHealer
	now      time.Time
	updating bool
}

func newRig(t *testing.T) *healerRig {
	r := &healerRig{t: t, k: &fakeKiosk{url: "http://127.0.0.1:8080/"}, now: time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)}
	r.h = newTestHealer(r.k, &r.now, &r.updating)
	return r
}

// ticks advances the clock one watch interval per tick.
func (r *healerRig) ticks(n int) {
	for i := 0; i < n; i++ {
		r.now = r.now.Add(kioskWatchInterval)
		r.h.tick(context.Background())
	}
}

func (r *healerRig) want(reloads, restarts int) {
	r.t.Helper()
	if r.k.reloads != reloads || r.k.restarts != restarts {
		r.t.Fatalf("reloads=%d restarts=%d, want %d and %d", r.k.reloads, r.k.restarts, reloads, restarts)
	}
}

// A crash is reloaded at once; if the reload does not bring it back, the
// kiosk unit is restarted.
func TestKioskHealerCrashReloadThenRestart(t *testing.T) {
	r := newRig(t)
	r.k.health = errCrashed

	r.ticks(1)
	r.want(1, 0)
	r.ticks(1)
	r.want(1, 1)
}

func TestKioskHealerCrashRecoveredByReload(t *testing.T) {
	r := newRig(t)
	r.k.health = errCrashed
	r.ticks(1)
	r.k.health = nil
	r.ticks(10)
	r.want(1, 0)
}

// A page that does not answer gets kioskHungThreshold probes first: a Pi
// decoding a large poster can miss one.
func TestKioskHealerHungPageThreshold(t *testing.T) {
	r := newRig(t)
	r.k.health = errHung

	r.ticks(kioskHungThreshold - 1)
	r.want(0, 0)
	r.ticks(1)
	r.want(1, 0)
	r.ticks(kioskHungThreshold)
	r.want(1, 1)
}

// One slow answer among good ones never escalates.
func TestKioskHealerIntermittentSlownessIsForgiven(t *testing.T) {
	r := newRig(t)
	for i := 0; i < 20; i++ {
		r.k.health = errHung
		r.ticks(kioskHungThreshold - 1)
		r.k.health = nil
		r.ticks(1)
	}
	r.want(0, 0)
}

// A page that is broken whatever we do: restarts back off up to an hour and
// each window gets at most one reload and one restart.
func TestKioskHealerBacksOffPersistentFailure(t *testing.T) {
	r := newRig(t)
	r.k.health = errCrashed

	r.ticks(2) // reload, restart
	r.want(1, 1)

	// 24 hours of a permanently crashed page.
	r.ticks(int(24 * time.Hour / kioskWatchInterval))
	// Gaps 5, 10, 20, 40 min then hourly: about 4 + 23 more restarts.
	if r.k.restarts > 30 {
		t.Errorf("%d restarts in a day: the backoff is not holding", r.k.restarts)
	}
	if r.k.restarts < 10 {
		t.Errorf("only %d restarts in a day: the healer gave up", r.k.restarts)
	}
	if r.k.reloads > r.k.restarts+1 {
		t.Errorf("%d reloads for %d restarts: reloads must be bounded too", r.k.reloads, r.k.restarts)
	}
}

func TestKioskHealerBackoffMinimumGap(t *testing.T) {
	r := newRig(t)
	r.k.health = errCrashed
	r.ticks(2)
	r.want(1, 1)
	// After the restart the page gets one more reload, then nothing until
	// kioskRestartMinGap has passed.
	r.ticks(int(kioskRestartMinGap/kioskWatchInterval) - 1)
	r.want(2, 1)
	r.ticks(1)
	r.want(2, 2)
	r.ticks(1)
	r.want(3, 2)
}

// A page that recovers when reloaded is reloaded each time it dies, whatever
// the restart backoff says.
func TestKioskHealerReloadsAPageThatRecovers(t *testing.T) {
	r := newRig(t)
	for i := 0; i < 5; i++ {
		r.k.health = errCrashed
		r.ticks(1)
		r.k.health = nil
		r.ticks(2)
	}
	r.want(5, 0)
}

// The backoff starts over only after a sustained healthy stretch; a page that
// comes up for a minute and dies again keeps its backoff.
func TestKioskHealerBackoffResetsAfterSustainedHealth(t *testing.T) {
	r := newRig(t)
	r.k.health = errCrashed
	r.ticks(2)
	r.want(1, 1)

	r.k.health = nil
	r.ticks(2)
	r.k.health = errCrashed
	r.ticks(2)
	r.want(2, 1) // reloaded; the restart is held by the backoff

	r.k.health = nil
	r.ticks(int(kioskRestartResetAfter/kioskWatchInterval) + 1)
	r.k.health = errCrashed
	r.ticks(2)
	r.want(3, 2) // reload, then a restart at once: the backoff started over
	r.ticks(1)
	r.want(4, 2)
	r.ticks(1)
	r.want(4, 2) // and the next restart waits kioskRestartMinGap again
}

// Legitimate states are never healed.
func TestKioskHealerLeavesLegitimateStatesAlone(t *testing.T) {
	for name, err := range map[string]error{
		"board":         nil,
		"splash or old": cdp.ErrEvalUndefined,
		"navigating":    fmt.Errorf("%w: Execution context was destroyed.", cdp.ErrPageNavigating),
		"hook threw":    fmt.Errorf("%w: TypeError", cdp.ErrPageThrew),
		"browser gone":  fmt.Errorf("%w: ws dial", cdp.ErrDebuggerUnreachable),
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t)
			r.k.health = err
			r.ticks(50)
			r.want(0, 0)
		})
	}
}

func TestKioskHealerNoBrowser(t *testing.T) {
	r := newRig(t)
	r.k.targetErr = cdp.ErrDebuggerUnreachable
	r.k.health = errCrashed // never consulted
	r.ticks(50)
	r.want(0, 0)
}

// The update screen is driven over CDP by the importer; the healer must not
// reload or restart under it, even when it does not answer.
func TestKioskHealerSkipsUpdateScreen(t *testing.T) {
	r := newRig(t)
	r.updating = true
	r.k.health = errHung
	r.ticks(50)
	r.want(0, 0)

	// Failures seen during the update do not carry over.
	r.updating = false
	r.ticks(kioskHungThreshold - 1)
	r.want(0, 0)
}

func TestKioskHealerChromeErrorPage(t *testing.T) {
	r := newRig(t)
	r.k.url = "chrome-error://chromewebdata/"
	r.k.health = errCrashed
	r.ticks(1)
	if r.k.navigated != 1 {
		t.Errorf("navigated = %d, want 1", r.k.navigated)
	}
	r.want(0, 0)
}

func TestBackendTransport(t *testing.T) {
	on := true
	cases := []struct {
		name    string
		cfg     config.Config
		wantErr bool
		wantLog string
	}{
		{"tls", config.Config{BackendURL: "https://api.example.com", WebSocketURL: "wss://api.example.com/ws"}, false, ""},
		{"http without opt-in", config.Config{BackendURL: "http://10.0.2.2:8080", WebSocketURL: "ws://10.0.2.2:8080/ws"}, true, "REFUSING TO TALK TO THE BACKEND"},
		{"socket elsewhere", config.Config{BackendURL: "https://api.example.com", WebSocketURL: "wss://other.example.com/ws"}, true, "REFUSING TO TALK TO THE BACKEND"},
		{"insecure dev", config.Config{BackendURL: "http://10.0.2.2:8080", WebSocketURL: "ws://10.0.2.2:8080/ws", InsecureDevSet: &on}, false, "INSECURE DEVELOPMENT BOARD"},
		{"insecure dev with tls still warns", config.Config{BackendURL: "https://api.example.com", WebSocketURL: "wss://api.example.com/ws", InsecureDevSet: &on}, false, "INSECURE DEVELOPMENT BOARD"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			err := backendTransport(slog.New(slog.NewTextHandler(&buf, nil)), &tc.cfg)
			if (err != nil) != tc.wantErr {
				t.Errorf("err = %v, want error %v", err, tc.wantErr)
			}
			if tc.wantLog == "" && buf.Len() != 0 {
				t.Errorf("unexpected log: %s", buf.String())
			}
			if tc.wantLog != "" && !strings.Contains(buf.String(), tc.wantLog) {
				t.Errorf("log %q does not say %q", buf.String(), tc.wantLog)
			}
		})
	}
}
