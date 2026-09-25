// Package updatescreen puts the kiosk on the "Working on updates" screen
// (update-anim/index.html, served by the local server at /_mb/updating) while
// the importer installs something, shows the outcome, and returns the kiosk to
// the board afterwards (docs/architecture.md, section 8). When nothing is
// installed there is no screen: the board app shows a banner instead.
//
// Switching between the board and the screen is a dip to black: the page on
// screen fades to black (injected over CDP, so it works whatever page it is),
// and the next page fades in from black on its own. A true cross-fade would
// need both pages alive at once, which a navigation cannot give.
//
// The screen is driven over CDP through its window.mbUpdate hook. It lives in
// the agent, not the board app, because an app update replaces the app while
// the screen is up, and an agent update restarts the server that served it:
// once loaded the page needs nothing from anyone.
//
// Every call is best effort. The kiosk may be restarting or absent (a board
// without a display attached still takes updates), and an update must never
// fail because the screen could not be shown.
package updatescreen

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/LensBridge/agent/internal/cdp"
	"github.com/LensBridge/agent/internal/notice"
)

// Path is where the local server serves the screen.
const Path = "/_mb/updating"

// callTimeout bounds one CDP round trip.
const callTimeout = 5 * time.Second

// fadeOutScript lays a black veil over whatever page is showing and resolves
// once it is opaque. Keep the duration in step with the fade-in of
// update-anim/index.html and of the board app's index.html.
const fadeOutScript = `new Promise(function (done) {
  var v = document.getElementById('mb-fade-out');
  if (!v) {
    v = document.createElement('div');
    v.id = 'mb-fade-out';
    v.style.cssText = 'position:fixed;inset:0;background:#000;opacity:0;z-index:2147483647;' +
      'pointer-events:none;transition:opacity 600ms ease-in-out';
    (document.body || document.documentElement).appendChild(v);
    v.getBoundingClientRect();
  }
  v.style.opacity = '1';
  setTimeout(function () { done(true); }, 650);
})`

// Screen implements importer.Screen.
type Screen struct {
	cdp     *cdp.Client
	baseURL string // e.g. http://127.0.0.1:8080
	logger  *slog.Logger

	mu       sync.Mutex
	returnAt *time.Timer
	active   bool
	// carry is the outcome of an agent update, held while the batch queued
	// behind it installs, and shown together with that batch's outcome.
	carry *notice.Notice
}

// New returns a Screen for the kiosk at the default CDP endpoint, whose board
// is served at baseURL.
func New(c *cdp.Client, baseURL string, logger *slog.Logger) *Screen {
	return &Screen{cdp: c, baseURL: strings.TrimRight(baseURL, "/"), logger: logger}
}

// Active reports whether the screen is up (for /api/local/status).
func (s *Screen) Active() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.active
}

// Begin puts the kiosk on the update screen, or, when it is already there
// (held over an agent restart), takes it back to the working state.
func (s *Screen) Begin(ctx context.Context) {
	s.mu.Lock()
	if s.returnAt != nil {
		s.returnAt.Stop()
		s.returnAt = nil
	}
	already := s.active
	s.active = true
	s.mu.Unlock()

	if already && s.OnScreen(ctx) {
		s.eval(ctx, "window.mbUpdate && window.mbUpdate.working()")
		return
	}
	s.fadeOut(ctx)
	if err := s.navigate(ctx, s.baseURL+Path); err != nil {
		s.logger.Debug("update screen: navigate failed", "err", err)
		return
	}
	// The hook is defined at the end of <body>; poll rather than assume.
	for range 25 {
		var ready bool
		cctx, cancel := context.WithTimeout(ctx, callTimeout)
		err := s.cdp.EvaluateValue(cctx, `typeof window.mbUpdate === 'object'`, &ready)
		cancel()
		if err == nil && ready {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// Caption sets the line under "Working on updates".
func (s *Screen) Caption(ctx context.Context, text string) {
	arg, _ := json.Marshal(text)
	s.eval(ctx, "window.mbUpdate && window.mbUpdate.caption("+string(arg)+")")
}

// Restarting says the agent is about to be replaced. The screen stays up and
// working; the next agent finishes it (package agentupdate).
func (s *Screen) Restarting(ctx context.Context) {
	s.Caption(ctx, "Restarting MusallahBoard")
}

// Finish shows the outcome, merged with any held one, then returns the kiosk
// to the board when the countdown ends.
func (s *Screen) Finish(ctx context.Context, n notice.Notice) {
	s.mu.Lock()
	if s.carry != nil {
		n = merge(*s.carry, n)
		s.carry = nil
	}
	if s.returnAt != nil {
		s.returnAt.Stop()
	}
	s.mu.Unlock()

	seconds := 6
	if n.Tone == notice.Problem {
		seconds = 12
	}
	lines := n.Lines
	if lines == nil {
		lines = []string{}
	}
	arg, _ := json.Marshal(map[string]any{
		"tone": n.Tone, "headline": n.Headline, "lines": lines, "footer": n.Footer,
		"seconds": seconds, "message": "Returning to MusallahBoard",
	})
	s.eval(ctx, "window.mbUpdate && window.mbUpdate.complete("+string(arg)+")")

	s.mu.Lock()
	defer s.mu.Unlock()
	s.returnAt = time.AfterFunc(time.Duration(seconds)*time.Second, func() {
		s.returnToBoard(context.Background())
	})
}

// holdTimeout bounds how long a held screen waits for the batch queued behind
// an agent update, should that batch never report.
const holdTimeout = 3 * time.Minute

// Hold keeps the screen up with n, the outcome of an agent update, for the
// batch queued behind it to finish; that batch's Finish shows both.
func (s *Screen) Hold(ctx context.Context, n notice.Notice) {
	s.mu.Lock()
	s.active = true
	s.carry = &n
	if s.returnAt != nil {
		s.returnAt.Stop()
	}
	s.returnAt = time.AfterFunc(holdTimeout, func() { s.Finish(context.Background(), notice.Notice{Tone: notice.Progress}) })
	s.mu.Unlock()
	s.eval(ctx, "window.mbUpdate && window.mbUpdate.working()")
	s.Caption(ctx, "Installing the rest of the update")
}

// OnScreen reports whether the kiosk is showing the update screen (it is
// after an agent update restarted the agent under it).
func (s *Screen) OnScreen(ctx context.Context) bool {
	cctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	target, err := s.cdp.FirstPageTarget(cctx)
	return err == nil && strings.Contains(target.URL, Path)
}

// merge combines a held outcome with the one that followed it: the more
// serious tone and its headline, and the lines of both.
func merge(a, b notice.Notice) notice.Notice {
	rank := map[notice.Tone]int{notice.Progress: 0, notice.Neutral: 1, notice.OK: 2, notice.Problem: 3}
	out := a
	if rank[b.Tone] > rank[a.Tone] {
		out = b
	}
	out.Lines = notice.Fold(append(append([]string{}, a.Lines...), b.Lines...))
	if out.Footer == "" {
		out.Footer = a.Footer
		if out.Footer == "" {
			out.Footer = b.Footer
		}
	}
	return out
}

func (s *Screen) returnToBoard(ctx context.Context) {
	s.mu.Lock()
	s.active = false
	s.returnAt = nil
	s.mu.Unlock()
	s.fadeOut(ctx)
	if err := s.navigate(ctx, s.baseURL+"/"); err != nil {
		s.logger.Debug("update screen: return to board failed", "err", err)
	}
}

// fadeOut fades the current page to black. If it cannot (no kiosk, a page
// that will not run script), the switch is simply a cut.
func (s *Screen) fadeOut(ctx context.Context) {
	cctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	if err := s.cdp.EvaluateValue(cctx, fadeOutScript, nil); err != nil {
		s.logger.Debug("update screen: fade out failed", "err", err)
	}
}

func (s *Screen) navigate(ctx context.Context, url string) error {
	cctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	target, err := s.cdp.FirstPageTarget(cctx)
	if err != nil {
		return err
	}
	return s.cdp.Call(cctx, target, "Page.navigate", map[string]any{"url": url}, nil)
}

func (s *Screen) eval(ctx context.Context, expr string) {
	cctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	if _, err := s.cdp.RuntimeEvaluate(cctx, expr); err != nil {
		s.logger.Debug("update screen: evaluate failed", "err", err)
	}
}
