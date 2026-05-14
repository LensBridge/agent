package commands

import (
	"context"
	"encoding/json"

	"github.com/utmmsa/musallahboard-agent/internal/cdp"
)

// ChromeReload triggers a hard reload of the kiosk page via CDP.
type ChromeReload struct {
	CDP *cdp.Client
}

func (h *ChromeReload) Kind() string { return "chrome.reload" }

func (h *ChromeReload) Execute(ctx context.Context, _ json.RawMessage, progress ProgressFn) (any, error) {
	progress("reloading", "issuing Page.reload via CDP", nil)
	if err := h.CDP.PageReload(ctx); err != nil {
		return nil, err
	}
	return map[string]any{"reloaded": true}, nil
}
