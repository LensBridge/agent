package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/LensBridge/agent/internal/fsutil"
	"github.com/LensBridge/agent/packaging"
)

// defaultSplashPath is where the kiosk loads the enrollment splash from
// (packaging/start-kiosk.sh).
const defaultSplashPath = "/usr/share/musallahboard/waiting.html"

// runSplash is `splash install [path]`: write the enrollment splash embedded
// in this binary. setup.sh runs it once the binary is in place, and the
// self-updater after each update, so the page always matches the agent.
func runSplash(args []string) {
	if len(args) < 1 || args[0] != "install" || len(args) > 2 {
		fmt.Fprintln(os.Stderr, "usage: musallahboard-agent splash install [path]")
		os.Exit(2)
	}
	path := defaultSplashPath
	if len(args) == 2 {
		path = args[1]
	}
	requireRoot("splash install")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		fail("%v", err)
	}
	if err := fsutil.WriteAtomic(path, packaging.WaitingHTML, 0o644); err != nil {
		fail("could not write %s: %v", path, err)
	}
}
