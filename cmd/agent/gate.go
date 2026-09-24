package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/LensBridge/agent/internal/offline"
)

// ── gate ──────────────────────────────────────────────────────────────────────
//
// The gate is the only thing the restricted push account (setup.sh
// --offline, "mbpush") can run. sshd forces every session for that account to
// `sudo -n /usr/bin/musallahboard-agent gate`, whatever the client asked for,
// and passes the client's request in SSH_ORIGINAL_COMMAND. The gate accepts a
// short, fixed list of requests and refuses everything else, so a leaked push
// key can update content and fix the clock but cannot open a shell, read
// files or run anything else as root. See docs/offline.md, "Push account".
//
// The request is split on whitespace and matched token for token; it is never
// handed to a shell.
//
// Clients send every request as `sudo -n musallahboard-agent gate <request>`.
// For the push account sshd replaces that with the forced command, and the
// gate strips the prefix back off SSH_ORIGINAL_COMMAND. For the admin account
// the same text simply runs, and the request arrives as arguments. So one
// client works against either account without knowing which it has.

const (
	// The compressed app build a client may stream in. InstallApp separately
	// caps the unpacked size.
	gateMaxAppBytes = 200 << 20

	// clock-set refuses times outside this range. No real board clock is set
	// before this software existed or after 2100; a request for one is a bug
	// in the client or someone trying to break certificate or log timestamps.
	gateClockFloor = 1767225600 // 2026-01-01T00:00:00Z
	gateClockCeil  = 4102444800 // 2100-01-01T00:00:00Z

	gateAllowed = "status, status --json, clock, clock-set <unix-seconds>, bundle-install, app-install"

	// binaryPath is where setup.sh installs the agent; the sudoers rule and
	// sshd ForceCommand name it literally.
	binaryPath = "/usr/bin/musallahboard-agent"
)

type gateRequest struct {
	verb  string // status | clock | clock-set | bundle-install | app-install
	json  bool   // status --json
	epoch int64  // clock-set
}

// parseGateRequest turns a client's request into a gateRequest, or explains
// why it is refused.
func parseGateRequest(raw string) (gateRequest, error) {
	f := strings.Fields(raw)
	if len(f) > 0 && f[0] == "sudo" {
		if len(f) < 4 || f[1] != "-n" || (f[2] != "musallahboard-agent" && f[2] != binaryPath) || f[3] != "gate" {
			return gateRequest{}, fmt.Errorf("%q is not a gate request", raw)
		}
		f = f[4:]
	}
	if len(f) == 0 {
		return gateRequest{}, errors.New("no request given (this account can only run the MusallahBoard gate)")
	}

	switch {
	case len(f) == 1 && f[0] == "status":
		return gateRequest{verb: "status"}, nil
	case len(f) == 2 && f[0] == "status" && f[1] == "--json":
		return gateRequest{verb: "status", json: true}, nil
	case len(f) == 1 && f[0] == "clock":
		return gateRequest{verb: "clock"}, nil
	case len(f) == 2 && f[0] == "clock-set":
		epoch, err := parseGateEpoch(f[1])
		if err != nil {
			return gateRequest{}, err
		}
		return gateRequest{verb: "clock-set", epoch: epoch}, nil
	case len(f) == 1 && (f[0] == "bundle-install" || f[0] == "app-install"):
		return gateRequest{verb: f[0]}, nil
	}
	return gateRequest{}, fmt.Errorf("%q is not an allowed request", strings.Join(f, " "))
}

// parseGateEpoch accepts plain decimal seconds only: no sign, no spaces, no
// hex, and within [gateClockFloor, gateClockCeil).
func parseGateEpoch(s string) (int64, error) {
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("clock-set needs whole seconds since 1970, not %q", s)
		}
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < gateClockFloor || n >= gateClockCeil {
		return 0, fmt.Errorf("clock-set %s is outside 2026–2100; refusing to set the clock to that", s)
	}
	return n, nil
}

func runGate(args []string) {
	raw := strings.Join(args, " ")
	if raw == "" {
		raw = os.Getenv("SSH_ORIGINAL_COMMAND")
	}
	req, err := parseGateRequest(raw)
	if err != nil {
		fmt.Fprintf(os.Stderr, "refused: %v\nAllowed: %s\n", err, gateAllowed)
		os.Exit(126)
	}
	requireRoot("gate")

	switch req.verb {
	case "status":
		if req.json {
			runStatus([]string{"--json"})
		} else {
			runStatus(nil)
		}
	case "clock":
		fmt.Println(time.Now().Unix())
	case "clock-set":
		gateClockSet(req.epoch)
	case "bundle-install":
		cfg := loadConfigCLI()
		gateInstallFromStdin("bundle.zip", offline.MaxBundleBytes, func(path string) error {
			return installBundleAndReport(cfg, path)
		})
	case "app-install":
		gateInstallFromStdin("app.tar.gz", gateMaxAppBytes, installAppAndReport)
	}
}

func gateClockSet(epoch int64) {
	if out, err := runCmd("date", "-s", "@"+strconv.FormatInt(epoch, 10)); err != nil {
		fail("could not set the clock: %v\n%s", err, out)
	}
	fmt.Printf("Set the clock to %s.\n", time.Unix(epoch, 0).Format("Mon 2 Jan 2006 15:04:05 MST"))
	if _, err := runCmd("hwclock", "-w"); err != nil {
		fmt.Println("Warning: could not save the time to an RTC (none fitted?). The clock is right now, but a power cut will lose it.")
		return
	}
	fmt.Println("Saved it to the RTC.")
}

// gateInstallFromStdin saves stdin to a root-only temp file named name, hands
// it to install, and removes it whatever happens.
func gateInstallFromStdin(name string, max int64, install func(path string) error) {
	dir, err := os.MkdirTemp("", "musallahboard-gate-")
	if err != nil {
		fail("could not create a temporary directory: %v", err)
	}
	path := filepath.Join(dir, name)
	err = saveLimited(path, os.Stdin, max)
	if err == nil {
		err = install(path)
	}
	os.RemoveAll(dir)
	if err != nil {
		fail("%v", err)
	}
}

// saveLimited copies at most max bytes from r to path, failing if there is
// more or nothing at all.
func saveLimited(path string, r io.Reader, max int64) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	n, err := io.Copy(f, io.LimitReader(r, max+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	switch {
	case err != nil:
		return fmt.Errorf("receiving the upload failed: %w\n       Nothing on this board has changed.", err)
	case n == 0:
		return errors.New("nothing was sent. Stream the file on stdin.\n       Nothing on this board has changed.")
	case n > max:
		return fmt.Errorf("the upload is larger than %d MB, so it was refused.\n       Nothing on this board has changed.", max>>20)
	}
	return nil
}
