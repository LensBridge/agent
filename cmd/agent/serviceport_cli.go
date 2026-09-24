package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/LensBridge/agent/internal/config"
)

// Names shared with setup.sh --service-port. Change them together.
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

// ── service-port ──────────────────────────────────────────────────────────────

// runServicePort is `service-port on|off [--force]` (docs/architecture.md,
// section 13): whether eth0 offers the service port and the upload server.
func runServicePort(args []string) {
	force := false
	var rest []string
	for _, a := range args {
		if a == "--force" {
			force = true
			continue
		}
		rest = append(rest, a)
	}
	if len(rest) != 1 || (rest[0] != "on" && rest[0] != "off") || (force && rest[0] != "on") {
		fmt.Fprintln(os.Stderr, "usage: musallahboard-agent service-port on|off [--force]")
		fmt.Fprintln(os.Stderr, "  --force  (on only) start the service port even if eth0 is connected to a network")
		os.Exit(2)
	}
	requireRoot("service-port " + rest[0])
	loadConfigCLI() // must be enrolled; SetServicePort rewrites this file

	// Turning it off over the service port kills this SSH session part way
	// through. Keep going when the terminal hangs up; every step left is one
	// we want finished.
	signal.Ignore(syscall.SIGHUP, syscall.SIGPIPE)

	if rest[0] == "on" {
		servicePortOn(force)
	} else {
		servicePortOff()
	}
}

// NetworkManager arrangement for eth0 (created by setup.sh --service-port):
//
//   - lanConn, a plain DHCP client profile at the default priority, tried
//     first. On a real network it gets an address and the board is just a
//     client there. `service-port on` gives it a single autoconnect retry, so when
//     a laptop (which runs no DHCP server) is plugged in it fails once and
//     NetworkManager moves on.
//   - serviceConn, the shared/DHCP-server profile, at the lowest priority
//     NetworkManager allows, so it is only ever autoconnected after every
//     other eth0 profile has failed.
//
// So a board booted plugged into a building's network comes up as a DHCP
// client, not a DHCP server, unless that network's DHCP server fails to answer
// within the 30 s DHCP timeout setup.sh gives lanConn. See docs/architecture.md, section 13.
const (
	lanConn             = "musallahboard-lan"
	servicePortPriority = "-999" // NM_SETTING_CONNECTION_AUTOCONNECT_PRIORITY_MIN
)

func servicePortOn(force bool) {
	if !nmConnectionExists(serviceConn) {
		fail("the NetworkManager connection %q does not exist, so a laptop could not reach this board.\n       Provision it first: bash setup.sh --service-port", serviceConn)
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
	if err := config.SetServicePort(defaultConfigPath, true); err != nil {
		fail("could not write %s: %v", defaultConfigPath, err)
	}
	fmt.Printf("Set service_port = true in %s.\n", defaultConfigPath)

	devices, _ := runCmd("nmcli", "-t", "-f", "DEVICE,CONNECTION", "device")
	routes, _ := runCmd("ip", "-4", "route", "show", "default")
	inUse, why := ifaceInUse(serviceIface, serviceConn, devices, routes)
	switch {
	case inUse && !force:
		fmt.Printf("%s is connected to a network right now (%s), so the service port was not started:\n"+
			"that would hand out addresses on that network. It starts by itself when a laptop is plugged\n"+
			"into %s instead, or run `sudo musallahboard-agent service-port on` again once %s is unplugged\n"+
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

	restartAgent()
	fmt.Printf("Done. Plug a laptop or phone into %s and open http://%s/ to send updates.\n", serviceIface, serviceIP)
}

func servicePortOff() {
	exists := nmConnectionExists(serviceConn)
	if sshViaServicePort() {
		warnf("you are connected through the service port (%s). Switching to online turns it off,\n"+
			"         so this SSH session will drop in a few seconds. The switch still completes.", serviceIP)
		time.Sleep(3 * time.Second)
	}

	if err := config.SetServicePort(defaultConfigPath, false); err != nil {
		fail("could not write %s: %v", defaultConfigPath, err)
	}
	fmt.Printf("Set service_port = false in %s.\n", defaultConfigPath)

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
	fmt.Println("Content keeps syncing from LensBridge whenever the board can reach it.")
}

// restartAgent restarts the daemon so it starts or stops the upload server.
func restartAgent() {
	if out, err := runCmd("systemctl", "restart", agentUnit); err != nil {
		fail("could not restart %s: %v\n%s", agentUnit, err, out)
	}
	fmt.Printf("Restarted %s.\n", agentUnit)
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
