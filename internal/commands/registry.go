// Package commands implements the per-kind handler registry that backs the
// agent's WS command channel.
//
// Adding a new command kind:
//  1. Write a struct with Kind() and Execute() methods.
//  2. Register an instance with Registry.Register at startup.
//  3. Add a matching CommandPayload subtype on the backend.
//
// Concurrency model:
//   - Each command runs in its own goroutine spawned by wsclient.
//   - The dedup cache is consulted *before* dispatch and stores the prior
//     result (so a duplicate commandId never re-executes).
//   - Long-running handlers should respect ctx cancellation (the dispatcher
//     applies the deadlineMs).
package commands

import (
	"context"
	"encoding/json"
	"fmt"
)

// ProgressFn lets a handler emit a progress update. Cheap to call; nil is fine
// to ignore (the dispatcher always supplies one, but tests may not).
type ProgressFn func(stage, message string, percent *int)

// Handler executes one kind of command.
//
// Execute returns:
//   - output: serialized to JSON in the command_result frame
//   - err: non-nil → status "error" + errorMessage = err.Error()
//
// To explicitly reject (e.g. invalid params), return ErrRejected wrapped
// with the reason via fmt.Errorf("%w: %s", ErrRejected, reason).
type Handler interface {
	Kind() string
	Execute(ctx context.Context, payload json.RawMessage, progress ProgressFn) (output any, err error)
}

// ErrRejected is the sentinel for "agent declines to run this command" (as
// opposed to "ran but failed"). Distinguished by the dispatcher into status
// "rejected" instead of "error".
var ErrRejected = fmt.Errorf("rejected")

// Registry is a kind → Handler lookup. Construct with NewRegistry, register
// handlers at startup, then call Lookup from the dispatcher hot path.
type Registry struct {
	handlers map[string]Handler
}

func NewRegistry() *Registry { return &Registry{handlers: map[string]Handler{}} }

// Register adds h. Panics on duplicate kinds — wiring bugs should fail fast.
func (r *Registry) Register(h Handler) {
	if _, exists := r.handlers[h.Kind()]; exists {
		panic("commands: duplicate handler for kind " + h.Kind())
	}
	r.handlers[h.Kind()] = h
}

// Lookup returns the handler for kind, or (nil, false) if none is registered.
func (r *Registry) Lookup(kind string) (Handler, bool) {
	h, ok := r.handlers[kind]
	return h, ok
}

// Kinds returns all registered kinds — used by startup logging and tests.
func (r *Registry) Kinds() []string {
	out := make([]string, 0, len(r.handlers))
	for k := range r.handlers {
		out = append(out, k)
	}
	return out
}
