// Package kioskurl persists the URL the on-device kiosk browser loads, where
// the cage/Chromium launcher can read it.
//
// In v2 every board is served by its own agent (docs/architecture.md,
// section 14), so once a device is enrolled the URL is always the local
// server, with no query string: the page asks the agent who it is. The file
// doubles as the launcher's enrollment sentinel: until it exists the kiosk
// shows the "waiting for enrollment" splash, and the systemd .path watcher
// restarts the kiosk when it appears or changes.
package kioskurl

import (
	"fmt"
	"os"
	"path/filepath"
)

// DefaultOutPath is the URL the kiosk launcher reads. It is also the
// enrollment sentinel the kiosk waits on.
const DefaultOutPath = "/etc/musallahboard/kiosk-url"

// LocalURL is the agent's local server. It must match
// localserver.BaseURL; a test checks.
const LocalURL = "http://127.0.0.1:8080/"

// WriteLocal writes LocalURL to outPath (0644 so the unprivileged kiosk user
// can read it across the 0751 config dir). An unchanged file is not
// rewritten, so a restart of the agent never bounces the kiosk.
func WriteLocal(outPath string) error {
	if outPath == "" {
		return fmt.Errorf("kioskurl: empty path")
	}
	return writeURL(outPath, LocalURL)
}

// writeURL atomically writes u (plus a trailing newline) to outPath.
func writeURL(outPath, u string) error {
	composed := u + "\n"

	// Skip the write entirely when the composed URL is unchanged. The kiosk
	// reload is driven by a systemd.path unit watching outPath; an identical
	// rewrite (every daemon startup re-composes this) would needlessly cycle
	// the kiosk on an already-enrolled device. Only a real change should fire
	// the watcher.
	if existing, err := os.ReadFile(outPath); err == nil && string(existing) == composed {
		return nil
	}

	tmp := outPath + ".tmp"
	if err := os.WriteFile(tmp, []byte(composed), 0o644); err != nil {
		return fmt.Errorf("kioskurl: write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, outPath); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("kioskurl: rename %s: %w", outPath, err)
	}
	// WriteFile honours umask; force the mode so the kiosk user can always read.
	_ = os.Chmod(outPath, 0o644)
	_ = chownToDirOwner(outPath, filepath.Dir(outPath))
	return nil
}
