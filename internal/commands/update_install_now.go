package commands

import (
	"context"
	"encoding/json"

	"github.com/LensBridge/agent/internal/updates"
)

// UpdateInstallNow checks the release channels and installs any new board app
// or agent at once, instead of in the board's quiet window. The board shows
// the update screen while it installs, as it would at night.
//
// An agent update restarts the agent a few seconds after this returns; the
// result is sent first, but may be lost if the restart wins the race.
type UpdateInstallNow struct {
	Run func(ctx context.Context) updates.Outcome
}

func (h *UpdateInstallNow) Kind() string { return "update.install_now" }

func (h *UpdateInstallNow) Execute(ctx context.Context, _ json.RawMessage, progress ProgressFn) (any, error) {
	progress("checking", "asking the release channels for new software", nil)
	return h.Run(ctx), nil
}
