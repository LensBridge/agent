package commands

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
)

// KioskRestart bounces the musallahboard-kiosk systemd unit. Requires a
// sudoers rule allowing `/bin/systemctl restart musallahboard-kiosk.service`
// without a password (installed by packaging/sudoers.d/musallahboard-agent).
type KioskRestart struct{}

func (h *KioskRestart) Kind() string { return "kiosk.restart" }

func (h *KioskRestart) Execute(ctx context.Context, _ json.RawMessage, progress ProgressFn) (any, error) {
	progress("restarting", "systemctl restart musallahboard-kiosk.service", nil)
	cmd := exec.CommandContext(ctx, "sudo", "-n", "/bin/systemctl", "restart", "musallahboard-kiosk.service")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("systemctl: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return map[string]any{"restarted": true}, nil
}
