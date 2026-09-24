package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/LensBridge/agent/internal/cdp"
	"github.com/LensBridge/agent/internal/config"
	"github.com/LensBridge/agent/internal/offline"
)

// Names shared with setup.sh --offline. Change them together.
const (
	serviceConn  = "musallahboard-service-port"
	serviceIface = "eth0"
	serviceIP    = "10.77.0.1"
	agentUnit    = "musallahboard-agent.service"
	rtcDevice    = "/dev/rtc0"
)

// The CLI subcommands below are run by a person at a terminal (or by mbpush
// on their behalf), so everything they print is plain English, errors
// included. Anything that changes the system checks for root up front rather
// than failing half-way through.

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "Error: "+format+"\n", args...)
	os.Exit(1)
}

func warnf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "Warning: "+format+"\n", args...)
}

func requireRoot(cmd string) {
	if os.Geteuid() != 0 {
		fail("`musallahboard-agent %s` changes the system and must run as root.\n       Try: sudo musallahboard-agent %s", cmd, strings.Join(os.Args[1:], " "))
	}
}

// loadConfigCLI loads the enrolled config, explaining the usual failures.
func loadConfigCLI() *config.Config {
	cfg, err := config.Load(defaultConfigPath)
	switch {
	case err == nil:
		return cfg
	case errors.Is(err, fs.ErrPermission):
		fail("cannot read %s (permission denied). Run this with sudo.", defaultConfigPath)
	case errors.Is(err, fs.ErrNotExist):
		fail("this board is not enrolled yet (%s does not exist). Enroll it online first:\n       sudo musallahboard-agent enroll --token=<token> --backend=<url>", defaultConfigPath)
	default:
		fail("%v", err)
	}
	return nil
}

// ── bundle install ────────────────────────────────────────────────────────────

func runBundle(args []string) {
	if len(args) != 2 || args[0] != "install" {
		fmt.Fprintln(os.Stderr, "usage: musallahboard-agent bundle install <bundle.zip>")
		os.Exit(2)
	}
	requireRoot("bundle install")
	cfg := loadConfigCLI()
	if err := installBundleAndReport(cfg, args[1]); err != nil {
		fail("%v", err)
	}
}

// installBundleAndReport installs zipPath and prints the outcome. It returns
// rather than exiting on failure so the gate can remove its upload first.
func installBundleAndReport(cfg *config.Config, zipPath string) error {
	paths := offline.DefaultPaths()

	fmt.Printf("Checking %s ...\n", filepath.Base(zipPath))
	res, err := offline.InstallBundle(paths, zipPath, cfg.DeviceID)
	if res == nil {
		return fmt.Errorf("the bundle was NOT installed: %v\n       Nothing on this board has changed; it is still showing its previous content.", err)
	}
	m := res.Manifest
	fmt.Printf("Installed content for %s to %s (%d days, generated %s).\n",
		m.FirstDay, m.LastDay, len(m.Days()), m.GeneratedAt)
	if res.Previous != "" {
		fmt.Printf("The previous bundle is kept as a fallback.\n")
	}
	if err != nil {
		warnf("%v", err)
	}

	if cfg.Mode == config.ModeOffline {
		if err := reloadKiosk(); err != nil {
			warnf("could not reload the screen (%v).\n         The board picks up the new content by itself within 10 minutes.", err)
		} else {
			fmt.Println("Reloaded the screen.")
		}
	} else {
		fmt.Println("This board is in online mode, so the bundle is stored but not shown.")
		fmt.Println("It will be served after: sudo musallahboard-agent mode offline")
	}

	fmt.Println()
	printStatus(os.Stdout, buildCLIStatus(cfg, paths))
	return nil
}

// reloadKiosk asks the kiosk Chromium to reload over CDP.
func reloadKiosk() error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return cdp.New("").PageReload(ctx)
}

// ── status ────────────────────────────────────────────────────────────────────

// cliStatus is /api/local/status plus what only the CLI reports.
type cliStatus struct {
	offline.Status
	Clock clockInfo `json:"clock"`
	RTC   bool      `json:"rtc"`
}

type clockInfo struct {
	Now      string `json:"now"`      // RFC 3339, system zone
	Unix     int64  `json:"unix"`     // seconds since the epoch
	Timezone string `json:"timezone"` // IANA name where it can be found
}

func runStatus(args []string) {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "print the status object as JSON")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "usage: musallahboard-agent status [--json]")
		os.Exit(2)
	}
	cfg := loadConfigCLI()
	st := buildCLIStatus(cfg, offline.DefaultPaths())
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(st)
		return
	}
	printStatus(os.Stdout, st)
}

func buildCLIStatus(cfg *config.Config, paths offline.Paths) cliStatus {
	now := time.Now()
	_, rtcErr := os.Stat(rtcDevice)
	return cliStatus{
		Status: offline.ReadStatus(paths, cfg.Mode, cfg.DeviceID, now),
		Clock:  clockInfo{Now: now.Format(time.RFC3339), Unix: now.Unix(), Timezone: systemZone()},
		RTC:    rtcErr == nil,
	}
}

// systemZone names the system timezone. Go reports time.Local as "Local",
// which tells a person nothing.
func systemZone() string {
	if tz := strings.TrimPrefix(os.Getenv("TZ"), ":"); tz != "" {
		return tz
	}
	if target, err := os.Readlink("/etc/localtime"); err == nil {
		if _, name, ok := strings.Cut(target, "zoneinfo/"); ok {
			return name
		}
	}
	if b, err := os.ReadFile("/etc/timezone"); err == nil && strings.TrimSpace(string(b)) != "" {
		return strings.TrimSpace(string(b))
	}
	name, _ := time.Now().Zone()
	return name
}

func printStatus(w *os.File, st cliStatus) {
	row := func(k, format string, args ...any) {
		fmt.Fprintf(w, "  %-13s %s\n", k+":", fmt.Sprintf(format, args...))
	}

	row("Mode", "%s", st.Mode)
	row("Device", "%s", st.DeviceID)

	switch {
	case st.Error != "":
		row("Bundle", "installed but unreadable: %s", st.Error)
	case st.Bundle == nil:
		row("Bundle", "none installed")
		if st.Mode == config.ModeOffline {
			row("", "The board has nothing to show. Push a bundle with mbpush.")
		}
	default:
		b := st.Bundle
		row("Bundle", "%s to %s (generated %s, %s)", b.FirstDay, b.LastDay, b.GeneratedAt, b.Timezone)
		switch {
		case *st.StaleDays > 0:
			row("Today", "%s — content ran out %d day(s) ago; repeating %s", st.Today, *st.StaleDays, *st.ServingDay)
			row("", "Push a new bundle.")
		case st.Today < b.FirstDay:
			row("Today", "%s — bundle starts %s; showing its first day", st.Today, b.FirstDay)
			row("Remaining", "%d day(s) of content", *st.DaysRemaining)
		case *st.DaysRemaining == 0:
			row("Today", "%s — the last day in this bundle", st.Today)
			row("Remaining", "none after today. Push a new bundle.")
		default:
			row("Today", "%s", st.Today)
			note := ""
			if *st.DaysRemaining <= 3 {
				note = " — plan a visit soon"
			}
			row("Remaining", "%d more day(s) after today%s", *st.DaysRemaining, note)
		}
	}

	t, _ := time.Parse(time.RFC3339, st.Clock.Now)
	row("Clock", "%s (%s)", t.Format("Mon 2 Jan 2006 15:04:05 MST"), st.Clock.Timezone)
	if st.RTC {
		row("RTC", "present (%s)", rtcDevice)
	} else {
		row("RTC", "not found — the clock is only as good as the last mbpush or network time sync")
	}
}

// ── mode ──────────────────────────────────────────────────────────────────────

func runMode(args []string) {
	force := false
	var rest []string
	for _, a := range args {
		if a == "--force" {
			force = true
			continue
		}
		rest = append(rest, a)
	}
	if len(rest) != 1 || config.ValidateMode(rest[0]) != nil || (force && rest[0] != config.ModeOffline) {
		fmt.Fprintln(os.Stderr, "usage: musallahboard-agent mode online|offline [--force]")
		fmt.Fprintln(os.Stderr, "  --force  (offline only) start the service port even if eth0 is connected to a network")
		os.Exit(2)
	}
	target := rest[0]
	requireRoot("mode " + target)
	loadConfigCLI() // must be enrolled; SetMode rewrites this file

	// Switching to online over the service port kills this SSH session part
	// way through. Keep going when the terminal hangs up; every step left is
	// one we want finished.
	signal.Ignore(syscall.SIGHUP, syscall.SIGPIPE)

	switch target {
	case config.ModeOffline:
		modeOffline(force)
	case config.ModeOnline:
		modeOnline()
	}
}

// NetworkManager arrangement for eth0 (created by setup.sh --offline):
//
//   - lanConn, a plain DHCP client profile at the default priority, tried
//     first. On a real network it gets an address and the board is just a
//     client there. Offline mode gives it a single autoconnect retry, so when
//     a laptop (which runs no DHCP server) is plugged in it fails once and
//     NetworkManager moves on.
//   - serviceConn, the shared/DHCP-server profile, at the lowest priority
//     NetworkManager allows, so it is only ever autoconnected after every
//     other eth0 profile has failed.
//
// So a board booted plugged into a building's network comes up as a DHCP
// client, not a DHCP server, unless that network's DHCP server fails to answer
// within the 30 s DHCP timeout setup.sh gives lanConn. See docs/offline.md.
const (
	lanConn             = "musallahboard-lan"
	servicePortPriority = "-999" // NM_SETTING_CONNECTION_AUTOCONNECT_PRIORITY_MIN
)

func modeOffline(force bool) {
	if !nmConnectionExists(serviceConn) {
		fail("the NetworkManager connection %q does not exist, so a laptop could not reach this board.\n       Provision it first (while the board still has internet): bash setup.sh --offline", serviceConn)
	}
	if out, err := runCmd("nmcli", "connection", "modify", serviceConn,
		"connection.autoconnect", "yes", "connection.autoconnect-priority", servicePortPriority); err != nil {
		fail("could not enable the service port: %v\n%s", err, out)
	}
	if nmConnectionExists(lanConn) {
		if out, err := runCmd("nmcli", "connection", "modify", lanConn, "connection.autoconnect-retries", "1"); err != nil {
			fail("could not configure %s: %v\n%s", lanConn, err, out)
		}
	}
	if err := config.SetMode(defaultConfigPath, config.ModeOffline); err != nil {
		fail("could not write %s: %v", defaultConfigPath, err)
	}
	fmt.Printf("Set mode = offline in %s.\n", defaultConfigPath)

	devices, _ := runCmd("nmcli", "-t", "-f", "DEVICE,CONNECTION", "device")
	routes, _ := runCmd("ip", "-4", "route", "show", "default")
	inUse, why := ifaceInUse(serviceIface, serviceConn, devices, routes)
	switch {
	case inUse && !force:
		fmt.Printf("%s is connected to a network right now (%s), so the service port was not started:\n"+
			"that would hand out addresses on that network. It starts by itself when a laptop is plugged\n"+
			"into %s instead, or run `sudo musallahboard-agent mode offline` again once %s is unplugged\n"+
			"from this network.\n", serviceIface, why, serviceIface, serviceIface)
	case connectionOnDevice(devices, serviceIface) == serviceConn:
		fmt.Printf("The service port is already up on %s (%s).\n", serviceIface, serviceIP)
	default:
		if inUse {
			warnf("--force: starting the service port although %s is in use (%s).", serviceIface, why)
		}
		if out, err := runCmd("nmcli", "connection", "up", serviceConn); err != nil {
			// Normal with no cable plugged in.
			fmt.Printf("The service port is enabled and starts when a laptop is plugged into %s (now: %s).\n", serviceIface, firstLine(out))
		} else {
			fmt.Printf("The service port is up: plug a laptop into %s and connect to %s.\n", serviceIface, serviceIP)
		}
	}

	if _, _, err := offline.CurrentBundle(offline.DefaultPaths()); errors.Is(err, offline.ErrNoBundle) {
		warnf("no content bundle is installed, so the board will have nothing to show.\n         Push one with: mbpush <bundle.zip>")
	}

	restartAgent()
	fmt.Println("Done. This board is now offline: it serves the installed bundle and makes no network calls.")
}

func modeOnline() {
	exists := nmConnectionExists(serviceConn)
	if sshViaServicePort() {
		warnf("you are connected through the service port (%s). Switching to online turns it off,\n"+
			"         so this SSH session will drop in a few seconds. The switch still completes.", serviceIP)
		time.Sleep(3 * time.Second)
	}

	if err := config.SetMode(defaultConfigPath, config.ModeOnline); err != nil {
		fail("could not write %s: %v", defaultConfigPath, err)
	}
	fmt.Printf("Set mode = online in %s.\n", defaultConfigPath)

	if exists {
		if out, err := runCmd("nmcli", "connection", "modify", serviceConn, "connection.autoconnect", "no"); err != nil {
			fail("could not disable the service port's autoconnect: %v\n%s\n"+
				"       Do not plug this board into a network until it is fixed: it could hand out addresses there.", err, out)
		}
	}
	if nmConnectionExists(lanConn) {
		// Back to NetworkManager's default number of DHCP attempts.
		if out, err := runCmd("nmcli", "connection", "modify", lanConn, "connection.autoconnect-retries", "-1"); err != nil {
			warnf("could not reset %s's retries: %v\n%s", lanConn, err, out)
		}
	}

	// Before taking the port down: once it is down this session may be gone.
	restartAgent()

	devices, _ := runCmd("nmcli", "-t", "-f", "DEVICE,CONNECTION", "device")
	if exists && connectionOnDevice(devices, serviceIface) == serviceConn {
		if out, err := runCmd("nmcli", "connection", "down", serviceConn); err != nil {
			fail("could not bring the service port down: %v\n%s\n"+
				"       Do not plug this board into a network until it is fixed: it could hand out addresses there.", err, out)
		}
	}
	fmt.Println("Done. The service port is off, so it is now safe to connect eth0 to the musallah's network.")
	fmt.Println("The board goes back to its online content once it can reach LensBridge.")
}

// restartAgent restarts the daemon so it picks up the new mode. On startup it
// rewrites kiosk-url, and the kiosk .path watcher restarts the browser.
func restartAgent() {
	if out, err := runCmd("systemctl", "restart", agentUnit); err != nil {
		fail("could not restart %s: %v\n%s", agentUnit, err, out)
	}
	fmt.Printf("Restarted %s; the screen reloads in a few seconds.\n", agentUnit)
}

func nmConnectionExists(name string) bool {
	out, err := runCmd("nmcli", "-g", "NAME", "connection", "show")
	if err != nil {
		return false
	}
	for _, line := range strings.Split(out, "\n") {
		if unescapeTerse(strings.TrimSpace(line)) == name {
			return true
		}
	}
	return false
}

func runCmd(name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(s, "\n")
	return s
}

// sshViaServicePort reports whether an SSH session is connected through the
// service-port address. sudo drops SSH_CONNECTION by default, so the kernel's
// socket table is the reliable source; the variable is a cheap first check.
func sshViaServicePort() bool {
	if f := strings.Fields(os.Getenv("SSH_CONNECTION")); len(f) == 4 && f[2] == serviceIP {
		return true
	}
	data, err := os.ReadFile("/proc/net/tcp")
	return err == nil && hasEstablishedTCP(string(data), serviceIP, 22)
}

// hasEstablishedTCP scans /proc/net/tcp content for an ESTABLISHED socket
// whose local end is ip:port. Addresses there are hex, in host byte order
// for the IP (little-endian on every Pi) and big-endian for the port.
func hasEstablishedTCP(procNetTCP, ip string, port int) bool {
	var a, b, c, d byte
	if n, _ := fmt.Sscanf(ip, "%d.%d.%d.%d", &a, &b, &c, &d); n != 4 {
		return false
	}
	want := fmt.Sprintf("%02X%02X%02X%02X:%04X", d, c, b, a, port)
	for _, line := range strings.Split(procNetTCP, "\n")[1:] {
		f := strings.Fields(line)
		// sl local_address rem_address st ...; st 01 is ESTABLISHED.
		if len(f) > 3 && strings.EqualFold(f[1], want) && f[3] == "01" {
			return true
		}
	}
	return false
}

// ── app install ───────────────────────────────────────────────────────────────

func runApp(args []string) {
	if len(args) != 2 || args[0] != "install" {
		fmt.Fprintln(os.Stderr, "usage: musallahboard-agent app install <dist-dir-or-tar.gz>")
		os.Exit(2)
	}
	requireRoot("app install")
	if err := installAppAndReport(args[1]); err != nil {
		fail("%v", err)
	}
}

// installAppAndReport installs the build at src and prints the outcome,
// returning on failure like installBundleAndReport.
func installAppAndReport(src string) error {
	paths := offline.DefaultPaths()

	fmt.Printf("Installing the board app from %s ...\n", src)
	res, err := offline.InstallApp(paths, src)
	if res == nil {
		return fmt.Errorf("the app was NOT installed: %v\n       Nothing on this board has changed.", err)
	}
	fmt.Printf("Installed the board app into %s.\n", paths.SPA)
	if err != nil {
		warnf("%v", err)
	}

	// The app is only served locally in offline mode; online, the kiosk loads
	// the hosted board and there is nothing to reload. An unenrolled board
	// (setup.sh --offline before enrollment) has no mode yet either.
	if cfg, err := config.Load(defaultConfigPath); err == nil && cfg.Mode == config.ModeOffline {
		if err := reloadKiosk(); err != nil {
			warnf("could not reload the screen (%v); it picks up the new app on its next reload.", err)
		} else {
			fmt.Println("Reloaded the screen.")
		}
	}
	return nil
}
