// Package splash pushes live device information into the local "waiting for
// enrollment" page the kiosk shows before a device has a config.
//
// The splash is loaded from file:///usr/share/musallahboard/waiting.html. A
// file:// page has an opaque origin and cannot fetch anything — not the
// network, not another local file — so it has no way to discover the address
// of the machine it is running on. The agent has to push, and the channel it
// already owns is CDP Runtime.evaluate against the kiosk Chromium on :9222.
// This is the same trick config.refresh uses to repair an already-loaded
// board; here it feeds a page that has no network of its own.
//
// The page contract (see packaging/waiting.html):
//
//	window.MusallahBoard.setNetInfo({ipv4, ssid, hostname, agentVersion}) → renders, no return
//
// Pushes are idempotent and stateless: the caller re-sends on a timer, so a
// kiosk restart (which wipes the injected values) self-heals on the next tick
// with no coordination between the two units.
package splash

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/LensBridge/agent/internal/cdp"
	"github.com/LensBridge/agent/internal/netinfo"
)

// Evaluator is the slice of *cdp.Client this package needs.
type Evaluator interface {
	EvaluateValue(ctx context.Context, expression string, out any) error
}

// ErrNoSplash means a page answered but is not the enrollment splash — the
// real board is loaded, or Chromium is still on about:blank. Callers polling
// on a timer should treat it as an ordinary skip, not a failure.
var ErrNoSplash = errors.New("splash: page does not expose window.MusallahBoard.setNetInfo")

// pushTimeout bounds one CDP round trip. Chromium is on loopback, so this is
// generous; the point is that a wedged browser can never stall the
// pre-enrollment loop that calls us.
const pushTimeout = 5 * time.Second

// netInfoScript builds the expression evaluated in the page. json.Marshal
// rather than string concatenation: hostname and SSID are attacker-adjacent
// (a hostile AP picks the SSID) and must not be able to break out of the
// literal and run as code in the kiosk browser.
func netInfoScript(info netinfo.Info, agentVersion string) (string, error) {
	payload, err := json.Marshal(struct {
		netinfo.Info
		AgentVersion string `json:"agentVersion,omitempty"`
	}{info, agentVersion})
	if err != nil {
		return "", fmt.Errorf("splash: marshal net info: %w", err)
	}
	return `(() => {
  const mb = window.MusallahBoard;
  if (!mb || typeof mb.setNetInfo !== 'function') return false;
  mb.setNetInfo(` + string(payload) + `);
  return true;
})()`, nil
}

// PushNetInfo renders info, and the running agent's version, onto the
// enrollment splash.
//
// Returns ErrNoSplash when the current page has no setNetInfo hook, and a
// wrapped transport error when Chromium is unreachable (not yet started, or
// mid-restart) — both are expected states while a board waits to be enrolled.
func PushNetInfo(ctx context.Context, ev Evaluator, info netinfo.Info, agentVersion string) error {
	script, err := netInfoScript(info, agentVersion)
	if err != nil {
		return err
	}

	cctx, cancel := context.WithTimeout(ctx, pushTimeout)
	defer cancel()

	var applied bool
	// ErrEvalUndefined would mean the expression itself did not resolve to a
	// value, which our IIFE cannot do — but a page that failed to parse the
	// script at all lands here, and "not the splash" is the right reading.
	if err := ev.EvaluateValue(cctx, script, &applied); err != nil {
		if errors.Is(err, cdp.ErrEvalUndefined) {
			return ErrNoSplash
		}
		return err
	}
	if !applied {
		return ErrNoSplash
	}
	return nil
}
