package commands

import (
	"context"
	"encoding/json"

	"github.com/utmmsa/musallahboard-agent/internal/cdp"
)

// ConfigRefresh asks the kiosk page to re-fetch its board payload in-place
// (no full reload). Relies on the page exposing a global window.__refreshBoard
// hook; if the function isn't defined the call is a no-op.
type ConfigRefresh struct {
	CDP *cdp.Client
}

func (h *ConfigRefresh) Kind() string { return "config.refresh" }

func (h *ConfigRefresh) Execute(ctx context.Context, _ json.RawMessage, progress ProgressFn) (any, error) {
	progress("evaluating", "Runtime.evaluate window.__refreshBoard()", nil)
	result, err := h.CDP.RuntimeEvaluate(ctx, "window.__refreshBoard?.()")
	if err != nil {
		return nil, err
	}
	return map[string]any{"cdpResult": json.RawMessage(result)}, nil
}
