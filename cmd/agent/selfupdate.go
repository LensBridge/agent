package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/LensBridge/agent/internal/selfupdate"
	"github.com/LensBridge/agent/internal/version"
)

// runSelfUpdate is `selfupdate apply` (docs/architecture.md, section 12),
// started as root by musallahboard-agent-update.service when the daemon has
// staged an agent package. Its output goes to the journal. It exits non-zero
// when the update was refused or rolled back, so `systemctl status` shows
// that something needs a look; the outcome is also in last-update.json.
func runSelfUpdate(args []string) {
	if len(args) != 1 || args[0] != "apply" {
		fmt.Fprintln(os.Stderr, "usage: musallahboard-agent selfupdate apply")
		fmt.Fprintln(os.Stderr, "  Installs the agent update the daemon staged, and rolls it back if the new agent does not start.")
		fmt.Fprintln(os.Stderr, "  It normally runs by itself; there is rarely a reason to run it by hand.")
		os.Exit(2)
	}
	requireRoot("selfupdate apply")

	// Once the binary is swapped the rest must finish (health check, and
	// rollback if needed), so a hangup of an interactive terminal is ignored
	// and SIGTERM only cancels the waits.
	signal.Ignore(syscall.SIGHUP, syscall.SIGPIPE)
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	u := selfupdate.New(version.Version)
	out, err := u.Apply(ctx)
	switch {
	case errors.Is(err, selfupdate.ErrNothingStaged):
		fmt.Println("No agent update is staged; nothing to do.")
	case err != nil:
		fmt.Printf("Agent update %s: %s\n", out.Status, out.Message)
		os.Exit(1)
	default:
		fmt.Println(out.Message)
	}
}
