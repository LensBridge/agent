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
// Replacing the binary is a transaction with a root-owned journal (txn.go),
// so a crash or power cut at any step is finished by the next run. The new
// agent then has a probation (probation.go): it must come up, stay up without
// a restart for several minutes and, if the board was connected to LensBridge
// before, connect again. If it does not, the previous agent is restored and
// the version is recorded so the board does not try it again.
package selfupdate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/LensBridge/agent/internal/core/fsutil"
	"github.com/LensBridge/agent/internal/core/mbu"
	"github.com/LensBridge/agent/internal/core/state"
	"github.com/LensBridge/agent/internal/core/store"
	"github.com/LensBridge/agent/internal/core/trust"
)

// Production paths and names.
const (
	DefaultBinary    = "/usr/bin/musallahboard-agent"
	DefaultNewBinary = "/usr/bin/.musallahboard-agent.new"
	DefaultPrev      = "/usr/lib/musallahboard/agent.prev"
	// DefaultTxn is the update record (txn.go). Root-owned, beside
	// agent.prev, where the daemon cannot write.
	DefaultTxn = "/usr/lib/musallahboard/update-in-progress.json"
	// DefaultStrikes counts connection-only failures per version.
	DefaultStrikes = "/usr/lib/musallahboard/update-strikes.json"
	AgentUnit      = "musallahboard-agent.service"
	StatusURL      = "http://127.0.0.1:8080/api/local/status"
)

// Outcome statuses written to last-update.json.
const (
	StatusOK         = "ok"
	StatusRolledBack = "rolled-back"
	StatusRefused    = "refused"
	// StatusRollbackFailed: the new agent failed its probation but the
	// previous one could not be restored, so the new one still runs.
	StatusRollbackFailed = "rollback-failed"
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
	Txn       string // the update record (txn.go)
	Strikes   string // connection-only failures per version
	// TempRoot is where the private working directory is made ("" is the
	// system temp directory; under systemd's PrivateTmp that is private too).
	TempRoot string

	Arch           string
	CurrentVersion string

	// Run executes a command and returns its combined output.
	Run func(ctx context.Context, name string, args ...string) (string, error)
	// Status reads the running agent's /api/local/status.
	Status func(ctx context.Context) (AgentStatus, error)
	Now    func() time.Time
	// Sleep waits d or until ctx ends.
	Sleep func(ctx context.Context, d time.Duration) error
	// Logf reports progress (to the journal, via stdout).
	Logf func(format string, args ...any)

	// Probation timing (probation.go); zero means the default.
	ComeUpTimeout, ResumeComeUpTimeout, BackendTimeout time.Duration
	ProbationPeriod, FinalCheckGrace, PollInterval     time.Duration

	// crash, in tests, is called at each step of the transaction; true
	// simulates a power cut there: nothing more runs or is cleaned up.
	crash      func(step string) bool
	poweredOff bool
}

// New returns an Updater with the production paths, for the agent version
// running now.
func New(currentVersion string) *Updater {
	u := &Updater{
		Layout:         store.Default(),
		TrustPath:      trust.DefaultPath,
		Binary:         DefaultBinary,
		NewBinary:      DefaultNewBinary,
		Prev:           DefaultPrev,
		Txn:            DefaultTxn,
		Strikes:        DefaultStrikes,
		Arch:           runtime.GOARCH,
		CurrentVersion: currentVersion,
		Run:            runCommand,
		Status:         func(ctx context.Context) (AgentStatus, error) { return FetchStatus(ctx, StatusURL) },
		Now:            time.Now,
		Sleep:          sleepCtx,
		Logf:           func(f string, a ...any) { fmt.Printf(f+"\n", a...) },
	}
	u.defaults()
	return u
}

func (u *Updater) defaults() {
	set := func(d *time.Duration, v time.Duration) {
		if *d == 0 {
			*d = v
		}
	}
	set(&u.ComeUpTimeout, ComeUpTimeout)
	set(&u.ResumeComeUpTimeout, ResumeComeUpTimeout)
	set(&u.BackendTimeout, BackendTimeout)
	set(&u.ProbationPeriod, ProbationPeriod)
	set(&u.FinalCheckGrace, FinalCheckGrace)
	set(&u.PollInterval, PollInterval)
	if u.Txn == "" {
		u.Txn = filepath.Join(filepath.Dir(u.Prev), "update-in-progress.json")
	}
	if u.Strikes == "" {
		u.Strikes = filepath.Join(filepath.Dir(u.Prev), "update-strikes.json")
	}
	if u.Sleep == nil {
		u.Sleep = sleepCtx
	}
}

// PowerCut is what a simulated power cut panics with (tests only).
type PowerCut struct{ Step string }

func (u *Updater) crashpoint(step string) {
	if u.crash != nil && u.crash(step) {
		u.poweredOff = true
		panic(PowerCut{step})
	}
}

// ErrNothingStaged means there was no staged package to apply and no
// unfinished update to resolve.
var ErrNothingStaged = errors.New("no agent update is staged")

// refusal is a reason not to install that leaves the running agent alone.
type refusal struct{ msg string }

func (r *refusal) Error() string { return r.msg }

func refuse(format string, args ...any) error { return &refusal{fmt.Sprintf(format, args...)} }

// Apply first finishes an update a previous run left unfinished, if there is
// one, and stops there. Otherwise it installs the staged package, puts the
// new agent on probation and commits it or rolls it back. The returned
// Outcome has been written to last-update.json, except with ErrNothingStaged
// and ErrInterrupted (stopped mid-probation: the record stays, and the next
// run resumes it).
func (u *Updater) Apply(ctx context.Context) (out Outcome, err error) {
	u.defaults()
	// The marker goes first: the path unit must not start this again for
	// the same staging. settleReady puts it back if something is still
	// waiting when we finish.
	if err := os.Remove(u.Layout.StagedReady()); err != nil && !errors.Is(err, fs.ErrNotExist) {
		u.Logf("could not remove %s: %v", u.Layout.StagedReady(), err)
	}
	defer func() {
		if u.poweredOff {
			return
		}
		if errors.Is(err, ErrInterrupted) {
			// Keep a trigger for the next run (older path units watch
			// only `ready`; newer ones also watch the record).
			u.markReady()
			return
		}
		u.settleReady()
	}()

	rec, lerr := loadTxn(u.Txn)
	switch {
	case lerr == nil:
		u.Logf("resuming the unfinished update from agent %s to %s (%s)", rec.From, rec.To, rec.Phase)
		return u.resume(ctx, rec)
	case !errors.Is(lerr, fs.ErrNotExist):
		return u.unreadableTxn(ctx, lerr)
	}
	if _, err := os.Lstat(u.Layout.StagedPackage()); errors.Is(err, fs.ErrNotExist) {
		return Outcome{From: u.CurrentVersion}, ErrNothingStaged
	}

	out = Outcome{From: u.CurrentVersion}
	rec, err = u.prepare(ctx, &out)
	if err != nil {
		var r *refusal
		out.Status = StatusRefused
		if errors.As(err, &r) {
			out.Message = r.msg
		} else {
			out.Message = err.Error()
		}
		err := u.finish(&out)
		return out, err
	}
	return u.swap(ctx, rec)
}

// finish writes a final outcome and returns it as an error unless it is a
// success.
func (u *Updater) finish(out *Outcome) error {
	if out.At == "" {
		out.At = u.Now().UTC().Format(time.RFC3339)
	}
	u.Logf("%s: %s", out.Status, out.Message)
	if werr := u.writeOutcome(*out); werr != nil {
		u.Logf("could not write %s: %v", u.Layout.AgentLastUpdate(), werr)
	}
	if out.Status == StatusOK {
		return nil
	}
	return errors.New(out.Message)
}

// prepare verifies the staged package, extracts the new binary, keeps the
// running one in agent.prev and writes the update record. Every failure is
// a refusal: nothing installed has changed.
func (u *Updater) prepare(ctx context.Context, out *Outcome) (*Txn, error) {
	work, err := os.MkdirTemp(u.TempRoot, "musallahboard-selfupdate-")
	if err != nil {
		return nil, refuse("cannot make a private working directory: %v", err)
	}
	defer os.RemoveAll(work)
	if err := os.Chmod(work, 0o700); err != nil {
		return nil, refuse("cannot secure the working directory: %v", err)
	}

	// Copy first, verify the copy: the daemon owns the staged file and could
	// swap it between a check and a use.
	pkgPath := filepath.Join(work, "package.mbu")
	err = copyStaged(u.Layout.StagedPackage(), pkgPath)
	// Consumed, or unusable. Either way it must go: a package left staged
	// gets its `ready` marker back (settleReady), which would start this
	// again at once.
	_ = os.Remove(u.Layout.StagedPackage())
	if err != nil {
		return nil, refuse("cannot read the staged package: %v", err)
	}

	ring, err := trust.LoadRingRootOwned(u.TrustPath)
	if err != nil {
		return nil, refuse("cannot read the trust store: %v", err)
	}
	pkg, err := mbu.Open(pkgPath, ring, mbu.OpenOptions{})
	if err != nil {
		return nil, refuse("the staged package did not verify: %v", err)
	}
	defer pkg.Close()
	m := pkg.Manifest
	out.To = m.Version
	if m.Type != mbu.TypeAgent {
		return nil, refuse("the staged package is a %s package, not an agent", m.Type)
	}
	if m.Agent.Arch != u.Arch {
		return nil, refuse("agent %s is built for %s; this board is %s", m.Version, m.Agent.Arch, u.Arch)
	}
	if mbu.CompareVersions(m.Version, u.CurrentVersion) <= 0 {
		return nil, refuse("agent %s is not newer than the installed %s", m.Version, u.CurrentVersion)
	}
	st, err := u.Layout.State().Load()
	if err != nil {
		return nil, refuse("cannot read %s: %v", u.Layout.State().Path, err)
	}
	if st.AgentRejected(m.Version) {
		return nil, refuse("agent %s failed to start on this board before; it will not be retried", m.Version)
	}

	u.Logf("verified agent %s (signed by %s); installing", m.Version, pkg.SignedBy)
	if err := u.extract(pkg, m.Agent.Binary); err != nil {
		os.Remove(u.NewBinary)
		return nil, refuse("cannot write the new agent: %v", err)
	}
	fail := func(format string, args ...any) (*Txn, error) {
		os.Remove(u.NewBinary)
		return nil, refuse(format, args...)
	}
	vout, err := u.Run(ctx, u.NewBinary, "version")
	if err != nil || !strings.Contains(vout, m.Version) {
		return fail("the new agent does not run on this board (`version` said %q, error %v)", firstLine(vout), err)
	}

	if err := os.MkdirAll(filepath.Dir(u.Prev), 0o755); err != nil {
		return fail("cannot keep the current agent for rollback: %v", err)
	}
	if err := copyFileAtomic(u.Binary, u.Prev, 0o755); err != nil {
		return fail("cannot keep the current agent for rollback: %v", err)
	}
	cur, err1 := fileSHA256(u.Binary)
	prev, err2 := fileSHA256(u.Prev)
	next, err3 := fileSHA256(u.NewBinary)
	if err := errors.Join(err1, err2, err3); err != nil {
		return fail("cannot fingerprint the agents: %v", err)
	}
	if cur != prev {
		return fail("the copy of the current agent kept for rollback does not match it")
	}

	// Whether the board is connected now decides whether the new agent must
	// connect to pass. The agent answering is the one about to be replaced.
	backend := false
	if s, err := u.Status(ctx); err != nil {
		u.Logf("could not read the running agent's status (%v); the new agent will not be required to connect to LensBridge", err)
	} else {
		backend = s.BackendUp()
		u.Logf("connected to LensBridge before the update: %v", backend)
	}

	rec := &Txn{
		Phase: PhaseSwapping, From: u.CurrentVersion, To: m.Version,
		PrevSHA256: prev, NewSHA256: next, BackendBefore: backend,
		StartedAt: u.Now().UTC().Format(time.RFC3339Nano),
	}
	if err := saveTxn(u.Txn, rec); err != nil {
		removeDurable(u.Txn)
		return fail("cannot record the update before installing it: %v", err)
	}
	return rec, nil
}

// swap puts the new binary in place and starts its probation.
func (u *Updater) swap(ctx context.Context, rec *Txn) (Outcome, error) {
	u.crashpoint("recorded")
	// From here a power cut must lead to a run that finishes the update.
	// The record (watched by current path units) and `ready` (watched by
	// every path unit since the first release) both start one at boot.
	u.markReady()
	u.crashpoint("marked")
	if err := os.Rename(u.NewBinary, u.Binary); err != nil {
		os.Remove(u.NewBinary)
		if rerr := removeDurable(u.Txn); rerr != nil {
			u.Logf("could not remove %s: %v", u.Txn, rerr)
		}
		out := Outcome{From: rec.From, To: rec.To, Status: StatusRefused,
			Message: fmt.Sprintf("cannot put the new agent in place: %v", err)}
		err := u.finish(&out)
		return out, err
	}
	_ = fsutil.SyncDir(filepath.Dir(u.Binary))
	u.crashpoint("swapped")
	return u.startProbation(ctx, rec, false)
}

// startProbation records the probation, restarts the agent unless resuming,
// judges it and commits or rolls back.
func (u *Updater) startProbation(ctx context.Context, rec *Txn, resumed bool) (Outcome, error) {
	rec.Phase = PhaseProbation
	rec.Attempts++
	if rec.Attempts > MaxProbationAttempts {
		return u.rollBack(ctx, rec, fmt.Sprintf("agent %s could not be judged: its probation was cut short %d times",
			rec.To, rec.Attempts-1), true)
	}
	if err := saveTxn(u.Txn, rec); err != nil {
		u.Logf("could not update %s: %v", u.Txn, err)
	}
	u.crashpoint("probation")

	restart := !resumed
	if resumed {
		// A run that stopped after the swap but before the restart, then
		// resumed without a reboot, leaves the previous agent running. Only an
		// agent that already reports the new version is left alone; one that
		// is still starting is the installed (new) binary, so restarting it
		// costs nothing.
		if st, err := u.Status(ctx); err != nil || st.AgentVersion != rec.To {
			restart = true
		}
	}
	if restart {
		u.Logf("restarting %s", AgentUnit)
		if o, err := u.Run(ctx, "systemctl", "restart", AgentUnit); err != nil {
			if ctx.Err() != nil {
				return Outcome{From: rec.From, To: rec.To}, ErrInterrupted
			}
			return u.rollBack(ctx, rec, fmt.Sprintf("restarting the agent failed: %v %s", err, firstLine(o)), true)
		}
	}
	v, err := u.probation(ctx, rec, resumed)
	if err != nil {
		u.Logf("%v; the next run resumes the update", err)
		return Outcome{From: rec.From, To: rec.To, Message: err.Error()}, err
	}
	if v != nil {
		return u.rollBack(ctx, rec, v.reason, v.reject)
	}
	return u.commit(ctx, rec)
}

// commit makes a passed update final.
func (u *Updater) commit(ctx context.Context, rec *Txn) (Outcome, error) {
	if rec.Phase != PhaseCommitting {
		rec.Phase = PhaseCommitting
		rec.OutcomeAt = u.Now().UTC().Format(time.RFC3339)
		if err := saveTxn(u.Txn, rec); err != nil {
			u.Logf("could not update %s: %v", u.Txn, err)
		}
	}
	u.crashpoint("committing")
	out := Outcome{From: rec.From, To: rec.To, At: rec.OutcomeAt, Status: StatusOK,
		Message: fmt.Sprintf("Updated the agent from %s to %s", rec.From, rec.To)}
	err := u.finish(&out)
	u.crashpoint("committed")
	if rerr := removeDurable(u.Txn); rerr != nil {
		u.Logf("could not remove %s: %v", u.Txn, rerr)
	}
	return out, err
}

// rollBack restores the previous agent, records the version as rejected (or,
// for a connection-only failure, a strike against it) so the release channel
// or a USB stick cannot loop on it, writes the outcome and only then
// restarts the previous agent, which reads the outcome when it starts. Every
// step can be repeated, so a run that resumes it simply does it again.
func (u *Updater) rollBack(ctx context.Context, rec *Txn, reason string, reject bool) (Outcome, error) {
	// Once begun, a rollback finishes: a stop request does not cut it short.
	ctx = context.WithoutCancel(ctx)
	if rec.Phase != PhaseRollingBack {
		rec.Phase, rec.Reason, rec.Reject = PhaseRollingBack, reason, reject
		rec.OutcomeAt = u.Now().UTC().Format(time.RFC3339)
		if err := saveTxn(u.Txn, rec); err != nil {
			u.Logf("could not update %s: %v", u.Txn, err)
		}
	}
	u.crashpoint("rolling-back")
	u.Logf("%s; rolling back to %s", rec.Reason, rec.From)

	out := Outcome{From: rec.From, To: rec.To, At: rec.OutcomeAt, Status: StatusRolledBack,
		Message: rec.Reason + "; rolled back to " + rec.From}
	if err := u.restorePrev(ctx, rec); err != nil {
		out.Status = StatusRollbackFailed
		out.Message = fmt.Sprintf("%s; restoring agent %s FAILED (%v), so %s still runs", rec.Reason, rec.From, err, rec.To)
	}
	u.crashpoint("restored")

	reject = rec.Reject
	if !reject {
		n, err := addStrike(u.Strikes, rec.To, rec.StartedAt)
		switch {
		case err != nil:
			u.Logf("could not record the failure in %s: %v", u.Strikes, err)
		case n >= StrikesToReject:
			reject = true
			out.Message += fmt.Sprintf(" (failed this way %d times; not retried)", n)
		default:
			out.Message += " (it will be tried again)"
		}
	}
	if reject {
		if _, err := u.Layout.State().Update(func(s *state.State) error {
			if !slices.Contains(s.AgentVersionsRejected, rec.To) {
				s.AgentVersionsRejected = append(s.AgentVersionsRejected, rec.To)
			}
			return nil
		}); err != nil {
			out.Message += fmt.Sprintf(" (could not record %s as rejected: %v)", rec.To, err)
		}
	}
	err := u.finish(&out)
	u.crashpoint("rolled-back")
	if rerr := removeDurable(u.Txn); rerr != nil {
		u.Logf("could not remove %s: %v", u.Txn, rerr)
	}
	u.crashpoint("closed")
	if o, rerr := u.Run(ctx, "systemctl", "restart", AgentUnit); rerr != nil {
		u.Logf("restarting the agent failed: %v %s", rerr, firstLine(o))
	}
	return out, err
}

// restorePrev puts agent.prev back in place, if it is not there already, and
// only if agent.prev is still the agent that ran before the update.
func (u *Updater) restorePrev(ctx context.Context, rec *Txn) error {
	var err error
	for try := 0; try < 3; try++ {
		if try > 0 {
			_ = u.Sleep(ctx, 2*time.Second)
		}
		if h, herr := fileSHA256(u.Binary); herr == nil && h == rec.PrevSHA256 {
			return nil
		}
		h, herr := fileSHA256(u.Prev)
		switch {
		case herr != nil:
			err = herr
		case h != rec.PrevSHA256:
			return fmt.Errorf("%s is not the agent that ran before the update", u.Prev)
		default:
			err = copyFileAtomic(u.Prev, u.Binary, 0o755)
			if err == nil {
				if h, herr := fileSHA256(u.Binary); herr == nil && h == rec.PrevSHA256 {
					return nil
				}
				err = errors.New("the restored binary does not match agent.prev")
			}
		}
	}
	return err
}

// resume finishes a transaction a previous run left behind.
func (u *Updater) resume(ctx context.Context, rec *Txn) (Outcome, error) {
	switch rec.Phase {
	case PhaseCommitting:
		return u.commit(ctx, rec)
	case PhaseRollingBack:
		return u.rollBack(ctx, rec, rec.Reason, rec.Reject)
	}
	h, err := fileSHA256(u.Binary)
	switch {
	case err == nil && h == rec.NewSHA256 && (rec.Phase == PhaseSwapping || rec.Phase == PhaseProbation):
		// The new agent is in place and was not judged: judge it now.
		return u.startProbation(ctx, rec, true)
	case err == nil && h == rec.NewSHA256:
		return u.rollBack(ctx, rec, fmt.Sprintf("the update record is in an unknown state (%q)", rec.Phase), true)
	case err == nil && h == rec.PrevSHA256:
		// Interrupted before the swap: nothing changed.
		os.Remove(u.NewBinary)
		if rerr := removeDurable(u.Txn); rerr != nil {
			u.Logf("could not remove %s: %v", u.Txn, rerr)
		}
		out := Outcome{From: rec.From, To: rec.To, Status: StatusRefused,
			Message: fmt.Sprintf("the update to agent %s was interrupted before the new agent was put in place; agent %s still runs", rec.To, rec.From)}
		err := u.finish(&out)
		return out, err
	default:
		// Something else was installed since (by hand, or setup.sh). It
		// is not ours to judge or undo.
		if err == nil {
			err = errors.New("it is neither the previous nor the new agent")
		}
		os.Remove(u.NewBinary)
		if rerr := removeDurable(u.Txn); rerr != nil {
			u.Logf("could not remove %s: %v", u.Txn, rerr)
		}
		out := Outcome{From: rec.From, To: rec.To, Status: StatusRefused,
			Message: fmt.Sprintf("the update to agent %s was interrupted, and the installed agent was changed since (%v); it is left as it is", rec.To, err)}
		err := u.finish(&out)
		return out, err
	}
}

// unreadableTxn handles a record that cannot be parsed (it is written
// atomically, so only a damaged disk does this). An update was under way, so
// the agent that is known to have worked, agent.prev, is put back.
func (u *Updater) unreadableTxn(ctx context.Context, lerr error) (Outcome, error) {
	u.Logf("%v", lerr)
	out := Outcome{Status: StatusRolledBack, Message: "an unfinished agent update could not be read back; the previous agent was put back"}
	prev, perr := fileSHA256(u.Prev)
	cur, cerr := fileSHA256(u.Binary)
	switch {
	case perr != nil:
		out.Status = StatusRollbackFailed
		out.Message = fmt.Sprintf("an unfinished agent update could not be read back, and there is no previous agent to return to (%v)", perr)
	case cerr == nil && cur == prev:
		out.Message = "an unfinished agent update could not be read back; the previous agent is in place"
	default:
		if err := copyFileAtomic(u.Prev, u.Binary, 0o755); err != nil {
			out.Status = StatusRollbackFailed
			out.Message = fmt.Sprintf("an unfinished agent update could not be read back, and putting the previous agent back failed: %v", err)
		}
	}
	err := u.finish(&out)
	if rerr := removeDurable(u.Txn); rerr != nil {
		u.Logf("could not remove %s: %v", u.Txn, rerr)
	}
	if o, rerr := u.Run(context.WithoutCancel(ctx), "systemctl", "restart", AgentUnit); rerr != nil {
		u.Logf("restarting the agent failed: %v %s", rerr, firstLine(o))
	}
	return out, err
}

// markReady (re)creates the `ready` marker, so the path unit starts the
// updater again. The marker carries no trust; it only triggers a run.
func (u *Updater) markReady() {
	if err := fsutil.WriteAtomic(u.Layout.StagedReady(), []byte("resume\n"), 0o640); err != nil {
		u.Logf("could not write %s: %v", u.Layout.StagedReady(), err)
	}
}

// settleReady removes `ready`, then puts it back if a package is staged (the
// daemon may have staged another while this ran). Removing before looking
// means a staging that lands in between always leaves its own marker.
func (u *Updater) settleReady() {
	if err := os.Remove(u.Layout.StagedReady()); err != nil && !errors.Is(err, fs.ErrNotExist) {
		u.Logf("could not remove %s: %v", u.Layout.StagedReady(), err)
	}
	if _, err := os.Lstat(u.Layout.StagedPackage()); err == nil {
		u.markReady()
	}
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
