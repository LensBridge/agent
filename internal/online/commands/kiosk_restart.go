package commands

import (
	"context"
	"encoding/json"

	"github.com/LensBridge/agent/internal/core/kiosk"
)

// KioskRestart bounces the musallahboard-kiosk systemd unit (kiosk.Restart),
// through the sudoers allow-list.
type KioskRestart struct{}

func (h *KioskRestart) Kind() string { return "kiosk.restart" }

func (h *KioskRestart) Execute(ctx context.Context, _ json.RawMessage, progress ProgressFn) (any, error) {
	progress("restarting", "systemctl restart musallahboard-kiosk.service", nil)
	if err := kiosk.Restart(ctx); err != nil {
		return nil, err
	}
	return map[string]any{"restarted": true}, nil
}
