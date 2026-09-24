// Package updatescreen puts the kiosk on the "Working on updates" screen
// (update-anim/index.html, served by the local server at /_mb/updating) while
// the importer installs something a person brought to the board, and returns
// it to the board afterwards (docs/architecture.md, section 8).
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
)

// Path is where the local server serves the screen.
const Path = "/_mb/updating"

// callTimeout bounds one CDP round trip.
const callTimeout = 5 * time.Second

// Screen implements importer.Screen.
type Screen struct {
	cdp      *cdp.Client
	baseURL  string // e.g. http://127.0.0.1:8080
	logger   *slog.Logger
	mu       sync.Mutex
	returnAt *time.Timer
	active   bool
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

// Begin navigates the kiosk to the update screen and waits for its hook.
func (s *Screen) Begin(ctx context.Context) {
	s.mu.Lock()
	if s.returnAt != nil {
		s.returnAt.Stop()
		s.returnAt = nil
	}
	s.active = true
	s.mu.Unlock()

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

// Caption sets the line under the headline.
func (s *Screen) Caption(ctx context.Context, text string) {
	arg, _ := json.Marshal(text)
	s.eval(ctx, "window.mbUpdate && window.mbUpdate.caption("+string(arg)+")")
}

// Finish shows the outcome, then returns the kiosk to the board when the
// countdown ends. When restarting, the agent is about to be replaced; the new
// agent returns the kiosk to the board when it starts (see Resume).
func (s *Screen) Finish(ctx context.Context, ok bool, detail string, restarting bool) {
	seconds := 5
	opts := map[string]any{"seconds": seconds, "caption": detail, "message": "Returning to MusallahBoard"}
	switch {
	case restarting:
		opts["message"] = "Restarting MusallahBoard"
	case !ok:
		seconds = 10
		opts["seconds"] = seconds
		opts["headline"] = "Update not installed"
	}
	arg, _ := json.Marshal(opts)
	s.eval(ctx, "window.mbUpdate && window.mbUpdate.complete("+string(arg)+")")
	if restarting {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.returnAt = time.AfterFunc(time.Duration(seconds)*time.Second, func() {
		s.returnToBoard(context.Background())
	})
}

// Resume is called once at agent startup. If the kiosk is still on the update
// screen (the previous agent was replaced mid-update, or crashed), it
// completes it and returns to the board.
func (s *Screen) Resume(ctx context.Context) {
	cctx, cancel := context.WithTimeout(ctx, callTimeout)
	target, err := s.cdp.FirstPageTarget(cctx)
	cancel()
	if err != nil || !strings.Contains(target.URL, Path) {
		return
	}
	s.mu.Lock()
	s.active = true
	s.mu.Unlock()
	s.Finish(ctx, true, "", false)
}

func (s *Screen) returnToBoard(ctx context.Context) {
	s.mu.Lock()
	s.active = false
	s.returnAt = nil
	s.mu.Unlock()
	if err := s.navigate(ctx, s.baseURL+"/"); err != nil {
		s.logger.Debug("update screen: return to board failed", "err", err)
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
