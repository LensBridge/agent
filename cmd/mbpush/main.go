// Command mbpush updates a MusallahBoard from the admin's laptop.
//
// Plug the laptop into the board's ethernet port (the service port), then:
//
//	mbpush musallahboard-content-3f2a1b4c-2026-09-24.mbu musallahboard-app-2.1.0.mbu
//	mbpush status
//
// and, the day before a visit, while the laptop has internet:
//
//	mbpush fetch
//
// Packages are uploaded over plain HTTP to the board's upload server at
// 10.77.0.1 (docs/architecture.md, section 9.5). That needs no account or key:
// every package is signed and checked by the board, which is what makes it
// safe. The same page is at http://10.77.0.1/ in any browser, so mbpush is a
// convenience, not a requirement.
//
// For a board that can only be reached over SSH, --ssh user@host streams each
// package to `sudo musallahboard-agent import -` through the system ssh.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const (
	// defaultHost is the board's address on the service-port cable. The
	// upload server only answers to this name (or musallahboard.local).
	defaultHost = "10.77.0.1"

	defaultAppChannel   = "https://github.com/LensBridge/MusallahBoard/releases/latest/download/app-channel.json"
	defaultAgentChannel = "https://github.com/LensBridge/agent/releases/latest/download/agent-channel-%s.json"
)

type options struct {
	host         string
	ssh          string // user@host; empty means HTTP upload
	identity     string
	checkHostKey bool

	// fetch
	dir          string
	arch         string
	appChannel   string
	agentChannel string

	command string   // "push", "status" or "fetch"
	files   []string // for push
}

func usage(w io.Writer) {
	fmt.Fprintf(w, `mbpush: update a MusallahBoard from this laptop

Usage:
  mbpush <file.mbu>...        Send update packages to the board and install them
  mbpush status               Show what the board has installed and its clock
  mbpush fetch                Download the latest board app and agent packages
                              (do this before a visit, while you have internet)

The laptop must be plugged into the board's ethernet port. The board gives it
an address within about a minute. You can also open http://%s/ in a browser.

Flags (anywhere on the line):
  --host <addr>          Board address (default %s)
  --ssh <user@host>      Send over SSH instead, to 'sudo musallahboard-agent import -'
  -i <file>              SSH identity file (with --ssh)
  --check-host-key       Check the board's SSH host key (with --ssh; off by default
                         on %s, where every board shares one address)

Fetch flags:
  --dir <dir>            Where to save the packages (default: current directory)
  --arch arm64|amd64     The boards' architecture (default arm64, a Raspberry Pi)
  --app-channel <url>    Board app release channel (default: MusallahBoard releases)
  --agent-channel <url>  Agent release channel (default: agent releases, for --arch)

Content packages (musallahboard-content-*.mbu) come from LensBridge:
Devices > the board > Download offline bundle.
`, defaultHost, defaultHost, defaultHost)
}

// parseArgs accepts flags before or after the positional arguments, since
// "mbpush file.mbu --host x" is as natural to type as the reverse.
func parseArgs(args []string) (options, error) {
	var o options
	fs := flag.NewFlagSet("mbpush", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&o.host, "host", defaultHost, "")
	fs.StringVar(&o.ssh, "ssh", "", "")
	fs.StringVar(&o.identity, "i", "", "")
	fs.BoolVar(&o.checkHostKey, "check-host-key", false, "")
	fs.StringVar(&o.dir, "dir", ".", "")
	fs.StringVar(&o.arch, "arch", "arm64", "")
	fs.StringVar(&o.appChannel, "app-channel", defaultAppChannel, "")
	fs.StringVar(&o.agentChannel, "agent-channel", "", "")
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return o, err
		}
		if fs.NArg() == 0 {
			break
		}
		pos = append(pos, fs.Arg(0))
		args = fs.Args()[1:]
	}

	switch {
	case len(pos) == 0:
		return o, errors.New("give one or more .mbu files, status, or fetch")
	case pos[0] == "status" || pos[0] == "fetch":
		if len(pos) > 1 {
			return o, fmt.Errorf("%s takes no other arguments", pos[0])
		}
		o.command = pos[0]
	default:
		o.command = "push"
		o.files = pos
	}

	if o.host == "" {
		return o, errors.New("--host cannot be empty")
	}
	if o.ssh != "" {
		if o.command == "fetch" {
			return o, errors.New("fetch downloads to this laptop; --ssh does not apply")
		}
		if !strings.Contains(o.ssh, "@") || strings.HasPrefix(o.ssh, "@") || strings.HasSuffix(o.ssh, "@") {
			return o, errors.New("--ssh needs user@host, e.g. --ssh ibra@board.local")
		}
	}
	if o.arch != "arm64" && o.arch != "amd64" {
		return o, errors.New("--arch must be arm64 or amd64")
	}
	if o.agentChannel == "" {
		o.agentChannel = fmt.Sprintf(defaultAgentChannel, o.arch)
	}
	for _, f := range o.files {
		if !strings.EqualFold(filepath.Ext(f), ".mbu") {
			return o, fmt.Errorf("%s is not an update package (.mbu)", f)
		}
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

	out := os.Stdout
	switch {
	case o.command == "fetch":
		err = runFetch(newDownloadClient(), o, out)
	case o.ssh != "":
		err = runSSH(o, out, os.Stderr)
	case o.command == "status":
		err = runStatus(newBoardClient(o.host), out)
	default:
		err = runPush(newBoardClient(o.host), o.files, out)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "\nmbpush: %v\n", err)
		os.Exit(1)
	}
}

// step prints a heading for one stage of the run.
func step(w io.Writer, format string, args ...any) {
	fmt.Fprintf(w, "==> "+format+"\n", args...)
}

// humanBytes prints a size the way a file manager would.
func humanBytes(n int64) string {
	switch {
	case n < 1<<10:
		return fmt.Sprintf("%d B", n)
	case n < 1<<20:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	case n < 1<<30:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	default:
		return fmt.Sprintf("%.2f GB", float64(n)/(1<<30))
	}
}
