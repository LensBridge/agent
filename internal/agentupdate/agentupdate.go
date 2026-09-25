// Package agentupdate tells the people at a board how an agent self-update
// went (docs/architecture.md, section 12).
//
// An agent update spans two processes. The running agent stages the package,
// puts "Restarting MusallahBoard" on the update screen and leaves its batch's
// outcome in agent/pending-notice.json. The root updater then either refuses
// the package (this agent keeps running), or swaps the binary and starts the
// new agent, rolling back to this one if the new one does not come up. The
// updater records which in agent/last-update.json before it starts anything.
//
// So the outcome is shown by whichever agent runs afterwards:
//
//   - the new agent, at startup: "Update complete · Agent 0.3.0";
//   - this agent again, after a rollback, at startup: "Update not installed:
//     agent 0.3.0 did not start correctly, so the board went back to 0.2.1";
//   - this agent, still running, when the updater refused: Watch notices.
//
// Each outcome is shown once (state.json's agentUpdateSeen). When the batch
// had more behind the agent (queued in the inbox for the new agent), the
// screen is held until that finishes, so there is one screen, not two.
package agentupdate

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/LensBridge/agent/internal/fsutil"
	"github.com/LensBridge/agent/internal/notice"
	"github.com/LensBridge/agent/internal/selfupdate"
	"github.com/LensBridge/agent/internal/state"
	"github.com/LensBridge/agent/internal/store"
)

// Screen is the update screen (package updatescreen).
type Screen interface {
	OnScreen(ctx context.Context) bool
	Finish(ctx context.Context, n notice.Notice)
	Hold(ctx context.Context, n notice.Notice)
}

// Deps is what a Reporter needs.
type Deps struct {
	Layout  store.Layout
	Version string // this agent's
	Screen  Screen
	// Banner shows a notice over the running board.
	Banner func(notice.Notice)
	// Resume lets the inbox take work again after a refused update.
	Resume func()
	Logger *slog.Logger
	Now    func() time.Time
}

// Reporter shows agent update outcomes.
type Reporter struct {
	d Deps
	// watchTimeout bounds how long Watch waits for the root updater.
	watchTimeout, watchPoll time.Duration
}

// New returns a Reporter.
func New(d Deps) *Reporter {
	if d.Logger == nil {
		d.Logger = slog.New(slog.DiscardHandler)
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	return &Reporter{d: d, watchTimeout: 3 * time.Minute, watchPoll: 2 * time.Second}
}

// LastOutcome is the root updater's last outcome, if any.
func (r *Reporter) LastOutcome() *selfupdate.Outcome { return selfupdate.ReadOutcome(r.d.Layout) }

// Staged is called after a batch staged an agent (importer.Deps.AgentStaged):
// it leaves the batch's outcome for the next agent, and watches for the
// updater refusing the package, in which case nobody else would say so.
func (r *Reporter) Staged(n notice.Notice) {
	raw, _ := json.Marshal(n)
	if err := fsutil.WriteAtomic(r.d.Layout.AgentPendingNotice(), raw, 0o640); err != nil {
		r.d.Logger.Warn("could not keep the update outcome for the new agent", "err", err)
	}
	go r.watch(context.Background(), r.d.Now())
}

// watch waits for the updater's verdict on a package staged at since. A new
// agent or a rollback restarts this process, so the only verdict this agent
// lives to see is a refusal; if none comes at all, the update did not run.
func (r *Reporter) watch(ctx context.Context, since time.Time) {
	deadline := time.Now().Add(r.watchTimeout)
	for time.Now().Before(deadline) {
		if o := r.LastOutcome(); o != nil && o.Status == selfupdate.StatusRefused && !before(o.At, since) {
			r.markSeen(o.At)
			r.conclude(ctx, r.failure(*o, r.takePending()))
			r.d.Resume()
			return
		}
		time.Sleep(r.watchPoll)
	}
	r.d.Logger.Error("the agent update did not run: no outcome from the root updater",
		"path", r.d.Layout.AgentLastUpdate())
	pending := r.takePending()
	n := notice.New(notice.Problem, "Update not installed",
		append([]string{"The agent update did not start"}, withoutAgent(pending, "")...)...)
	r.conclude(ctx, n)
	r.d.Resume()
}

// Startup shows the outcome of an agent update that ended in this process
// starting: the new agent after a success, or this one after a rollback. Run
// it before the inbox, which may hold the rest of the batch.
func (r *Reporter) Startup(ctx context.Context) {
	o := r.LastOutcome()
	st, _ := r.d.Layout.State().Load()
	unseen := o != nil && o.At != st.AgentUpdateSeen
	if unseen {
		r.markSeen(o.At)
	}
	pending := r.takePending()
	onScreen := r.d.Screen.OnScreen(ctx)

	var n notice.Notice
	switch {
	case unseen && o.Status != selfupdate.StatusOK:
		n = r.failure(*o, pending)
	case pending != nil:
		n = *pending
	case onScreen:
		// The screen was up with no record of why (an agent that crashed
		// mid-update, say). Close it honestly.
		n = notice.New(notice.OK, "Update complete", "Agent "+r.d.Version)
	default:
		return
	}
	r.d.Logger.Info("agent update outcome", "tone", n.Tone, "headline", n.Headline, "lines", n.Lines)
	if !onScreen {
		r.d.Banner(n)
		return
	}
	r.conclude(ctx, n)
}

// conclude finishes the screen with n, or holds it with n while the batch
// queued behind the agent installs.
func (r *Reporter) conclude(ctx context.Context, n notice.Notice) {
	if r.queued() {
		r.d.Screen.Hold(ctx, n)
		return
	}
	r.d.Screen.Finish(ctx, n)
}

// failure is the notice for an update that did not replace the agent. The
// lines from its batch that are not about the agent still stand.
func (r *Reporter) failure(o selfupdate.Outcome, pending *notice.Notice) notice.Notice {
	var line string
	switch {
	case o.Status == selfupdate.StatusRolledBack && o.To != "":
		line = "Agent " + o.To + " did not start correctly, so the board went back to " + o.From
	case o.To != "":
		line = "Agent " + o.To + " could not be installed on this board"
	default:
		line = "The agent update could not be installed"
	}
	return notice.New(notice.Problem, "Update not installed", append([]string{line}, withoutAgent(pending, o.To)...)...)
}

// withoutAgent is a pending notice's lines, less the one announcing the agent
// that did not, after all, install.
func withoutAgent(n *notice.Notice, version string) []string {
	if n == nil {
		return nil
	}
	var out []string
	for _, l := range n.Lines {
		if strings.HasPrefix(l, "Agent ") && (version == "" || l == "Agent "+version) {
			continue
		}
		out = append(out, l)
	}
	return out
}

func (r *Reporter) takePending() *notice.Notice {
	path := r.d.Layout.AgentPendingNotice()
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	os.Remove(path)
	var n notice.Notice
	if json.Unmarshal(raw, &n) != nil || n.Headline == "" {
		return nil
	}
	return &n
}

func (r *Reporter) markSeen(at string) {
	if _, err := r.d.Layout.State().Update(func(s *state.State) error {
		s.AgentUpdateSeen = at
		return nil
	}); err != nil {
		r.d.Logger.Warn("could not record the agent update as shown", "err", err)
	}
}

// queued reports whether packages wait in the inbox (behind an agent update).
func (r *Reporter) queued() bool {
	entries, err := os.ReadDir(r.d.Layout.Inbox())
	if errors.Is(err, fs.ErrNotExist) {
		return false
	}
	for _, e := range entries {
		n := e.Name()
		if e.Type().IsRegular() && !strings.HasPrefix(n, ".") && filepath.Ext(n) == ".mbu" {
			return true
		}
	}
	return false
}

// before reports whether the RFC 3339 time at is earlier than t, allowing a
// little for the updater's clock reading being taken in another process.
func before(at string, t time.Time) bool {
	parsed, err := time.Parse(time.RFC3339, at)
	return err != nil || parsed.Before(t.Add(-5*time.Second))
}
