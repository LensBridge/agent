// Package selfupdate is the root side of an agent update
// (docs/architecture.md, section 12).
//
// The daemon runs unprivileged. When it has verified an agent package it only
// stages it (agent/staged/package.mbu plus a "ready" marker) in a directory it
// owns; a systemd path unit then starts `musallahboard-agent selfupdate apply`
// as root. Because everything in that directory is writable by the daemon,
// nothing there is trusted: the updater copies the package into a private
// directory first, verifies that copy from scratch against keys only root
// controls (trust.json and the keys compiled into the binary running now),
// and only then replaces /usr/bin/musallahboard-agent. A compromised daemon
// can therefore at worst make the updater refuse something.
//
// A new agent that does not come up and report its version within two
// minutes is rolled back, and its version is recorded so the board never
// tries it again.
package selfupdate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/LensBridge/agent/internal/fsutil"
	"github.com/LensBridge/agent/internal/mbu"
	"github.com/LensBridge/agent/internal/state"
	"github.com/LensBridge/agent/internal/store"
	"github.com/LensBridge/agent/internal/trust"
)

// Production paths and names.
const (
	DefaultBinary    = "/usr/bin/musallahboard-agent"
	DefaultNewBinary = "/usr/bin/.musallahboard-agent.new"
	DefaultPrev      = "/usr/lib/musallahboard/agent.prev"
	AgentUnit        = "musallahboard-agent.service"
	StatusURL        = "http://127.0.0.1:8080/api/local/status"
	HealthTimeout    = 120 * time.Second
)

// Outcome statuses written to last-update.json.
const (
	StatusOK         = "ok"
	StatusRolledBack = "rolled-back"
	StatusRefused    = "refused"
)

// Outcome is agent/last-update.json.
type Outcome struct {
	From    string `json:"from"`
	To      string `json:"to"`
	At      string `json:"at"`
	Status  string `json:"status"`
	Message string `json:"message"`
}

// Updater applies a staged agent package. Every outside effect is a field so
// the whole sequence can be tested in a temp directory without root.
type Updater struct {
	Layout    store.Layout
	TrustPath string
	Binary    string // the installed agent
	NewBinary string // where the new binary is extracted before the swap
	Prev      string // the previous agent, kept for rollback
	// TempRoot is where the private working directory is made ("" is the
	// system temp directory; under systemd's PrivateTmp that is private too).
	TempRoot string

	Arch           string
	CurrentVersion string

	// Run executes a command and returns its combined output.
	Run func(ctx context.Context, name string, args ...string) (string, error)
	// Healthy waits until the restarted daemon reports version, or fails.
	Healthy func(ctx context.Context, version string) error
	Now     func() time.Time
	// Logf reports progress (to the journal, via stdout).
	Logf func(format string, args ...any)
}

// New returns an Updater with the production paths, for the agent version
// running now.
func New(currentVersion string) *Updater {
	return &Updater{
		Layout:         store.Default(),
		TrustPath:      trust.DefaultPath,
		Binary:         DefaultBinary,
		NewBinary:      DefaultNewBinary,
		Prev:           DefaultPrev,
		Arch:           runtime.GOARCH,
		CurrentVersion: currentVersion,
		Run:            runCommand,
		Healthy:        func(ctx context.Context, v string) error { return WaitHealthy(ctx, StatusURL, v, HealthTimeout) },
		Now:            time.Now,
		Logf:           func(f string, a ...any) { fmt.Printf(f+"\n", a...) },
	}
}

// ErrNothingStaged means there was no staged package to apply.
var ErrNothingStaged = errors.New("no agent update is staged")

// refusal is a reason not to install that leaves the running agent alone.
type refusal struct{ msg string }

func (r *refusal) Error() string { return r.msg }

func refuse(format string, args ...any) error { return &refusal{fmt.Sprintf(format, args...)} }

// Apply installs the staged package, health-checks it and rolls back on
// failure. The returned Outcome has been written to last-update.json (unless
// the error is ErrNothingStaged).
func (u *Updater) Apply(ctx context.Context) (Outcome, error) {
	out := Outcome{From: u.CurrentVersion}
	// The marker goes first: whatever happens next, the path unit must not
	// start this again for the same staging.
	if err := os.Remove(u.Layout.StagedReady()); err != nil && !errors.Is(err, fs.ErrNotExist) {
		u.Logf("could not remove %s: %v", u.Layout.StagedReady(), err)
	}
	if _, err := os.Lstat(u.Layout.StagedPackage()); errors.Is(err, fs.ErrNotExist) {
		return out, ErrNothingStaged
	}

	err := u.apply(ctx, &out)
	var r *refusal
	switch {
	case err == nil:
		out.Status = StatusOK
		out.Message = fmt.Sprintf("Updated the agent from %s to %s", out.From, out.To)
	case errors.As(err, &r):
		out.Status = StatusRefused
		out.Message = r.msg
	default:
		out.Status = StatusRolledBack
		out.Message = err.Error()
	}
	out.At = u.Now().UTC().Format(time.RFC3339)
	u.Logf("%s: %s", out.Status, out.Message)
	if werr := u.writeOutcome(out); werr != nil {
		u.Logf("could not write %s: %v", u.Layout.AgentLastUpdate(), werr)
	}
	// Only now start the previous agent again: it reads the outcome when it
	// starts, to tell the people at the board what happened.
	if out.Status == StatusRolledBack {
		if o, rerr := u.Run(ctx, "systemctl", "restart", AgentUnit); rerr != nil {
			u.Logf("restarting the previous agent failed: %v %s", rerr, firstLine(o))
		}
	}
	return out, err
}

func (u *Updater) apply(ctx context.Context, out *Outcome) error {
	work, err := os.MkdirTemp(u.TempRoot, "musallahboard-selfupdate-")
	if err != nil {
		return refuse("cannot make a private working directory: %v", err)
	}
	defer os.RemoveAll(work)
	if err := os.Chmod(work, 0o700); err != nil {
		return refuse("cannot secure the working directory: %v", err)
	}

	// Copy first, verify the copy: the daemon owns the staged file and could
	// swap it between a check and a use.
	pkgPath := filepath.Join(work, "package.mbu")
	if err := copyStaged(u.Layout.StagedPackage(), pkgPath); err != nil {
		return refuse("cannot read the staged package: %v", err)
	}
	// Consumed; a leftover would only waste space.
	_ = os.Remove(u.Layout.StagedPackage())

	ring, err := trust.LoadRingRootOwned(u.TrustPath)
	if err != nil {
		return refuse("cannot read the trust store: %v", err)
	}
	pkg, err := mbu.Open(pkgPath, ring, mbu.OpenOptions{})
	if err != nil {
		return refuse("the staged package did not verify: %v", err)
	}
	defer pkg.Close()
	m := pkg.Manifest
	out.To = m.Version
	if m.Type != mbu.TypeAgent {
		return refuse("the staged package is a %s package, not an agent", m.Type)
	}
	if m.Agent.Arch != u.Arch {
		return refuse("agent %s is built for %s; this board is %s", m.Version, m.Agent.Arch, u.Arch)
	}
	if mbu.CompareVersions(m.Version, u.CurrentVersion) <= 0 {
		return refuse("agent %s is not newer than the installed %s", m.Version, u.CurrentVersion)
	}
	st, err := u.Layout.State().Load()
	if err != nil {
		return refuse("cannot read %s: %v", u.Layout.State().Path, err)
	}
	if st.AgentRejected(m.Version) {
		return refuse("agent %s failed to start on this board before; it will not be retried", m.Version)
	}

	u.Logf("verified agent %s (signed by %s); installing", m.Version, pkg.SignedBy)
	if err := u.extract(pkg, m.Agent.Binary); err != nil {
		os.Remove(u.NewBinary)
		return refuse("cannot write the new agent: %v", err)
	}
	vout, err := u.Run(ctx, u.NewBinary, "version")
	if err != nil || !strings.Contains(vout, m.Version) {
		os.Remove(u.NewBinary)
		return refuse("the new agent does not run on this board (`version` said %q, error %v)", firstLine(vout), err)
	}

	if err := os.MkdirAll(filepath.Dir(u.Prev), 0o755); err != nil {
		os.Remove(u.NewBinary)
		return refuse("cannot keep the current agent for rollback: %v", err)
	}
	if err := copyFileAtomic(u.Binary, u.Prev, 0o755); err != nil {
		os.Remove(u.NewBinary)
		return refuse("cannot keep the current agent for rollback: %v", err)
	}
	if err := os.Rename(u.NewBinary, u.Binary); err != nil {
		os.Remove(u.NewBinary)
		return refuse("cannot put the new agent in place: %v", err)
	}
	_ = fsutil.SyncDir(filepath.Dir(u.Binary))

	u.Logf("restarting %s", AgentUnit)
	if o, err := u.Run(ctx, "systemctl", "restart", AgentUnit); err != nil {
		return u.rollback(ctx, m.Version, fmt.Sprintf("restarting the agent failed: %v %s", err, firstLine(o)))
	}
	if err := u.Healthy(ctx, m.Version); err != nil {
		return u.rollback(ctx, m.Version, fmt.Sprintf("agent %s did not come up healthy: %v", m.Version, err))
	}
	return nil
}

// rollback restores the previous agent and records the version as rejected
// so the release channel or a USB stick cannot loop on it. Apply restarts
// the previous agent once the outcome is written.
func (u *Updater) rollback(ctx context.Context, version, reason string) error {
	u.Logf("%s; rolling back to %s", reason, u.CurrentVersion)
	msg := reason + "; rolled back to " + u.CurrentVersion
	if err := copyFileAtomic(u.Prev, u.Binary, 0o755); err != nil {
		msg += fmt.Sprintf(" (restoring the previous binary FAILED: %v)", err)
	}
	if _, err := u.Layout.State().Update(func(s *state.State) error {
		if !slices.Contains(s.AgentVersionsRejected, version) {
			s.AgentVersionsRejected = append(s.AgentVersionsRejected, version)
		}
		return nil
	}); err != nil {
		msg += fmt.Sprintf(" (could not record %s as rejected: %v)", version, err)
	}
	return errors.New(msg)
}

func (u *Updater) extract(pkg *mbu.Package, binary string) error {
	os.Remove(u.NewBinary)
	f, err := os.OpenFile(u.NewBinary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o700)
	if err != nil {
		return err
	}
	if err := pkg.CopyFile(binary, f); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	// Executable by everyone only once it is complete and verified.
	return os.Chmod(u.NewBinary, 0o755)
}

// ReadOutcome is the last outcome recorded in l, or nil if there is none.
func ReadOutcome(l store.Layout) *Outcome {
	raw, err := os.ReadFile(l.AgentLastUpdate())
	if err != nil {
		return nil
	}
	var o Outcome
	if json.Unmarshal(raw, &o) != nil || o.At == "" {
		return nil
	}
	return &o
}

func (u *Updater) writeOutcome(o Outcome) error {
	raw, err := json.MarshalIndent(o, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(u.Layout.AgentLastUpdate()), 0o755); err != nil {
		return err
	}
	return fsutil.WriteAtomic(u.Layout.AgentLastUpdate(), append(raw, '\n'), 0o644)
}

// copyStaged copies the daemon-owned staged package, refusing anything that
// is not a plain file (a symlink or FIFO planted by the daemon), and at most
// one byte more than a package may be.
func copyStaged(src, dst string) error {
	in, err := openRegularNoFollow(src)
	if err != nil {
		return err
	}
	defer in.Close()
	n, err := fsutil.CopyFileSync(dst, in, mbu.MaxPackageBytes+1, 0o600)
	if err != nil {
		return err
	}
	if n > mbu.MaxPackageBytes {
		return fmt.Errorf("the package is larger than %d MiB", mbu.MaxPackageBytes>>20)
	}
	return nil
}

// copyFileAtomic copies src over dst via a temp file beside dst and a rename,
// so dst is always either the old file or the complete new one. A running
// binary can be replaced this way; writing into it in place cannot.
func copyFileAtomic(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := filepath.Join(filepath.Dir(dst), "."+filepath.Base(dst)+".tmp")
	os.Remove(tmp)
	if _, err := fsutil.CopyFileSync(tmp, in, 1<<40, mode); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Chmod(tmp, mode); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		return err
	}
	return fsutil.SyncDir(filepath.Dir(dst))
}

func runCommand(ctx context.Context, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	return s
}

// WaitHealthy polls statusURL until it reports agentVersion == version, or
// timeout passes. The daemon needs a few seconds to start its local server;
// connection errors until then are expected.
func WaitHealthy(ctx context.Context, statusURL, version string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	client := &http.Client{Timeout: 5 * time.Second}
	last := "no answer"
	for {
		if v, err := probe(ctx, client, statusURL); err != nil {
			last = err.Error()
		} else if v == version {
			return nil
		} else {
			last = "it reports version " + v
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("after %s: %s", timeout, last)
		case <-time.After(2 * time.Second):
		}
	}
}

func probe(ctx context.Context, client *http.Client, statusURL string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, statusURL, nil)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("status returned %d", resp.StatusCode)
	}
	var st struct {
		AgentVersion string `json:"agentVersion"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&st); err != nil {
		return "", err
	}
	return st.AgentVersion, nil
}
