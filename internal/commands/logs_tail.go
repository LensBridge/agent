package commands

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

const (
	defaultLogsTailLines = 100
	maxLogsTailLines     = 500
)

// LogsTail returns the last N lines from the agent + kiosk journald units.
// Caller can override N via {"lines": N}; 1 ≤ N ≤ 500.
type LogsTail struct{}

type logsTailParams struct {
	Lines *int `json:"lines,omitempty"`
}

func (h *LogsTail) Kind() string { return "logs.tail" }

func (h *LogsTail) Execute(ctx context.Context, payload json.RawMessage, progress ProgressFn) (any, error) {
	n := defaultLogsTailLines
	if len(payload) > 0 && string(payload) != "null" {
		var p logsTailParams
		if err := json.Unmarshal(payload, &p); err != nil {
			return nil, fmt.Errorf("%w: bad payload: %v", ErrRejected, err)
		}
		if p.Lines != nil {
			n = *p.Lines
		}
	}
	if n < 1 {
		return nil, fmt.Errorf("%w: lines must be >= 1", ErrRejected)
	}
	if n > maxLogsTailLines {
		n = maxLogsTailLines
	}

	progress("reading", fmt.Sprintf("journalctl -n %d", n), nil)

	cmd := exec.CommandContext(ctx,
		"journalctl",
		"-u", "musallahboard-agent.service",
		"-u", "musallahboard-kiosk.service",
		"-n", strconv.Itoa(n),
		"--no-pager",
		"--output=short-iso",
	)
	out, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && len(exitErr.Stderr) > 0 {
			return nil, fmt.Errorf("journalctl: %w: %s", err, strings.TrimSpace(string(exitErr.Stderr)))
		}
		return nil, fmt.Errorf("journalctl: %w", err)
	}
	lines := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	return map[string]any{
		"lines":     lines,
		"requested": n,
	}, nil
}
