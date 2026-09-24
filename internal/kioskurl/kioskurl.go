// Package kioskurl composes the URL the on-device kiosk browser loads and
// persists it where the cage/Chromium launcher can read it.
//
// The board's base URL (e.g. https://board.lensbridge.tech) is provisioned by
// the setup scripts into BoardURLPath. The agent owns the device identity, so
// it is the only component that can append ?deviceId=<uuid>. It writes the
// composed URL to OutPath, which doubles as the launcher's enrollment sentinel:
// the kiosk systemd unit blocks until this file exists, guaranteeing the board
// never loads without a device id.
//
// The frontend reads ?deviceId off the URL before its first render, persists
// it to a cookie, and strips the query in place (history.replaceState — no
// reload; an extra navigation here would flash the screen on every boot). So
// re-asserting the param on every launch is idempotent and self-healing.
//
// This file is the provisioning path for a board that is starting up. To fix
// a board that is already running — wrong cookie, wiped profile — the agent
// pushes the id into the live page instead, via the config.refresh command.
//
// In offline mode the base URL is not the provisioned one but the agent's own
// local server (OfflineBaseURL), and the page is told so with &mode=offline.
// board-url is left alone, so switching back to online needs nothing restored.
package kioskurl

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// DefaultBoardURLPath holds the operator-provisioned base board URL (no query).
const DefaultBoardURLPath = "/etc/musallahboard/board-url"

// DefaultOutPath is the composed URL the kiosk launcher reads. It is also the
// enrollment sentinel the kiosk systemd unit waits on.
const DefaultOutPath = "/etc/musallahboard/kiosk-url"

// OfflineBaseURL is the agent's local server, which serves the board in
// offline mode. It must match the listen address in internal/offline.
const OfflineBaseURL = "http://127.0.0.1:8080/"

// Write reads the base board URL from boardURLPath, appends ?deviceId=<deviceID>,
// and atomically writes the result to outPath (0644 so the unprivileged kiosk
// user can read it across the 0751 config dir).
//
// Returns an error if the base URL file is missing/empty or deviceID is empty —
// callers treat a write failure as non-fatal (the launcher keeps waiting) but
// should log it.
func Write(boardURLPath, outPath, deviceID string) error {
	if deviceID == "" {
		return fmt.Errorf("kioskurl: empty deviceID")
	}
	raw, err := os.ReadFile(boardURLPath)
	if err != nil {
		return fmt.Errorf("kioskurl: read base url %s: %w", boardURLPath, err)
	}
	base := strings.TrimSpace(string(raw))
	if base == "" {
		return fmt.Errorf("kioskurl: base url %s is empty", boardURLPath)
	}

	return writeURL(outPath, Compose(base, deviceID))
}

// WriteOffline writes the offline-mode kiosk URL for deviceID to outPath:
// the local server, with the device id and mode=offline. It ignores board-url.
func WriteOffline(outPath, deviceID string) error {
	if deviceID == "" {
		return fmt.Errorf("kioskurl: empty deviceID")
	}
	return writeURL(outPath, OfflineURL(deviceID))
}

// Compose appends deviceId to base, respecting a query base already has.
func Compose(base, deviceID string) string {
	sep := "?"
	if strings.Contains(base, "?") {
		sep = "&"
	}
	return base + sep + "deviceId=" + deviceID
}

// OfflineURL is the URL the kiosk loads in offline mode.
func OfflineURL(deviceID string) string {
	return OfflineBaseURL + "?deviceId=" + url.QueryEscape(deviceID) + "&mode=offline"
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
