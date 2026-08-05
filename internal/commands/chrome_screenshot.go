package commands

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/LensBridge/agent/internal/cdp"
)

// maxResultBase64 keeps the result frame under the backend's 10 MB WebSocket
// text buffer (WebSocketConfig.setMaxTextMessageBufferSize). Overshooting it
// doesn't fail the command — it kills the whole session with no explanation on
// either side — so refuse locally with something readable instead.
const maxResultBase64 = 8 << 20 // 8 MiB

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
	if len(data) > maxResultBase64 {
		return nil, fmt.Errorf("screenshot too large to return: %d bytes of base64 (limit %d)", len(data), maxResultBase64)
	}
	return map[string]any{
		"format": "png",
		"base64": data,
	}, nil
}
