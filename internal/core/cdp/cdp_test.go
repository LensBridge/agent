package cdp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// fakeChrome is a debugger endpoint with one page target. Each incoming CDP
// request is answered by reply; a nil frame list means "never answer" (a
// hung renderer).
type fakeChrome struct {
	t       *testing.T
	srv     *httptest.Server
	url     string // the page target's URL as /json lists it
	noPages bool
	reply   func(req rpcRequest) []any

	mu      sync.Mutex
	methods []string
}

func newFakeChrome(t *testing.T, reply func(req rpcRequest) []any) *fakeChrome {
	t.Helper()
	f := &fakeChrome{t: t, url: "http://127.0.0.1:8080/", reply: reply}
	mux := http.NewServeMux()
	mux.HandleFunc("/json", func(w http.ResponseWriter, r *http.Request) {
		targets := []Target{{ID: "devtools", Type: "other", URL: "devtools://x"}}
		if !f.noPages {
			ws := "ws" + strings.TrimPrefix(f.srv.URL, "http") + "/devtools/page/P1"
			targets = append(targets, Target{ID: "P1", Type: "page", URL: f.url, WebSocketDebuggerURL: ws})
		}
		_ = json.NewEncoder(w).Encode(targets)
	})
	mux.HandleFunc("/devtools/page/P1", func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		ctx := r.Context()
		for {
			_, msg, err := conn.Read(ctx)
			if err != nil {
				return
			}
			var req rpcRequest
			if err := json.Unmarshal(msg, &req); err != nil {
				return
			}
			f.mu.Lock()
			f.methods = append(f.methods, req.Method)
			f.mu.Unlock()
			for _, frame := range f.reply(req) {
				data, _ := json.Marshal(frame)
				if err := conn.Write(ctx, websocket.MessageText, data); err != nil {
					return
				}
			}
		}
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeChrome) client() *Client { return New(f.srv.URL) }

func (f *fakeChrome) seen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.methods...)
}

func result(id int64, v any) map[string]any { return map[string]any{"id": id, "result": v} }

func rpcErr(id int64, code int, msg string) map[string]any {
	return map[string]any{"id": id, "error": map[string]any{"code": code, "message": msg}}
}

func evalValue(v any) map[string]any {
	return map[string]any{"result": map[string]any{"type": "object", "value": v}}
}

// healthy answers everything; Runtime.evaluate with value.
func healthy(value any) func(rpcRequest) []any {
	return func(req rpcRequest) []any {
		if req.Method == "Runtime.evaluate" {
			if value == nil {
				return []any{result(req.ID, map[string]any{"result": map[string]any{"type": "undefined"}})}
			}
			return []any{result(req.ID, evalValue(value))}
		}
		return []any{result(req.ID, map[string]any{})}
	}
}

func probeCtx(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	t.Cleanup(cancel)
	return ctx
}

func TestPageStatusBoardAnswers(t *testing.T) {
	f := newFakeChrome(t, healthy(map[string]any{"paired": true, "slideKey": "poster-3"}))

	st, err := f.client().PageStatus(probeCtx(t))
	if err != nil {
		t.Fatalf("PageStatus: %v", err)
	}
	if st.SlideKey != "poster-3" || !st.Paired {
		t.Errorf("status = %+v", st)
	}
	if got := f.seen(); len(got) != 2 || got[0] != "Inspector.enable" || got[1] != "Runtime.evaluate" {
		t.Errorf("methods = %v, want Inspector.enable then Runtime.evaluate", got)
	}
}

// The splash, the update screen and old board apps have no getStatus().
func TestPageStatusNoHook(t *testing.T) {
	f := newFakeChrome(t, healthy(nil))
	if _, err := f.client().PageStatus(probeCtx(t)); !errors.Is(err, ErrEvalUndefined) {
		t.Fatalf("err = %v, want ErrEvalUndefined", err)
	}
}

// Chromium answers Inspector.enable on a tab whose renderer is gone with an
// Inspector.targetCrashed event, and never answers Runtime.evaluate.
func TestPageStatusCrashedRenderer(t *testing.T) {
	f := newFakeChrome(t, func(req rpcRequest) []any {
		if req.Method == "Inspector.enable" {
			return []any{map[string]any{"method": "Inspector.targetCrashed", "params": map[string]any{}}, result(req.ID, map[string]any{})}
		}
		return nil
	})
	start := time.Now()
	_, err := f.client().PageStatus(probeCtx(t))
	if !errors.Is(err, ErrPageCrashed) {
		t.Fatalf("err = %v, want ErrPageCrashed", err)
	}
	if time.Since(start) > 400*time.Millisecond {
		t.Errorf("a crash should be reported at once, took %v", time.Since(start))
	}
}

// Hung JavaScript: the browser takes the connection, the renderer never
// replies.
func TestPageStatusHungRenderer(t *testing.T) {
	f := newFakeChrome(t, func(req rpcRequest) []any {
		if req.Method == "Inspector.enable" {
			return []any{result(req.ID, map[string]any{})}
		}
		return nil
	})
	_, err := f.client().PageStatus(probeCtx(t))
	if !errors.Is(err, ErrPageUnresponsive) {
		t.Fatalf("err = %v, want ErrPageUnresponsive", err)
	}
	if errors.Is(err, ErrDebuggerUnreachable) {
		t.Errorf("a hung page must not read as an unreachable debugger: %v", err)
	}
}

func TestPageStatusNavigating(t *testing.T) {
	f := newFakeChrome(t, func(req rpcRequest) []any {
		if req.Method == "Runtime.evaluate" {
			return []any{rpcErr(req.ID, -32000, "Execution context was destroyed.")}
		}
		return []any{result(req.ID, map[string]any{})}
	})
	if _, err := f.client().PageStatus(probeCtx(t)); !errors.Is(err, ErrPageNavigating) {
		t.Fatalf("err = %v, want ErrPageNavigating", err)
	}
}

func TestPageStatusThrew(t *testing.T) {
	f := newFakeChrome(t, func(req rpcRequest) []any {
		if req.Method == "Runtime.evaluate" {
			return []any{result(req.ID, map[string]any{
				"result":           map[string]any{"type": "object"},
				"exceptionDetails": map[string]any{"text": "Uncaught", "exception": map[string]any{"description": "TypeError: boom"}},
			})}
		}
		return []any{result(req.ID, map[string]any{})}
	})
	_, err := f.client().PageStatus(probeCtx(t))
	if !errors.Is(err, ErrPageThrew) || !strings.Contains(err.Error(), "TypeError: boom") {
		t.Fatalf("err = %v, want ErrPageThrew with the description", err)
	}
}

func TestPageStatusNoPageTarget(t *testing.T) {
	f := newFakeChrome(t, healthy(true))
	f.noPages = true
	if _, err := f.client().PageStatus(probeCtx(t)); !errors.Is(err, ErrNoPageTarget) {
		t.Fatalf("err = %v, want ErrNoPageTarget", err)
	}
}

func TestPageStatusDebuggerUnreachable(t *testing.T) {
	f := newFakeChrome(t, healthy(true))
	c := f.client()
	f.srv.Close()
	_, err := c.PageStatus(probeCtx(t))
	if !errors.Is(err, ErrDebuggerUnreachable) {
		t.Fatalf("err = %v, want ErrDebuggerUnreachable", err)
	}
	if !strings.Contains(err.Error(), "chrome not reachable") {
		t.Errorf("message changed: %v", err)
	}
}

func TestPageHealth(t *testing.T) {
	f := newFakeChrome(t, healthy(true))
	if err := f.client().PageHealth(probeCtx(t)); err != nil {
		t.Fatalf("PageHealth: %v", err)
	}
}

// Call keeps returning CDP error replies as errors, now typed.
func TestCallRPCError(t *testing.T) {
	f := newFakeChrome(t, func(req rpcRequest) []any {
		return []any{rpcErr(req.ID, -32601, "'Page.bogus' wasn't found")}
	})
	c := f.client()
	target, err := c.FirstPageTarget(probeCtx(t))
	if err != nil {
		t.Fatal(err)
	}
	err = c.Call(probeCtx(t), target, "Page.bogus", nil, nil)
	var rerr *RPCError
	if !errors.As(err, &rerr) || rerr.Code != -32601 || rerr.Method != "Page.bogus" {
		t.Fatalf("err = %v, want an RPCError for Page.bogus", err)
	}
}

func TestPageReloadSendsReload(t *testing.T) {
	f := newFakeChrome(t, healthy(true))
	if err := f.client().PageReload(probeCtx(t)); err != nil {
		t.Fatalf("PageReload: %v", err)
	}
	if got := f.seen(); len(got) != 1 || got[0] != "Page.reload" {
		t.Errorf("methods = %v", got)
	}
}
