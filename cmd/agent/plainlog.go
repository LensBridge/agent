package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
)

// plainHandler writes log records as plain sentences for a person at a
// terminal: the message only, warnings and errors marked, no JSON. The
// daemon logs JSON for the journal; one-off commands (enroll) talk to people.
type plainHandler struct{ w io.Writer }

func (h plainHandler) Enabled(_ context.Context, l slog.Level) bool { return l >= slog.LevelInfo }

func (h plainHandler) Handle(_ context.Context, r slog.Record) error {
	prefix := "  "
	switch {
	case r.Level >= slog.LevelError:
		prefix = "  Error: "
	case r.Level >= slog.LevelWarn:
		prefix = "  Warning: "
	}
	_, err := fmt.Fprintln(h.w, prefix+r.Message)
	return err
}

func (h plainHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h plainHandler) WithGroup(string) slog.Handler      { return h }
