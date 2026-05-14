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

// SystemReboot schedules a host reboot.
//
// Crucially: the result frame is returned BEFORE the reboot command is exec'd,
// so the dashboard sees a clean "ok" before the box dies. We achieve that by
// delegating the actual `shutdown` invocation to a goroutine that sleeps a
// few seconds, giving the dispatcher time to send the result frame and the
// backend time to persist it.
//
// Sudoers must allow `/sbin/shutdown -r +1` (1-minute delay) without a
// password.
type SystemReboot struct {
	// PostResultDelay is how long the goroutine sleeps before exec'ing
	// shutdown. Must be long enough for the result frame to traverse the
	// WS + STOMP fanout. Default 3s.
	PostResultDelay time.Duration

	// SkipExec is set by tests so the goroutine doesn't actually try to call
	// /sbin/shutdown. Production leaves it false.
	SkipExec bool
}

func (h *SystemReboot) Kind() string { return "system.reboot" }

func (h *SystemReboot) Execute(ctx context.Context, _ json.RawMessage, progress ProgressFn) (any, error) {
	progress("scheduling", "shutdown -r +1 (deferred)", nil)

	delay := h.PostResultDelay
	if delay <= 0 {
		delay = 3 * time.Second
	}

	if !h.SkipExec {
		// Detached goroutine — must outlive the dispatcher's run context.
		go func() {
			time.Sleep(delay)
			out, err := exec.Command("sudo", "-n", "/sbin/shutdown", "-r", "+1").CombinedOutput()
			if err != nil {
				fmt.Fprintf(os.Stderr, "system.reboot: shutdown failed: %v: %s\n",
					err, strings.TrimSpace(string(out)))
			}
		}()
	}

	return map[string]any{"scheduled": "+1 minute"}, nil
}
