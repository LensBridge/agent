package commands

import (
	"context"
	"encoding/json"

	"github.com/LensBridge/agent/internal/cdp"
)

// ChromeScreenshot returns a base64-encoded PNG of the current kiosk frame.
//
// Output shape: {"format":"png","base64":"..."} — backend may later move the
// blob into object storage and rewrite output to {"url":"..."}, but the
// agent always returns inline base64.
type ChromeScreenshot struct {
	CDP *cdp.Client
}

func (h *ChromeScreenshot) Kind() string { return "chrome.screenshot" }

func (h *ChromeScreenshot) Execute(ctx context.Context, _ json.RawMessage, progress ProgressFn) (any, error) {
	progress("capturing", "Page.captureScreenshot", nil)
	data, err := h.CDP.PageCaptureScreenshot(ctx)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"format": "png",
		"base64": data,
	}, nil
}
