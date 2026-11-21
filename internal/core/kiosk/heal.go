package kiosk

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
	"time"
)

// WatchInterval is how often the daemon checks that Chromium is not
// sitting on its own error page. The kiosk is ordered after the agent, so
// this is a backstop, not the mechanism.
const WatchInterval = 30 * time.Second

// Kiosk self-healing (Watch). The page is probed every
// WatchInterval. A crashed renderer is certain after one probe; a page
// that does not answer may be busy (a large poster decoding on a Pi), so it
// gets kioskHungThreshold probes. Then the watcher reloads the page; if the
// page is still dead after that, it restarts the kiosk unit. Restarts back off
// from kioskRestartMinGap to kioskRestartMaxGap, and the backoff only starts
// over after the page has been healthy for kioskRestartResetAfter, so a page
// that never comes back costs at most one reload and one restart an hour.
const (
	kioskProbeTimeout      = 5 * time.Second
	kioskCrashThreshold    = 1
	kioskHungThreshold     = 3
	kioskRestartMinGap     = 5 * time.Minute
	kioskRestartMaxGap     = time.Hour
	kioskRestartResetAfter = time.Hour
)

// Watch keeps the kiosk on the board at boardURL until ctx ends: it
// navigates Chromium back if it lands on its own error page (the server
// restarting under it, for instance) and heals a crashed or hung page
// (healer). updateActive reports whether the update screen is up.
func Watch(ctx context.Context, logger *slog.Logger, c *Client, boardURL string, updateActive func() bool) {
	h := newHealer(logger, c, boardURL, updateActive)
	t := time.NewTicker(WatchInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		h.tick(ctx)
	}
}

// healer is the watcher's state between ticks. Its browser and systemd
// operations are fields so the escalation can be tested without either.
type healer struct {
	log          *slog.Logger
	boardURL     string
	now          func() time.Time
	updateActive func() bool
	target       func(context.Context) (Target, error)
	navigate     func(context.Context, Target, string) error
	health       func(context.Context) error
	reload       func(context.Context) error
	restart      func(context.Context) error

	fails        int       // consecutive dead-page probes
	reloaded     bool      // reloaded since the last restart (or since healthy)
	restarts     int       // restarts since the backoff last started over
	nextRestart  time.Time // no restart before this
	healthySince time.Time // zero while the page is dead
	waitLogged   bool
}

func newHealer(logger *slog.Logger, c *Client, boardURL string, updateActive func() bool) *healer {
	if updateActive == nil {
		updateActive = func() bool { return false }
	}
	return &healer{
		log:          logger,
		boardURL:     boardURL,
		now:          time.Now,
		updateActive: updateActive,
		target:       c.FirstPageTarget,
		navigate: func(ctx context.Context, t Target, url string) error {
			return c.Call(ctx, t, "Page.navigate", map[string]any{"url": url}, nil)
		},
		health:  c.PageHealth,
		reload:  c.PageReload,
		restart: Restart,
	}
}

func (h *healer) tick(ctx context.Context) {
	// The update screen is driven over CDP by the importer and may be
	// waiting on an agent restart; nothing here may navigate under it.
	if h.updateActive() {
		h.fails = 0
		return
	}

	cctx, cancel := context.WithTimeout(ctx, kioskProbeTimeout)
	target, err := h.target(cctx)
	cancel()
	if err == nil && strings.HasPrefix(target.URL, "chrome-error:") {
		h.log.Info("kiosk is on an error page; returning it to the board")
		cctx, cancel := context.WithTimeout(ctx, kioskProbeTimeout)
		_ = h.navigate(cctx, target, h.boardURL)
		cancel()
		return
	}
	if err != nil {
		// No browser, or no page yet: starting, or restarting (perhaps
		// because we asked). systemd's Restart=always owns that case.
		return
	}

	cctx, cancel = context.WithTimeout(ctx, kioskProbeTimeout)
	err = h.health(cctx)
	cancel()
	if ctx.Err() != nil {
		return
	}

	threshold := 0
	switch {
	case errors.Is(err, ErrPageCrashed):
		threshold = kioskCrashThreshold
	case errors.Is(err, ErrPageUnresponsive):
		threshold = kioskHungThreshold
	case errors.Is(err, ErrDebuggerUnreachable), errors.Is(err, ErrNoPageTarget):
		// The browser went away between the two calls: not the page's fault,
		// and not proof it recovered either.
		return
	default:
		// Answered, or between documents, or the hook threw: the page is
		// alive. A navigation in flight is not a failure.
		h.healthy()
		return
	}

	h.healthySince = time.Time{}
	h.fails++
	if h.fails < threshold {
		h.log.Warn("kiosk page is not answering", "err", err, "failures", h.fails, "url", target.URL)
		return
	}
	h.heal(ctx, err, target.URL)
}

func (h *healer) healthy() {
	now := h.now()
	if h.fails > 0 || h.reloaded {
		h.log.Info("kiosk page is answering again")
	}
	h.fails, h.reloaded, h.waitLogged = 0, false, false
	if h.healthySince.IsZero() {
		h.healthySince = now
	}
	if h.restarts > 0 && now.Sub(h.healthySince) >= kioskRestartResetAfter {
		h.restarts, h.nextRestart = 0, time.Time{}
	}
}

// heal takes the next step for a dead page: a reload, at most once until the
// page answers again or the kiosk restarts; then a restart, at most once per
// backoff gap. A page that recovers each time it is reloaded is reloaded each
// time it dies, which is what it needs; a page that never recovers gets one
// reload and one restart per gap.
func (h *healer) heal(ctx context.Context, cause error, url string) {
	if !h.reloaded {
		h.fails, h.reloaded = 0, true
		h.log.Warn("kiosk page is dead; reloading it", "err", cause, "url", url)
		cctx, cancel := context.WithTimeout(ctx, kioskProbeTimeout)
		if err := h.reload(cctx); err != nil {
			h.log.Warn("could not reload the kiosk page", "err", err)
		}
		cancel()
		return
	}

	now := h.now()
	if now.Before(h.nextRestart) {
		if !h.waitLogged {
			h.log.Error("kiosk page is still dead; waiting before restarting the kiosk again so a broken page cannot restart it in a loop",
				"err", cause, "url", url, "nextAttempt", h.nextRestart.Format(time.RFC3339))
			h.waitLogged = true
		}
		return
	}

	h.fails, h.reloaded, h.waitLogged = 0, false, false
	gap := min(kioskRestartMinGap<<min(h.restarts, 8), kioskRestartMaxGap)
	h.restarts++
	h.nextRestart = now.Add(gap)
	h.log.Error("kiosk page is still dead after a reload; restarting the kiosk",
		"err", cause, "url", url, "restarts", h.restarts, "nextAllowed", h.nextRestart.Format(time.RFC3339))
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	if err := h.restart(cctx); err != nil {
		h.log.Error("could not restart the kiosk", "err", err)
	}
	cancel()
}

// Restart restarts the kiosk unit, through the sudoers allow-list
// (packaging/sudoers.d/musallahboard-agent). The healer uses it, and so does
// the kiosk.restart remote command.
func Restart(ctx context.Context) error {
	out, err := exec.CommandContext(ctx, "sudo", "-n", "/bin/systemctl", "restart", "musallahboard-kiosk.service").CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemctl: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}
