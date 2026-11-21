// Package packaging embeds the system files the agent installs itself: its
// systemd units, udev rules, sudoers allow-list, modules-load list, the kiosk
// launcher and the kiosk's local pages. `musallahboard-agent install-system`
// writes them (setup.sh runs it), so a board set up by the curl|bash setup.sh
// (which has no repository checkout) gets exactly these bytes, and there is
// one copy of each.
package packaging

import (
	"bytes"
	"embed"
	"io/fs"

	updateanim "github.com/LensBridge/agent/update-anim"
)

//go:embed *.service *.path start-kiosk.sh starting.html udev sudoers.d modules-load.d rtc
var files embed.FS

// File is one system file: its name in this directory, where it goes, and
// its mode.
type File struct {
	Name string
	Path string
	Mode fs.FileMode
}

// Units are the systemd units every board has, enabled by install-system.
var Units = []string{
	"musallahboard-agent.service",
	"musallahboard-kiosk.service",
	"musallahboard-agent-update.path",
}

// System is what every board gets.
var System = []File{
	{"musallahboard-agent.service", "/lib/systemd/system/musallahboard-agent.service", 0o644},
	{"musallahboard-kiosk.service", "/lib/systemd/system/musallahboard-kiosk.service", 0o644},
	{"musallahboard-agent-update.path", "/lib/systemd/system/musallahboard-agent-update.path", 0o644},
	{"musallahboard-agent-update.service", "/lib/systemd/system/musallahboard-agent-update.service", 0o644},
	{"musallahboard-usb-import@.service", "/lib/systemd/system/musallahboard-usb-import@.service", 0o644},
	{"udev/90-musallahboard-usb.rules", "/etc/udev/rules.d/90-musallahboard-usb.rules", 0o644},
	{"udev/90-musallahboard-rtc.rules", "/etc/udev/rules.d/90-musallahboard-rtc.rules", 0o644},
	{"sudoers.d/musallahboard-agent", "/etc/sudoers.d/musallahboard-agent", 0o440},
	{"modules-load.d/musallahboard.conf", "/etc/modules-load.d/musallahboard.conf", 0o644},
	{"start-kiosk.sh", "/usr/bin/start-kiosk.sh", 0o755},
	{"starting.html", "/usr/share/musallahboard/starting.html", 0o644},
}

// RTC is what a board with a DS3231 clock module also gets (--rtc).
var RTC = []File{
	{"rtc/85-musallahboard-rtc.rules", "/etc/udev/rules.d/85-musallahboard-rtc.rules", 0o644},
	{"rtc/musallahboard-rtc.service", "/lib/systemd/system/musallahboard-rtc.service", 0o644},
}

// Obsolete are files earlier versions installed and nothing uses any more;
// install-system removes them (units are disabled first).
var Obsolete = []string{
	// The kiosk loaded the URL in kiosk-url and was restarted when it
	// changed; it always loads the local server now.
	"/lib/systemd/system/musallahboard-kiosk-watch.path",
	"/lib/systemd/system/musallahboard-kiosk-reload.service",
	"/etc/musallahboard/kiosk-url",
	// The enrollment splash; an unprovisioned board's screen is served by
	// the local server now.
	"/usr/share/musallahboard/waiting.html",
}

// screenCSS is the marker in starting.html that Read replaces with the
// update screen's stylesheet and the shared screen styles.
const screenCSS = "/*{{screen-css}}*/"

// Read returns the embedded file named name. starting.html comes back with
// the update screen's style filled in, so the page on disk is complete.
func Read(name string) ([]byte, error) {
	b, err := files.ReadFile(name)
	if err != nil || name != "starting.html" {
		return b, err
	}
	return bytes.Replace(b, []byte(screenCSS), []byte(updateanim.Style+updateanim.SharedCSS), 1), nil
}
