package commands

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// SystemReboot triggers an immediate host reboot.
//
// Crucially: the result frame is returned BEFORE `systemctl reboot` is exec'd,
// so the dashboard sees a clean "ok" before the box dies. We achieve that by
// delegating the actual invocation to a goroutine that sleeps PostResultDelay,
// giving the dispatcher time to send the result frame and the backend time to
// persist it. After that brief delay the reboot happens immediately (no
// `shutdown +1` grace period).
//
// Sudoers must allow `/bin/systemctl reboot` without a password.
type SystemReboot struct {
	// PostResultDelay is how long the goroutine sleeps before exec'ing
	// systemctl. Must be long enough for the result frame to traverse the
	// WS + STOMP fanout. Default 3s.
	PostResultDelay time.Duration

	// SkipExec is set by tests so the goroutine doesn't actually reboot.
	// Production leaves it false.
	SkipExec bool
}

func (h *SystemReboot) Kind() string { return "system.reboot" }

func (h *SystemReboot) Execute(ctx context.Context, _ json.RawMessage, progress ProgressFn) (any, error) {
	progress("rebooting", "systemctl reboot (immediate)", nil)

	delay := h.PostResultDelay
	if delay <= 0 {
		delay = 3 * time.Second
	}

	if !h.SkipExec {
		// Detached goroutine — must outlive the dispatcher's run context.
		go func() {
			time.Sleep(delay)
			out, err := exec.Command("sudo", "-n", "/bin/systemctl", "reboot").CombinedOutput()
			if err != nil {
				fmt.Fprintf(os.Stderr, "system.reboot: systemctl reboot failed: %v: %s\n",
					err, strings.TrimSpace(string(out)))
			}
		}()
	}

	return map[string]any{"scheduled": "immediate"}, nil
}
