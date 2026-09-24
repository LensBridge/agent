// Command mbpush updates an offline MusallahBoard from the admin's laptop.
//
// Plug the laptop into the board's ethernet port, then:
//
//	mbpush musallahboard-3f2a1b4c-2026-09-24.zip   # clock sync, copy, install, status
//	mbpush --app dist/                             # update the board app
//	mbpush status                                  # clock check + status, changes nothing
//
// It shells out to the system ssh (Windows 10+ ships it), so the user's
// existing keys and ssh-agent work as they do everywhere else. There is no
// upload endpoint on the board: everything goes over SSH to the restricted
// push account setup.sh --offline creates, whose only command is the agent's
// gate. Files are streamed to it on stdin. See docs/offline.md, "mbpush" and
// "Push account".
package main

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const (
	// defaultHost is the board's address on the service-port cable.
	defaultHost = "10.77.0.1"
	// defaultUser is the restricted push account setup.sh --offline creates.
	// The admin account works too: requests are sent in a form both accept.
	defaultUser = "mbpush"
)

type options struct {
	host, user, identity string
	app                  string
	checkHostKey         bool
	args                 []string
}

func usage(w io.Writer) {
	fmt.Fprintf(w, `mbpush — update an offline MusallahBoard over its ethernet port

Usage:
  mbpush <bundle.zip>                  Set the board's clock, send and install the bundle, show status
  mbpush --app <dist-dir-or-tar.gz>    Install a new board app build
  mbpush status                        Check the clock and show status; changes nothing

Flags (anywhere on the line):
  --host <addr>       Board address (default %s)
  --user <name>       Account on the board (default %s, the push account)
  -i <file>           SSH identity file (default: your ssh config / agent)
  --check-host-key    Check the board's SSH host key as usual (off by default
                      on %s, where every board shares one address)
`, defaultHost, defaultUser, defaultHost)
}

// parseArgs accepts flags before or after the positional argument, since
// "mbpush bundle.zip --user admin" is as natural to type as the reverse.
func parseArgs(args []string) (options, error) {
	var o options
	fs := flag.NewFlagSet("mbpush", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&o.host, "host", defaultHost, "")
	fs.StringVar(&o.user, "user", defaultUser, "")
	fs.StringVar(&o.identity, "i", "", "")
	fs.StringVar(&o.app, "app", "", "")
	fs.BoolVar(&o.checkHostKey, "check-host-key", false, "")
	for {
		if err := fs.Parse(args); err != nil {
			return o, err
		}
		if fs.NArg() == 0 {
			break
		}
		o.args = append(o.args, fs.Arg(0))
		args = fs.Args()[1:]
	}
	switch {
	case o.app != "" && len(o.args) > 0:
		return o, errors.New("--app takes the build to install; give no other arguments")
	case o.app == "" && len(o.args) != 1:
		return o, errors.New("give one bundle .zip, --app <build>, or status")
	case o.host == "" || o.user == "":
		return o, errors.New("--host and --user cannot be empty")
	}
	return o, nil
}

func main() {
	for _, a := range os.Args[1:] {
		if a == "-h" || a == "--help" || a == "help" {
			usage(os.Stdout)
			return
		}
	}
	o, err := parseArgs(os.Args[1:])
	if err != nil {
		fmt.Fprintf(os.Stderr, "mbpush: %v\n\n", err)
		usage(os.Stderr)
		os.Exit(2)
	}
	if _, err := exec.LookPath("ssh"); err != nil {
		die("ssh was not found. mbpush uses the system OpenSSH client.\n" +
			"On Windows: Settings > System > Optional features > OpenSSH Client.")
	}

	r := remote{host: o.host, user: o.user, identity: o.identity, checkHostKey: o.checkHostKey,
		stdout: os.Stdout, stderr: os.Stderr}

	switch {
	case o.app != "":
		pushApp(r, o.app)
	case o.args[0] == "status":
		syncClock(r, false)
		showStatus(r)
	default:
		pushBundle(r, o.args[0])
	}
}

// cleanups run before die exits; os.Exit skips deferred calls.
var cleanups []func()

func die(format string, args ...any) {
	for _, f := range cleanups {
		f()
	}
	fmt.Fprintf(os.Stderr, "\nmbpush: "+format+"\n", args...)
	os.Exit(1)
}

func step(format string, args ...any) {
	fmt.Printf("==> "+format+"\n", args...)
}

// connectHelp explains a failure to reach the board.
func connectHelp(r remote) string {
	return fmt.Sprintf("could not connect to %s.\n"+
		"  - Is the cable plugged into the board's ethernet port?\n"+
		"  - Is this laptop's wired adapter set to get an address automatically (DHCP)?\n"+
		"    It should get one in 10.77.0.x within a few seconds of plugging in.\n"+
		"  - Is your SSH key the one set up for %q on this board? (use -i to pick one)\n"+
		"Nothing on the board has changed.", r.target(), r.user)
}

// syncClock compares the board's clock with this laptop's and, when set is
// true and they are more than clockTolerance apart, sets the board's clock
// and writes it to the RTC. The drift is always printed.
func syncClock(r remote, set bool) {
	step("Checking the board's clock")
	sent := time.Now()
	out, err := r.output(gate("clock"))
	got := time.Now()
	if err != nil {
		if isConnectFailure(err) {
			die("%s", connectHelp(r))
		}
		die("could not read the board's clock: %v", err)
	}
	epoch, err := parseEpoch(out)
	if err != nil {
		die("could not read the board's clock: %v", err)
	}
	c := compareClocks(sent, got, epoch)
	fmt.Printf("    The board's clock is %s.\n", describeDrift(c.Drift))
	if !c.NeedsSet {
		return
	}
	if !set {
		fmt.Printf("    That is more than %s off; `mbpush <bundle.zip>` would correct it.\n", clockTolerance)
		return
	}

	// The gate also reports whether it could save the time to an RTC.
	if err := r.run(gate(fmt.Sprintf("clock-set %d", time.Now().Unix()))); err != nil {
		die("could not set the board's clock: %v\nNothing else on the board has changed.", err)
	}
}

func showStatus(r remote) {
	step("Board status")
	if err := r.run(gate("status")); err != nil {
		if isConnectFailure(err) {
			die("%s", connectHelp(r))
		}
		die("could not read the board's status: %v", err)
	}
}

func pushBundle(r remote, local string) {
	fi, err := os.Stat(local)
	if err != nil {
		die("cannot read %s: %v", local, err)
	}
	if fi.IsDir() || !strings.EqualFold(filepath.Ext(local), ".zip") {
		die("%s is not a .zip bundle. Download one from LensBridge: Devices > the board > Download offline bundle.", local)
	}

	syncClock(r, true)

	step("Sending and installing the bundle")
	if err := r.runWithInput(gate("bundle-install"), local); err != nil {
		if isConnectFailure(err) {
			die("lost the connection to the board during the install.\n" +
				"The board is still showing its previous content unless the output above says the bundle was installed.")
		}
		die("the bundle was not installed (see above).\nThe board is still showing its previous content.")
	}
	showStatus(r)
	fmt.Println("\nDone. You can unplug the cable.")
}

func pushApp(r remote, local string) {
	fi, err := os.Stat(local)
	if err != nil {
		die("cannot read %s: %v", local, err)
	}
	if fi.IsDir() {
		if _, err := os.Stat(filepath.Join(local, "index.html")); err != nil {
			die("%s has no index.html. Point at the build output directory (usually dist/).", local)
		}
		tmp, err := tarGzDir(local)
		if err != nil {
			die("could not package %s: %v", local, err)
		}
		cleanups = append(cleanups, func() { os.Remove(tmp) })
		defer os.Remove(tmp)
		local = tmp
	} else if !strings.HasSuffix(local, ".tar.gz") && !strings.HasSuffix(local, ".tgz") {
		die("%s is neither a directory nor a .tar.gz.", local)
	}

	syncClock(r, true)

	step("Sending and installing the app")
	if err := r.runWithInput(gate("app-install"), local); err != nil {
		if isConnectFailure(err) {
			die("lost the connection to the board during the install.")
		}
		die("the app was not installed (see above).\nThe board is still showing its previous content.")
	}
	fmt.Println("\nDone. You can unplug the cable.")
}

// gate wraps a request for the agent's gate (cmd/agent/gate.go). The push
// account's forced command ignores what we send and reads it back from
// SSH_ORIGINAL_COMMAND; the admin account simply runs it. Either way it
// reaches the gate.
func gate(request string) string {
	return "sudo -n musallahboard-agent gate " + request
}

// tarGzDir packs the files under dir into a temporary .tar.gz and returns
// its path. Only regular files and directories are included.
func tarGzDir(dir string) (string, error) {
	f, err := os.CreateTemp("", "mbpush-app-*.tar.gz")
	if err != nil {
		return "", err
	}
	name := f.Name()
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)

	werr := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil || rel == "." {
			return err
		}
		rel = filepath.ToSlash(rel)
		info, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case d.IsDir():
			return tw.WriteHeader(&tar.Header{Name: rel + "/", Typeflag: tar.TypeDir, Mode: 0o755, ModTime: info.ModTime()})
		case d.Type().IsRegular():
			if err := tw.WriteHeader(&tar.Header{Name: rel, Typeflag: tar.TypeReg, Mode: 0o644, Size: info.Size(), ModTime: info.ModTime()}); err != nil {
				return err
			}
			src, err := os.Open(p)
			if err != nil {
				return err
			}
			defer src.Close()
			_, err = io.Copy(tw, src)
			return err
		default:
			return fmt.Errorf("%s is a link or special file; the build should contain only files and directories", rel)
		}
	})
	for _, c := range []io.Closer{tw, gz, f} {
		if err := c.Close(); err != nil && werr == nil {
			werr = err
		}
	}
	if werr != nil {
		os.Remove(name)
		return "", werr
	}
	return name, nil
}
