// Package cdp is a minimal Chrome DevTools Protocol client for the three
// methods the agent actually uses against the kiosk Chromium instance:
// Page.reload, Page.captureScreenshot, Runtime.evaluate.
//
// We don't pull in chromedp because we don't drive a browser — we attach to
// an already-running Chromium with --remote-debugging-port=9222 and only
// need a thin JSON-RPC over WS shim.
package cdp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
)

// DefaultDebuggerURL is the default localhost JSON discovery endpoint.
const DefaultDebuggerURL = "http://localhost:9222"

// Target is one /json tab entry. We only care about webSocketDebuggerUrl
// and Type (which filters out DevTools UI tabs).
type Target struct {
	ID                   string `json:"id"`
	Type                 string `json:"type"`
	URL                  string `json:"url"`
	Title                string `json:"title"`
	WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
}

// Client is a per-call connector. Each method dials a fresh WS to the picked
// target, runs one JSON-RPC round trip, and closes — Chromium tolerates this
// well and we avoid the bookkeeping of a long-lived multiplexed connection.
type Client struct {
	debuggerURL string
	http        *http.Client

	id atomic.Int64 // monotonic JSON-RPC request id
}

// New returns a Client that talks to the given debugger HTTP endpoint
// (e.g. "http://localhost:9222"). Pass "" to use DefaultDebuggerURL.
func New(debuggerURL string) *Client {
	if debuggerURL == "" {
		debuggerURL = DefaultDebuggerURL
	}
	return &Client{
		debuggerURL: strings.TrimRight(debuggerURL, "/"),
		http:        &http.Client{Timeout: 5 * time.Second},
	}
}

// FirstPageTarget returns the WS URL of the first non-DevTools page tab.
// Errors if Chromium isn't reachable or has no pages open.
func (c *Client) FirstPageTarget(ctx context.Context) (Target, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.debuggerURL+"/json", nil)
	if err != nil {
		return Target{}, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return Target{}, fmt.Errorf("chrome not reachable at %s: %w", c.debuggerURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return Target{}, fmt.Errorf("chrome /json returned %d: %s", resp.StatusCode, body)
	}
	var targets []Target
	if err := json.NewDecoder(resp.Body).Decode(&targets); err != nil {
		return Target{}, fmt.Errorf("decode targets: %w", err)
	}
	for _, t := range targets {
		if t.Type == "page" && t.WebSocketDebuggerURL != "" {
			return t, nil
		}
	}
	return Target{}, fmt.Errorf("no page targets in chrome (%d total)", len(targets))
}

type rpcRequest struct {
	ID     int64  `json:"id"`
	Method string `json:"method"`
	Params any    `json:"params,omitempty"`
}

type rpcResponse struct {
	ID     int64           `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// Call sends one JSON-RPC request and decodes the response into out (may be nil).
// Errors include network failures, CDP error replies, and JSON decode errors.
func (c *Client) Call(ctx context.Context, target Target, method string, params, out any) error {
	conn, _, err := websocket.Dial(ctx, target.WebSocketDebuggerURL, nil)
	if err != nil {
		return fmt.Errorf("ws dial cdp: %w", err)
	}
	defer conn.CloseNow()

	id := c.id.Add(1)
	req := rpcRequest{ID: id, Method: method, Params: params}
	data, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	if err := conn.Write(ctx, websocket.MessageText, data); err != nil {
		return fmt.Errorf("write: %w", err)
	}

	// Chromium emits domain events on the same socket; ignore everything that
	// isn't a reply to our id.
	for {
		_, msg, err := conn.Read(ctx)
		if err != nil {
			return fmt.Errorf("read: %w", err)
		}
		var resp rpcResponse
		if err := json.Unmarshal(msg, &resp); err != nil {
			return fmt.Errorf("decode response: %w", err)
		}
		if resp.ID != id {
			continue
		}
		if resp.Error != nil {
			return fmt.Errorf("cdp %s: %s (code %d)", method, resp.Error.Message, resp.Error.Code)
		}
		if out != nil && len(resp.Result) > 0 {
			if err := json.Unmarshal(resp.Result, out); err != nil {
				return fmt.Errorf("decode result: %w", err)
			}
		}
		return nil
	}
}

// PageReload triggers Page.reload on the first page target. ignoreCache=true
// forces a hard reload (bypasses HTTP cache).
func (c *Client) PageReload(ctx context.Context) error {
	target, err := c.FirstPageTarget(ctx)
	if err != nil {
		return err
	}
	return c.Call(ctx, target, "Page.reload", map[string]any{"ignoreCache": true}, nil)
}

// PageCaptureScreenshot returns a base64-encoded PNG of the first page target's
// current viewport.
func (c *Client) PageCaptureScreenshot(ctx context.Context) (base64PNG string, err error) {
	target, err := c.FirstPageTarget(ctx)
	if err != nil {
		return "", err
	}
	var out struct {
		Data string `json:"data"`
	}
	if err := c.Call(ctx, target, "Page.captureScreenshot", map[string]any{"format": "png"}, &out); err != nil {
		return "", err
	}
	return out.Data, nil
}

// RuntimeEvaluate runs the given JS expression in the first page target and
// returns whatever the protocol replies with as the result object.
func (c *Client) RuntimeEvaluate(ctx context.Context, expression string) (json.RawMessage, error) {
	target, err := c.FirstPageTarget(ctx)
	if err != nil {
		return nil, err
	}
	var out json.RawMessage
	if err := c.Call(ctx, target, "Runtime.evaluate", map[string]any{
		"expression":   expression,
		"awaitPromise": true,
		"returnByValue": true,
	}, &out); err != nil {
		return nil, err
	}
	return out, nil
}
