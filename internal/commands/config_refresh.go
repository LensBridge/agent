package commands

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/LensBridge/agent/internal/cdp"
	"github.com/LensBridge/agent/internal/kioskurl"
)

// ConfigRefresh re-composes the on-disk kiosk URL (so an operator's board-url
// change is picked up), re-asserts this device's identity in the live page,
// and asks it to re-fetch its board payload in place — no reload, so the
// screen never flashes.
//
// The page contract is `window.MusallahBoard` (see frontend src/App.jsx):
//
//	setDeviceId(uuid, {reload:false}) → persists the id, no navigation
//	refresh()                         → Promise<{ok, deviceId, at|error}>
//
// Re-asserting the id here is what repairs a board whose cookie was wiped or
// points at the wrong device: the agent is the only component that knows the
// enrolled identity, and this is the one command that can push it into a page
// that is already loaded.
type ConfigRefresh struct {
	CDP      *cdp.Client
	DeviceID string
}

func (h *ConfigRefresh) Kind() string { return "config.refresh" }

// refreshScript builds the expression evaluated in the page. Both steps run in
// a single round trip on purpose: split across two Runtime.evaluate calls, the
// refresh would race React's re-render and refetch with the pre-update id.
func refreshScript(deviceID string) string {
	// json.Marshal, not string concatenation — the id is interpolated into
	// live JS and must not be able to terminate the literal.
	id, _ := json.Marshal(deviceID)
	return `(async () => {
  const mb = window.MusallahBoard;
  if (!mb || typeof mb.refresh !== 'function') return { hook: false };
  const id = ` + string(id) + `;
  if (id) {
    try { mb.setDeviceId(id, { reload: false }); }
    catch (e) { return { hook: true, ok: false, error: 'setDeviceId: ' + (e && e.message || e) }; }
  }
  const r = await mb.refresh();
  return Object.assign({ hook: true }, r);
})()`
}

// pageResult is the shape refreshScript resolves to.
type pageResult struct {
	Hook     bool   `json:"hook"`
	OK       bool   `json:"ok"`
	Reason   string `json:"reason"`
	Error    string `json:"error"`
	DeviceID string `json:"deviceId"`
	At       string `json:"at"`
}

func (h *ConfigRefresh) Execute(ctx context.Context, _ json.RawMessage, progress ProgressFn) (any, error) {
	if h.DeviceID != "" {
		if err := kioskurl.Write(kioskurl.DefaultBoardURLPath, kioskurl.DefaultOutPath, h.DeviceID); err != nil {
			progress("warn", "kiosk url not rewritten: "+err.Error(), nil)
		}
	}

	progress("evaluating", "window.MusallahBoard.refresh()", nil)

	var res pageResult
	err := h.CDP.EvaluateValue(ctx, refreshScript(h.DeviceID), &res)
	// A short-circuited optional call yields `undefined`. Reporting that as a
	// success is how this command spent its life lying to the admin portal:
	// the board sat on stale content while the dashboard said SUCCEEDED.
	if errors.Is(err, cdp.ErrEvalUndefined) {
		return nil, fmt.Errorf("kiosk page returned nothing — it is not the board (splash?) or predates the refresh hook")
	}
	if err != nil {
		return nil, err
	}
	if !res.Hook {
		return nil, fmt.Errorf("page does not expose window.MusallahBoard.refresh() — frontend predates the refresh hook")
	}
	if !res.OK {
		detail := firstNonEmpty(res.Error, res.Reason, "unknown")
		return nil, fmt.Errorf("board refresh failed: %s", detail)
	}

	return map[string]any{
		"deviceId":  res.DeviceID,
		"refreshed": res.At,
	}, nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
