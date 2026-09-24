package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// The --ssh fallback, for a board reachable only over SSH (for example on a
// building network, with the service port off). It uses the system ssh, so
// the admin's keys, agent and config apply unchanged, and needs the admin
// account on the board (passwordless sudo, as setup.sh configures it). The
// transport is not what makes this safe: the board verifies every package
// exactly as it does an upload.

const remoteAgent = "sudo -n musallahboard-agent"

// sshOptions are the options for every ssh call.
//
// On the default service-port address every board answers with its own host
// key, so with normal checking the second board an admin visits fails with
// "REMOTE HOST IDENTIFICATION HAS CHANGED". There the link is a cable from
// this laptop straight into the board, so host keys are not recorded or
// checked. Any other host keeps ssh's normal behaviour, and --check-host-key
// restores it for the default too.
func sshOptions(o options) []string {
	opts := []string{"-o", "ConnectTimeout=10", "-o", "ServerAliveInterval=15"}
	if o.identity != "" {
		opts = append(opts, "-i", o.identity)
	}
	if sshHost(o.ssh) == defaultHost && !o.checkHostKey {
		opts = append(opts,
			"-o", "StrictHostKeyChecking=no",
			"-o", "UserKnownHostsFile="+os.DevNull,
			"-o", "LogLevel=ERROR",
		)
	}
	return opts
}

func sshArgs(o options, command string) []string {
	return append(sshOptions(o), o.ssh, command)
}

// sshHost is the host part of user@host.
func sshHost(target string) string {
	_, host, _ := strings.Cut(target, "@")
	return host
}

func runSSH(o options, out, errOut io.Writer) error {
	if _, err := exec.LookPath("ssh"); err != nil {
		return errors.New("ssh was not found. --ssh uses the system OpenSSH client.\n" +
			"On Windows: Settings > System > Optional features > OpenSSH Client")
	}
	if o.command == "status" {
		step(out, "Board status (over SSH, %s)", o.ssh)
		cmd := exec.Command("ssh", sshArgs(o, remoteAgent+" status")...)
		cmd.Stdout, cmd.Stderr = out, errOut
		if err := cmd.Run(); err != nil {
			return sshError(o, err)
		}
		return nil
	}

	failed := 0
	for _, f := range o.files {
		src, err := os.Open(f)
		if err != nil {
			return fmt.Errorf("cannot read %s: %v", f, err)
		}
		step(out, "Sending %s over SSH to %s", filepath.Base(f), o.ssh)
		// One package per call: `import -` reads exactly one from stdin.
		cmd := exec.Command("ssh", sshArgs(o, remoteAgent+" import -")...)
		cmd.Stdin, cmd.Stdout, cmd.Stderr = src, out, errOut
		err = cmd.Run()
		src.Close()
		if err != nil {
			if isConnectFailure(err) {
				return sshError(o, err)
			}
			failed++
		}
	}
	if failed > 0 {
		return fmt.Errorf("%d package(s) were not installed (see above). The board keeps what it had", failed)
	}
	fmt.Fprintln(out, "\nDone.")
	return nil
}

func sshError(o options, err error) error {
	if isConnectFailure(err) {
		return fmt.Errorf("could not connect to %s over SSH.\n"+
			"  - Is %q the admin account on this board, with your key? (use -i to pick one)\n"+
			"  - Does the board answer on that address?", o.ssh, o.ssh)
	}
	return fmt.Errorf("the board's agent failed: %v", err)
}

// isConnectFailure reports whether err is ssh failing to reach or log in to
// the board, rather than the remote command failing. ssh reserves exit
// status 255 for its own errors.
func isConnectFailure(err error) bool {
	var ee *exec.ExitError
	return errors.As(err, &ee) && ee.ExitCode() == 255
}
