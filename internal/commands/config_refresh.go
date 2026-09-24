package commands

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/LensBridge/agent/internal/cdp"
	"github.com/LensBridge/agent/internal/kioskurl"
)

// ConfigRefresh pulls fresh content and repaints the board in place, with no
// reload, so the screen never flashes. It re-asserts the on-disk kiosk URL
// (the local server), asks the content syncer for a sync, and asks the live
// page to re-fetch its payload.
//
// The page contract is `window.MusallahBoard.refresh()` (see the frontend's
// src/App.jsx), which resolves to {ok, deviceId, at | error}. The page learns
// its device id from the agent's /api/local/status, so nothing is pushed into
// it any more.
type ConfigRefresh struct {
	CDP *cdp.Client
	// Before, when set, runs first. The daemon passes the content syncer's
	// Trigger: the page only shows what the agent has installed, so a refresh
	// from the admin portal should also pull new content.
	Before func()
}

func (h *ConfigRefresh) Kind() string { return "config.refresh" }

// refreshScript is evaluated in the page.
const refreshScript = `(async () => {
  const mb = window.MusallahBoard;
  if (!mb || typeof mb.refresh !== 'function') return { hook: false };
  const r = await mb.refresh();
  return Object.assign({ hook: true }, r);
})()`

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
	if h.Before != nil {
		h.Before()
	}
	if err := kioskurl.WriteLocal(kioskurl.DefaultOutPath); err != nil {
		progress("warn", "kiosk url not rewritten: "+err.Error(), nil)
	}

	progress("evaluating", "window.MusallahBoard.refresh()", nil)

	var res pageResult
	err := h.CDP.EvaluateValue(ctx, refreshScript, &res)
	// A short-circuited optional call yields `undefined`. Reporting that as a
	// success is how this command once spent its life lying to the admin
	// portal: the board sat on stale content while the dashboard said
	// SUCCEEDED.
	if errors.Is(err, cdp.ErrEvalUndefined) {
		return nil, fmt.Errorf("kiosk page returned nothing: it is not the board (splash or update screen?)")
	}
	if err != nil {
		return nil, err
	}
	if !res.Hook {
		return nil, fmt.Errorf("page does not expose window.MusallahBoard.refresh()")
	}
	if !res.OK {
		return nil, fmt.Errorf("board refresh failed: %s", firstNonEmpty(res.Error, res.Reason, "unknown"))
	}
	return map[string]any{"deviceId": res.DeviceID, "refreshed": res.At}, nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
