// Package kiosk is everything in the agent that touches the kiosk browser:
// a minimal Chrome DevTools Protocol client (this file), the update screen
// driven over it (screen.go), and the watcher that heals a crashed or hung
// page (heal.go).
//
// The CDP client covers the few methods the agent uses against the kiosk
// Chromium instance: Page.reload, Page.navigate, Page.captureScreenshot,
// Runtime.evaluate, and Inspector.enable to tell a crashed renderer from a
// live one. We don't pull in chromedp because we don't drive a browser — we
// attach to an already-running Chromium with --remote-debugging-port=9222 and
// only need a thin JSON-RPC over WS shim.
package kiosk

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

// NewClient returns a Client that talks to the given debugger HTTP endpoint
// (e.g. "http://localhost:9222"). Pass "" to use DefaultDebuggerURL.
func NewClient(debuggerURL string) *Client {
	if debuggerURL == "" {
		debuggerURL = DefaultDebuggerURL
	}
	return &Client{
		debuggerURL: strings.TrimRight(debuggerURL, "/"),
		http:        &http.Client{Timeout: 5 * time.Second},
	}
}

// Errors that say how far a probe of the kiosk got. They separate "the
// browser is not there (yet)", which only the kiosk unit's state can answer,
// from "the browser is there but its page is dead", which is a broken board.
var (
	// ErrDebuggerUnreachable: nothing answers on the debugging port, or it
	// answers garbage. Chromium is starting, restarting, or was built without
	// --remote-debugging-port.
	ErrDebuggerUnreachable = errors.New("chrome not reachable")
	// ErrNoPageTarget: the browser answers but has no page open, which is
	// normal for a moment while it starts.
	ErrNoPageTarget = errors.New("no page targets in chrome")
	// ErrPageCrashed: the page's renderer is gone ("Aw, Snap!"). The browser
	// keeps the tab, and its URL, so /json alone does not show it.
	ErrPageCrashed = errors.New("cdp: page renderer crashed")
	// ErrPageUnresponsive: the browser accepted a debugger connection to the
	// page but the page did not answer in time (hung JavaScript, a wedged or
	// crashed renderer).
	ErrPageUnresponsive = errors.New("cdp: page is not answering")
	// ErrPageNavigating: the page's JavaScript context went away under the
	// call, because the page is navigating or reloading. Transient.
	ErrPageNavigating = errors.New("cdp: page is navigating")
	// ErrPageThrew: the expression threw. The page is alive and answered.
	ErrPageThrew = errors.New("cdp: page threw")
)

// RPCError is an error reply from Chromium to one CDP method call.
type RPCError struct {
	Method  string
	Code    int
	Message string
}

func (e *RPCError) Error() string {
	return fmt.Sprintf("cdp %s: %s (code %d)", e.Method, e.Message, e.Code)
}

// navigatingMessages are the error replies Chromium gives a Runtime call
// whose execution context was torn down by a navigation or reload.
var navigatingMessages = []string{
	"Execution context was destroyed",
	"Cannot find default execution context",
	"Cannot find context with specified id",
	"Inspected target navigated or closed",
}

func isNavigatingError(e *RPCError) bool {
	for _, m := range navigatingMessages {
		if strings.Contains(e.Message, m) {
			return true
		}
	}
	return false
}

// FirstPageTarget returns the WS URL of the first non-DevTools page tab.
// Errors if Chromium isn't reachable (ErrDebuggerUnreachable) or has no pages
// open (ErrNoPageTarget).
func (c *Client) FirstPageTarget(ctx context.Context) (Target, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.debuggerURL+"/json", nil)
	if err != nil {
		return Target{}, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return Target{}, fmt.Errorf("%w at %s: %w", ErrDebuggerUnreachable, c.debuggerURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return Target{}, fmt.Errorf("%w: /json returned %d: %s", ErrDebuggerUnreachable, resp.StatusCode, body)
	}
	var targets []Target
	if err := json.NewDecoder(resp.Body).Decode(&targets); err != nil {
		return Target{}, fmt.Errorf("%w: decode targets: %w", ErrDebuggerUnreachable, err)
	}
	for _, t := range targets {
		if t.Type == "page" && t.WebSocketDebuggerURL != "" {
			return t, nil
		}
	}
	return Target{}, fmt.Errorf("%w (%d total)", ErrNoPageTarget, len(targets))
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
			return &RPCError{Method: method, Code: resp.Error.Code, Message: resp.Error.Message}
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

// PageStatus asks the board what it is currently showing. It probes the page
// the way ProbeEvaluate does, so the error says what state the kiosk is in:
//   - nil: the board is up and answered.
//   - ErrEvalUndefined: a page answered but has no hook — the local
//     setup screen, the update screen, or a frontend build older than
//     getStatus(). The page is alive; it just isn't the board.
//   - ErrPageThrew, ErrPageNavigating: the page is alive.
//   - ErrPageCrashed, ErrPageUnresponsive: the browser is up and the page is
//     dead. The board is not on screen.
//   - ErrDebuggerUnreachable, ErrNoPageTarget: nothing to ask; the browser
//     is not (yet) there.
func (c *Client) PageStatus(ctx context.Context) (PageStatus, error) {
	var s PageStatus
	raw, err := c.ProbeEvaluate(ctx, "window.MusallahBoard?.getStatus?.() ?? null")
	if err != nil {
		return s, err
	}
	err = decodeEvalReply(raw, &s)
	return s, err
}

// PageHealth reports whether the kiosk page's renderer answers, with the
// same errors as PageStatus. A nil error means the page ran a trivial script.
func (c *Client) PageHealth(ctx context.Context) error {
	raw, err := c.ProbeEvaluate(ctx, "true")
	if err != nil {
		return err
	}
	return decodeEvalReply(raw, nil)
}

// ProbeEvaluate runs expression in the first page target like RuntimeEvaluate,
// but classifies how a failure happened instead of returning a bare network
// error:
//
//   - the debugger cannot be reached or the WebSocket to the page cannot be
//     opened: ErrDebuggerUnreachable (or ErrNoPageTarget);
//   - Chromium reports the renderer crashed (Inspector.targetCrashed, which
//     Inspector.enable sends at once on a tab whose renderer is gone):
//     ErrPageCrashed;
//   - the connection is open but no reply arrives before ctx's deadline:
//     ErrPageUnresponsive. The Inspector domain is answered by the browser
//     process, Runtime by the renderer, so a hung or dead renderer shows up
//     here;
//   - the call is cut off by a navigation: ErrPageNavigating.
//
// The caller's deadline is the evaluate timeout; it should leave the browser
// a few seconds (the board probes with 3-5 s on loopback).
func (c *Client) ProbeEvaluate(ctx context.Context, expression string) (json.RawMessage, error) {
	target, err := c.FirstPageTarget(ctx)
	if err != nil {
		return nil, err
	}
	conn, _, err := websocket.Dial(ctx, target.WebSocketDebuggerURL, nil)
	if err != nil {
		// Not even a connection: the browser is going away or not there yet,
		// which says nothing about the page.
		return nil, fmt.Errorf("%w: ws dial cdp: %w", ErrDebuggerUnreachable, err)
	}
	defer conn.CloseNow()
	conn.SetReadLimit(maxReplyBytes)

	enableID := c.id.Add(1)
	evalID := c.id.Add(1)
	for _, req := range []rpcRequest{
		{ID: enableID, Method: "Inspector.enable"},
		{ID: evalID, Method: "Runtime.evaluate", Params: map[string]any{
			"expression":    expression,
			"awaitPromise":  true,
			"returnByValue": true,
		}},
	} {
		data, err := json.Marshal(req)
		if err != nil {
			return nil, fmt.Errorf("marshal: %w", err)
		}
		if err := conn.Write(ctx, websocket.MessageText, data); err != nil {
			return nil, probeReadError(ctx, "write", err)
		}
	}

	for {
		_, msg, err := conn.Read(ctx)
		if err != nil {
			return nil, probeReadError(ctx, "read", err)
		}
		var frame struct {
			rpcResponse
			Method string `json:"method"`
		}
		if err := json.Unmarshal(msg, &frame); err != nil {
			return nil, fmt.Errorf("decode response: %w", err)
		}
		switch {
		case frame.Method == "Inspector.targetCrashed":
			return nil, fmt.Errorf("%w (%s)", ErrPageCrashed, target.URL)
		case frame.Method != "" || frame.ID != evalID:
			// Other events, and the reply to Inspector.enable.
			continue
		case frame.Error != nil:
			rerr := &RPCError{Method: "Runtime.evaluate", Code: frame.Error.Code, Message: frame.Error.Message}
			if isNavigatingError(rerr) {
				return nil, fmt.Errorf("%w: %w", ErrPageNavigating, rerr)
			}
			if strings.Contains(strings.ToLower(rerr.Message), "crashed") {
				return nil, fmt.Errorf("%w: %w", ErrPageCrashed, rerr)
			}
			return nil, rerr
		default:
			return frame.Result, nil
		}
	}
}

// probeReadError classifies a failure on an open debugger connection. Once
// the browser has accepted the connection, running out of time means the page
// did not answer; the connection dropping means the browser went away.
func probeReadError(ctx context.Context, op string, err error) error {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("%w: %s: %w", ErrPageUnresponsive, op, err)
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return fmt.Errorf("%w: %s: %w", ErrDebuggerUnreachable, op, err)
}

// ErrEvalUndefined means the expression resolved to `undefined`. Usually an
// optional call short-circuiting because the page doesn't implement the hook
// (an old frontend build, or the local setup screen rather than the
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
	return decodeEvalReply(raw, out)
}

// decodeEvalReply turns a Runtime.evaluate result into out, ErrPageThrew or
// ErrEvalUndefined.
func decodeEvalReply(raw json.RawMessage, out any) error {
	var reply evalReply
	if err := json.Unmarshal(raw, &reply); err != nil {
		return fmt.Errorf("cdp: decode evaluate reply: %w", err)
	}
	if reply.ExceptionDetails != nil {
		msg := reply.ExceptionDetails.Text
		if reply.ExceptionDetails.Exception != nil && reply.ExceptionDetails.Exception.Description != "" {
			msg = reply.ExceptionDetails.Exception.Description
		}
		return fmt.Errorf("%w: %s", ErrPageThrew, msg)
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
		"expression":    expression,
		"awaitPromise":  true,
		"returnByValue": true,
	}, &out); err != nil {
		return nil, err
	}
	return out, nil
}
