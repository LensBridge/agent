package commands

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"

	"github.com/utmmsa/musallahboard-agent/internal/wsclient"
)

type fakeHandler struct {
	kind   string
	output any
	err    error
}

func (f *fakeHandler) Kind() string { return f.kind }
func (f *fakeHandler) Execute(_ context.Context, _ json.RawMessage, _ ProgressFn) (any, error) {
	return f.output, f.err
}

type fakeSender struct {
	mu        sync.Mutex
	acks      []string
	progress  []string
	results   []sentResult
}

type sentResult struct {
	commandID    string
	status       string
	output       any
	errorMessage string
	durationMs   int64
}

func (s *fakeSender) Ack(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.acks = append(s.acks, id)
	return nil
}
func (s *fakeSender) Progress(_ context.Context, id, stage, _ string, _ *int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.progress = append(s.progress, id+":"+stage)
	return nil
}
func (s *fakeSender) Result(_ context.Context, id, status string, output any, errMsg string, durMs int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.results = append(s.results, sentResult{id, status, output, errMsg, durMs})
	return nil
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestDispatcher_UnknownKindRejected(t *testing.T) {
	reg := NewRegistry()
	d := NewDispatcher(reg, discardLogger())
	send := &fakeSender{}

	d.Handle(context.Background(), send, wsclient.Command{ID: "c1", Kind: "not.real"})

	if len(send.acks) != 1 || send.acks[0] != "c1" {
		t.Fatalf("expected one ack for c1, got %v", send.acks)
	}
	if len(send.results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(send.results))
	}
	if got := send.results[0]; got.status != "rejected" {
		t.Fatalf("status = %q, want rejected", got.status)
	}
}

func TestDispatcher_HappyPath(t *testing.T) {
	reg := NewRegistry()
	reg.Register(&fakeHandler{kind: "test.ok", output: map[string]any{"done": true}})
	d := NewDispatcher(reg, discardLogger())
	send := &fakeSender{}

	d.Handle(context.Background(), send, wsclient.Command{ID: "c1", Kind: "test.ok"})

	if len(send.results) != 1 || send.results[0].status != "ok" {
		t.Fatalf("results = %+v", send.results)
	}
}

func TestDispatcher_HandlerErrorBecomesError(t *testing.T) {
	reg := NewRegistry()
	reg.Register(&fakeHandler{kind: "test.fail", err: errors.New("boom")})
	d := NewDispatcher(reg, discardLogger())
	send := &fakeSender{}

	d.Handle(context.Background(), send, wsclient.Command{ID: "c1", Kind: "test.fail"})

	got := send.results[0]
	if got.status != "error" || got.errorMessage != "boom" {
		t.Fatalf("got %+v, want status=error errMsg=boom", got)
	}
}

func TestDispatcher_RejectedSentinelMapsToRejected(t *testing.T) {
	reg := NewRegistry()
	reg.Register(&fakeHandler{kind: "test.rej", err: ErrRejected})
	d := NewDispatcher(reg, discardLogger())
	send := &fakeSender{}

	d.Handle(context.Background(), send, wsclient.Command{ID: "c1", Kind: "test.rej"})

	if got := send.results[0]; got.status != "rejected" {
		t.Fatalf("status = %q, want rejected", got.status)
	}
}

func TestDispatcher_DuplicateCommandIDReplaysCachedResult(t *testing.T) {
	executions := 0
	reg := NewRegistry()
	reg.Register(&countingHandler{kind: "test.count", n: &executions, output: "first"})
	d := NewDispatcher(reg, discardLogger())
	send := &fakeSender{}

	d.Handle(context.Background(), send, wsclient.Command{ID: "c1", Kind: "test.count"})
	d.Handle(context.Background(), send, wsclient.Command{ID: "c1", Kind: "test.count"})

	if executions != 1 {
		t.Fatalf("handler executed %d times, want 1", executions)
	}
	if len(send.results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(send.results))
	}
	for i, r := range send.results {
		if r.output != "first" {
			t.Fatalf("result[%d].output = %v, want first", i, r.output)
		}
	}
}

func TestRegistry_DuplicateKindPanics(t *testing.T) {
	reg := NewRegistry()
	reg.Register(&fakeHandler{kind: "x"})
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic on duplicate registration")
		}
	}()
	reg.Register(&fakeHandler{kind: "x"})
}

type countingHandler struct {
	kind   string
	n      *int
	output any
}

func (c *countingHandler) Kind() string { return c.kind }
func (c *countingHandler) Execute(_ context.Context, _ json.RawMessage, _ ProgressFn) (any, error) {
	*c.n++
	return c.output, nil
}
