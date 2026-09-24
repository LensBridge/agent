package main

import (
	"bytes"
	"errors"
	"io"
	"os"
	"os/exec"
)

// remote runs commands on the board through the system ssh, so the user's
// existing keys, agent and config apply unchanged.
type remote struct {
	host     string
	user     string
	identity string
	// checkHostKey keeps ssh's normal known_hosts checking. See sshOptions.
	checkHostKey bool

	stdout, stderr io.Writer
}

func (r remote) target() string { return r.user + "@" + r.host }

// sshOptions are the options for every ssh call.
//
// Every board answers on the same address, 10.77.0.1, with its own host key,
// so with normal checking the second board an admin visits fails with
// "REMOTE HOST IDENTIFICATION HAS CHANGED" and a scary instruction to edit
// known_hosts. On the default service-port address the link is a cable from
// this laptop straight into the board, so host keys are not recorded or
// checked there. Any other --host keeps ssh's normal behaviour, and
// --check-host-key restores it for the default too.
func (r remote) sshOptions() []string {
	opts := []string{"-o", "ConnectTimeout=10", "-o", "ServerAliveInterval=15"}
	if r.identity != "" {
		opts = append(opts, "-i", r.identity)
	}
	if !r.checkHostKey {
		opts = append(opts,
			"-o", "StrictHostKeyChecking=no",
			"-o", "UserKnownHostsFile="+os.DevNull,
			"-o", "LogLevel=ERROR",
		)
	}
	return opts
}

func (r remote) sshArgs(command string) []string {
	return append(r.sshOptions(), r.target(), command)
}

// run executes command on the board, streaming its output.
func (r remote) run(command string) error {
	cmd := exec.Command("ssh", r.sshArgs(command)...)
	cmd.Stdout, cmd.Stderr = r.stdout, r.stderr
	return cmd.Run()
}

// output executes command on the board and returns its stdout.
func (r remote) output(command string) (string, error) {
	var out bytes.Buffer
	cmd := exec.Command("ssh", r.sshArgs(command)...)
	cmd.Stdout, cmd.Stderr = &out, r.stderr
	err := cmd.Run()
	return out.String(), err
}

// runWithInput executes command on the board with the local file on its
// stdin, streaming its output. This is how files reach the gate: the push
// account cannot run scp or sftp.
func (r remote) runWithInput(command, local string) error {
	f, err := os.Open(local)
	if err != nil {
		return err
	}
	defer f.Close()
	cmd := exec.Command("ssh", r.sshArgs(command)...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = f, r.stdout, r.stderr
	return cmd.Run()
}

// isConnectFailure reports whether err is ssh failing to reach or log
// in to the board, rather than the remote command failing. ssh reserves exit
// status 255 for its own errors.
func isConnectFailure(err error) bool {
	var ee *exec.ExitError
	return errors.As(err, &ee) && ee.ExitCode() == 255
}
