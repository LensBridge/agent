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
	"errors"
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

// maxReplyBytes caps a single CDP reply. The library default is 32 KiB, which
// is fine for Page.reload and Runtime.evaluate but nowhere near a screenshot:
// Page.captureScreenshot returns the whole PNG base64-encoded in one text
// frame, so a 1080p board came back as
// "read: websocket: message too big: read limited at 32769 bytes".
// Chromium is on loopback and we asked for the payload, so the limit exists
// only to stop a wedged browser from exhausting memory.
const maxReplyBytes = 32 << 20 // 32 MiB

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
	conn.SetReadLimit(maxReplyBytes)

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

// PageStatus is what the board page reports about itself, via the
// `window.MusallahBoard.getStatus()` hook the frontend installs.
type PageStatus struct {
	DeviceID      string `json:"deviceId"`
	Paired        bool   `json:"paired"`
	SlideKey      string `json:"slideKey"`
	SlideIndex    *int   `json:"slideIndex"`
	SlideCount    int    `json:"slideCount"`
	LastPayloadAt string `json:"lastPayloadAt"`
	Error         string `json:"error"`
}

// PageStatus asks the board what it is currently showing.
//
// Three outcomes worth distinguishing:
//   - nil error: the board is up and answered.
//   - ErrEvalUndefined: a browser page exists but has no hook — the local
//     enrollment splash, or a frontend build older than getStatus(). The
//     browser is alive; it just isn't the board.
//   - any other error: Chromium is unreachable.
func (c *Client) PageStatus(ctx context.Context) (PageStatus, error) {
	var s PageStatus
	err := c.EvaluateValue(ctx, "window.MusallahBoard?.getStatus?.() ?? null", &s)
	return s, err
}

// ErrEvalUndefined means the expression resolved to `undefined`. Usually an
// optional call short-circuiting because the page doesn't implement the hook
// (an old frontend build, or the local enrollment splash rather than the
// board). Callers decide whether that's fatal.
var ErrEvalUndefined = errors.New("cdp: expression evaluated to undefined")

// evalReply is the slice of Runtime.evaluate's response we care about.
type evalReply struct {
	Result struct {
		Type  string          `json:"type"`
		Value json.RawMessage `json:"value"`
	} `json:"result"`
	ExceptionDetails *struct {
		Text      string `json:"text"`
		Exception *struct {
			Description string `json:"description"`
		} `json:"exception"`
	} `json:"exceptionDetails"`
}

// EvaluateValue runs expression and unmarshals its resolved value into out.
//
// Unlike RuntimeEvaluate it surfaces page-side failures as Go errors: a thrown
// exception becomes an error carrying the JS description, and a result of
// `undefined` becomes ErrEvalUndefined. Pass a nil out to evaluate for effect
// only. Promises are awaited (RuntimeEvaluate sets awaitPromise).
func (c *Client) EvaluateValue(ctx context.Context, expression string, out any) error {
	raw, err := c.RuntimeEvaluate(ctx, expression)
	if err != nil {
		return err
	}
	var reply evalReply
	if err := json.Unmarshal(raw, &reply); err != nil {
		return fmt.Errorf("cdp: decode evaluate reply: %w", err)
	}
	if reply.ExceptionDetails != nil {
		msg := reply.ExceptionDetails.Text
		if reply.ExceptionDetails.Exception != nil && reply.ExceptionDetails.Exception.Description != "" {
			msg = reply.ExceptionDetails.Exception.Description
		}
		return fmt.Errorf("cdp: page threw: %s", msg)
	}
	if reply.Result.Type == "undefined" || len(reply.Result.Value) == 0 || string(reply.Result.Value) == "null" {
		return ErrEvalUndefined
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(reply.Result.Value, out); err != nil {
		return fmt.Errorf("cdp: decode evaluate value: %w", err)
	}
	return nil
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
