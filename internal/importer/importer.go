// Package importer is the one path every package takes onto a board
// (docs/architecture.md, section 8), whatever carried it: backend sync, a USB
// stick, the service-port upload server or the admin CLI.
//
// For each package it verifies (mbu.Open against the trust ring), decides
// against the board's high-water marks, and installs content and app packages
// itself or stages an agent package for the root self-updater. Batches are
// serialised: only one import runs at a time.
package importer

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/LensBridge/agent/internal/fsutil"
	"github.com/LensBridge/agent/internal/mbu"
	"github.com/LensBridge/agent/internal/notice"
	"github.com/LensBridge/agent/internal/state"
	"github.com/LensBridge/agent/internal/store"
	"github.com/LensBridge/agent/internal/trust"
)

// LocalAPIVersion is the local kiosk API this agent serves (section 7). An app
// package needing a newer one is refused.
const LocalAPIVersion = 2

// Source is where a batch came from.
type Source string

const (
	SourceSync   Source = "sync"
	SourceUSB    Source = "usb"
	SourceUpload Source = "upload"
	SourceCLI    Source = "cli"
)

// Actions a package can end in.
const (
	ActionInstalled = "installed"
	ActionStaged    = "staged"
	// ActionUnchanged: exactly this is already installed.
	ActionUnchanged = "unchanged"
	// ActionOutdated: the board already has something newer. Not a failure:
	// an old USB stick at an online board is the normal case.
	ActionOutdated = "outdated"
	// ActionSkipped: content for another board. One stick serves many.
	ActionSkipped  = "skipped"
	ActionRejected = "rejected"
	ActionQueued   = "queued"
)

// Result is the outcome for one package.
type Result struct {
	File     string   `json:"file"`
	Type     mbu.Type `json:"type,omitempty"`
	Version  string   `json:"version,omitempty"`
	Sequence int64    `json:"sequence,omitempty"`
	Action   string   `json:"action"`
	// Message is one plain sentence for whoever is at the board: it is what
	// the update screen, the banner, the upload page and the CLI show.
	Message string `json:"message"`
	// Detail is the technical reason behind a rejection, for logs and for
	// the people reading them.
	Detail string `json:"detail,omitempty"`
}

// Batch is the outcome of one import.
type Batch struct {
	Results []Result `json:"results"`
	// Notice is what the board showed about the batch.
	Notice notice.Notice `json:"notice"`
	// Verified reports whether at least one package authenticated. The
	// upload server only trusts an uploader's clock when it did.
	Verified bool `json:"-"`
	// MaxCreatedAt is the newest createdAt among authenticated packages.
	MaxCreatedAt time.Time `json:"-"`
	// AgentStaged means a new agent is about to replace this one.
	AgentStaged bool `json:"-"`
}

// OK reports whether nothing was rejected.
func (b Batch) OK() bool {
	for _, r := range b.Results {
		if r.Action == ActionRejected {
			return false
		}
	}
	return true
}

// Screen is the kiosk's update screen (package updatescreen).
type Screen interface {
	// Active reports whether the screen is up, possibly held over from
	// before an agent restart for this batch to finish.
	Active() bool
	Begin(ctx context.Context)
	Caption(ctx context.Context, text string)
	// Finish shows the outcome and returns the kiosk to the board after a
	// countdown.
	Finish(ctx context.Context, n notice.Notice)
	// Restarting says the agent is about to be replaced. The new agent
	// finishes the screen (package agentupdate).
	Restarting(ctx context.Context)
}

// Events is told about installs the page should react to, and carries the
// banners shown over the running board.
type Events interface {
	ContentChanged(sequence int64)
	AppChanged(version string)
	Notice(n notice.Notice)
}

// Deps is what an Importer needs.
type Deps struct {
	Layout       store.Layout
	DeviceID     string
	AgentVersion string
	// Ring loads the trust ring. Called per batch, so `trust fetch` or
	// `trust add` take effect without a restart.
	Ring   func() (*trust.Ring, error)
	Screen Screen // may be nil
	Events Events // may be nil
	// AgentStaged is called after a batch staged a new agent, with the
	// batch's notice, which the new agent shows once it runs. May be nil.
	AgentStaged func(n notice.Notice)
	Logger      *slog.Logger
	Now         func() time.Time
}

// Importer serialises imports.
type Importer struct {
	d  Deps
	mu sync.Mutex

	// staged is closed when a staged agent turns out not to replace this
	// one (the root updater refused it), so the inbox takes work again.
	stagedMu sync.Mutex
	staged   chan struct{}
}

// AgentNotReplaced says the staged agent update ended without replacing this
// agent, so the inbox should carry on (package agentupdate).
func (im *Importer) AgentNotReplaced() {
	im.stagedMu.Lock()
	defer im.stagedMu.Unlock()
	if im.staged != nil {
		close(im.staged)
		im.staged = nil
	}
}

// awaitReplacement returns a channel closed by AgentNotReplaced, or already
// closed when no agent is staged.
func (im *Importer) awaitReplacement() <-chan struct{} {
	im.stagedMu.Lock()
	defer im.stagedMu.Unlock()
	if im.staged == nil {
		c := make(chan struct{})
		close(c)
		return c
	}
	return im.staged
}

// New returns an Importer.
func New(d Deps) *Importer {
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.Logger == nil {
		d.Logger = slog.New(slog.DiscardHandler)
	}
	return &Importer{d: d}
}

// ErrBusy is returned by TryImport while another import runs.
var ErrBusy = errors.New("another import is in progress")

// Import processes files as one batch, waiting for any running import.
func (im *Importer) Import(ctx context.Context, src Source, files []string) Batch {
	im.mu.Lock()
	defer im.mu.Unlock()
	return im.run(ctx, src, files)
}

// TryImport is Import, but fails at once with ErrBusy if an import is running.
func (im *Importer) TryImport(ctx context.Context, src Source, files []string) (Batch, error) {
	if !im.mu.TryLock() {
		return Batch{}, ErrBusy
	}
	defer im.mu.Unlock()
	return im.run(ctx, src, files), nil
}

type item struct {
	path string
	peek *mbu.Manifest // unverified; ordering and screen only
}

func rank(t mbu.Type) int {
	switch t {
	case mbu.TypeAgent:
		return 0
	case mbu.TypeApp:
		return 1
	}
	return 2
}

// progressLine says where a batch someone brought came from.
var progressLine = map[Source]string{
	SourceUSB:    "From the USB stick",
	SourceUpload: "From a laptop or phone",
	SourceCLI:    "From the command line",
}

// run processes one batch. The update screen goes up only once something is
// about to be installed; until then, and when nothing is, a person at the
// board sees a banner over the running board instead. Background sync is
// silent for content and uses the screen for software.
func (im *Importer) run(ctx context.Context, src Source, files []string) Batch {
	var b Batch
	items := make([]item, 0, len(files))
	for _, f := range files {
		m, _ := mbu.Peek(f)
		items = append(items, item{path: f, peek: m})
	}
	sort.SliceStable(items, func(i, j int) bool { return rank(peekType(items[i])) < rank(peekType(items[j])) })

	interactive := src != SourceSync
	banner := func(n notice.Notice) {
		if im.d.Events != nil && interactive {
			im.d.Events.Notice(n)
		}
	}
	// A screen held over from before an agent restart belongs to this batch.
	shown := im.d.Screen != nil && im.d.Screen.Active()
	banner(notice.New(notice.Progress, "Checking updates", progressLine[src]))
	show := func(kind mbu.Type, caption string) {
		if im.d.Screen == nil || (!interactive && kind == mbu.TypeContent) {
			return
		}
		if !shown {
			im.d.Screen.Begin(ctx)
			shown = true
		}
		im.d.Screen.Caption(ctx, caption)
	}

	ring, ringErr := im.d.Ring()
	for _, it := range items {
		name := filepath.Base(it.path)
		if b.AgentStaged {
			b.Results = append(b.Results, im.requeue(it.path, name))
			continue
		}
		if it.peek != nil && it.peek.Type == mbu.TypeContent && it.peek.DeviceID != "" && it.peek.DeviceID != im.d.DeviceID {
			b.Results = append(b.Results, skippedForeign(name))
			continue
		}
		var r Result
		if ringErr != nil {
			r = Result{File: name, Action: ActionRejected,
				Message: "The board cannot check updates: its list of trusted keys is unreadable",
				Detail:  ringErr.Error()}
		} else {
			r = im.one(ctx, src, it.path, name, ring, &b, show)
		}
		im.d.Logger.Info("import", "source", src, "file", name, "type", r.Type, "action", r.Action,
			"message", r.Message, "detail", r.Detail)
		b.Results = append(b.Results, r)
	}

	b.Notice = Summarize(b)
	if b.AgentStaged {
		im.stagedMu.Lock()
		im.staged = make(chan struct{})
		im.stagedMu.Unlock()
	}
	switch {
	case shown && b.AgentStaged:
		im.d.Screen.Restarting(ctx)
		if im.d.AgentStaged != nil {
			im.d.AgentStaged(b.Notice)
		}
	case shown:
		im.d.Screen.Finish(ctx, b.Notice)
	default:
		banner(b.Notice)
	}
	return b
}

// Summarize is what the board shows about a batch: what was installed first,
// then anything that went wrong; or, when nothing needed doing, why not.
func Summarize(b Batch) notice.Notice {
	var installed, problems, neutral []string
	foreign := 0
	for _, r := range b.Results {
		switch r.Action {
		case ActionInstalled, ActionStaged:
			installed = append(installed, r.Message)
		case ActionRejected:
			problems = append(problems, r.Message)
		case ActionSkipped:
			foreign++
		case ActionUnchanged, ActionOutdated:
			neutral = append(neutral, r.Message)
		}
	}
	switch {
	case len(installed) > 0 && len(problems) > 0:
		return notice.New(notice.Problem, "Some updates were not installed", append(problems, installed...)...)
	case len(installed) > 0:
		return notice.New(notice.OK, "Update complete", installed...)
	case len(problems) > 0:
		return notice.New(notice.Problem, "Update not installed", problems...)
	case len(neutral) > 0:
		return notice.New(notice.Neutral, "Already up to date", neutral...)
	case foreign > 0:
		return notice.New(notice.Neutral, "Nothing here is for this board",
			"These updates are for other MusallahBoards")
	}
	return notice.New(notice.Neutral, "No updates found")
}

func skippedForeign(name string) Result {
	return Result{File: name, Type: mbu.TypeContent, Action: ActionSkipped, Message: "Content for another board"}
}

func peekType(it item) mbu.Type {
	if it.peek == nil {
		return mbu.TypeContent
	}
	return it.peek.Type
}

func (im *Importer) one(ctx context.Context, src Source, path, name string, ring *trust.Ring, b *Batch, show func(mbu.Type, string)) Result {
	l := im.d.Layout
	pkg, err := mbu.Open(path, ring, mbu.OpenOptions{HaveMedia: l.HaveMedia})
	if err != nil {
		msg := "A file is damaged or is not a MusallahBoard update"
		if errors.Is(err, mbu.ErrNotAuthentic) {
			msg = "An update is not signed for this board"
		}
		return Result{File: name, Action: ActionRejected, Message: msg, Detail: err.Error()}
	}
	defer pkg.Close()
	m := pkg.Manifest
	b.Verified = true
	if t := m.CreatedTime(); t.After(b.MaxCreatedAt) {
		b.MaxCreatedAt = t
	}
	// A signed createdAt is a lower bound on the real time (section 10).
	if err := l.State().RaiseSignedClockFloor(m.CreatedTime().Unix()); err != nil {
		im.d.Logger.Warn("could not record clock floor", "err", err)
	}
	st, err := l.State().Load()
	if err != nil {
		return failed(Result{File: name, Type: m.Type}, "The board cannot read its own state", err)
	}
	res := Result{File: name, Type: m.Type, Version: m.Version, Sequence: m.Sequence}
	now := im.d.Now()

	switch m.Type {
	case mbu.TypeContent:
		if m.DeviceID != im.d.DeviceID {
			return skippedForeign(name)
		}
		switch {
		case m.Sequence < st.ContentSequence:
			res.Action, res.Message = ActionOutdated, "The board already has newer content"
			return res
		case m.Sequence == st.ContentSequence:
			res.Action, res.Message = ActionUnchanged, "This content is already on the board"
			return res
		}
		show(m.Type, "Installing "+describe(m))
		changed, err := l.InstallContent(pkg, string(src), now)
		if err != nil && !strings.Contains(err.Error(), "content installed, but") {
			return failed(res, "Installing the new content failed", err)
		}
		if err != nil {
			im.d.Logger.Warn("content cleanup", "err", err)
		}
		if _, err := l.State().Update(func(s *state.State) error {
			if m.Sequence > s.ContentSequence {
				s.ContentSequence = m.Sequence
			}
			return nil
		}); err != nil {
			im.d.Logger.Error("could not record content sequence", "err", err)
		}
		if changed && im.d.Events != nil {
			im.d.Events.ContentChanged(m.Sequence)
		}
		res.Action, res.Message = ActionInstalled, installedLine(m)

	case mbu.TypeApp:
		if m.App.LocalAPI > LocalAPIVersion {
			return failed(res, fmt.Sprintf("Board app %s needs a newer agent: install the agent update first", m.Version),
				fmt.Errorf("app needs local API %d; this agent serves %d", m.App.LocalAPI, LocalAPIVersion))
		}
		if action, msg := im.checkSoftware(m, st); action != "" {
			res.Action, res.Message = action, msg
			return res
		}
		show(m.Type, "Installing "+describe(m))
		if err := l.InstallApp(pkg, string(src), now); err != nil && !strings.Contains(err.Error(), "app installed, but") {
			return failed(res, "Installing board app "+m.Version+" failed", err)
		}
		if _, err := l.State().Update(func(s *state.State) error {
			if mbu.CompareVersions(m.Version, s.AppVersion) > 0 {
				s.AppVersion = m.Version
			}
			return nil
		}); err != nil {
			im.d.Logger.Error("could not record app version", "err", err)
		}
		if im.d.Events != nil {
			im.d.Events.AppChanged(m.Version)
		}
		res.Action, res.Message = ActionInstalled, installedLine(m)

	case mbu.TypeAgent:
		if action, msg := im.checkSoftware(m, st); action != "" {
			res.Action, res.Message = action, msg
			return res
		}
		show(m.Type, "Installing "+describe(m))
		if err := im.stageAgent(path); err != nil {
			return failed(res, "Preparing agent "+m.Version+" failed", err)
		}
		b.AgentStaged = true
		res.Action, res.Message = ActionStaged, installedLine(m)
	}
	return res
}

// describe names a package for the working screen's caption.
func describe(m *mbu.Manifest) string {
	switch m.Type {
	case mbu.TypeContent:
		return "new content"
	case mbu.TypeApp:
		return "board app " + m.Version
	case mbu.TypeAgent:
		return "agent " + m.Version
	}
	return "update"
}

// installedLine is the line an installed package gets on the outcome.
func installedLine(m *mbu.Manifest) string {
	switch m.Type {
	case mbu.TypeContent:
		if t, err := time.Parse("2006-01-02", m.Content.LastDay); err == nil {
			return "New content, through " + t.Format("Monday, January 2")
		}
		return "New content"
	case mbu.TypeApp:
		return "Board app " + m.Version
	case mbu.TypeAgent:
		return "Agent " + m.Version
	}
	return "Update"
}

// checkSoftware decides whether an app or agent package may replace what the
// board runs. It returns "" when it may, else the action (unchanged, outdated
// or rejected) and the line to show. The app's local API is checked at
// install time only: an app waiting for its agent is scheduled with it.
func (im *Importer) checkSoftware(m *mbu.Manifest, st state.State) (action, msg string) {
	switch m.Type {
	case mbu.TypeApp:
		switch c := mbu.CompareVersions(m.Version, st.AppVersion); {
		case c < 0:
			return ActionOutdated, "The board already has a newer board app (" + st.AppVersion + ")"
		case c == 0:
			return ActionUnchanged, "Board app " + m.Version + " is already installed"
		}
	case mbu.TypeAgent:
		if m.Agent.Arch != runtime.GOARCH {
			return ActionRejected, fmt.Sprintf("Agent %s is for a different kind of board (%s, not %s)", m.Version, m.Agent.Arch, runtime.GOARCH)
		}
		if st.AgentRejected(m.Version) {
			return ActionRejected, "Agent " + m.Version + " did not start on this board before, so it is not tried again"
		}
		switch c := mbu.CompareVersions(m.Version, im.d.AgentVersion); {
		case c < 0:
			return ActionOutdated, "The board already has a newer agent (" + im.d.AgentVersion + ")"
		case c == 0:
			return ActionUnchanged, "Agent " + m.Version + " is already running"
		}
	default:
		return ActionRejected, "This is not a software update"
	}
	return "", ""
}

// Preflight verifies a downloaded app or agent package and checks that it
// would install, without installing anything or recording anything about it.
// The update scheduler only holds packages that pass, so what a board
// announces is what it will install.
func (im *Importer) Preflight(path string) (*mbu.Manifest, error) {
	ring, err := im.d.Ring()
	if err != nil {
		return nil, fmt.Errorf("cannot read the trust store: %w", err)
	}
	pkg, err := mbu.Open(path, ring, mbu.OpenOptions{HaveMedia: im.d.Layout.HaveMedia})
	if err != nil {
		return nil, err
	}
	defer pkg.Close()
	st, err := im.d.Layout.State().Load()
	if err != nil {
		return nil, err
	}
	if action, msg := im.checkSoftware(pkg.Manifest, st); action != "" {
		return nil, errors.New(msg)
	}
	return pkg.Manifest, nil
}

// stageAgent hands a verified agent package to the root updater. The updater
// verifies it again from its own copy; staging only has to be atomic.
func (im *Importer) stageAgent(path string) error {
	l := im.d.Layout
	if err := os.MkdirAll(l.AgentStagedDir(), 0o750); err != nil {
		return err
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	tmp := l.StagedPackage() + ".part"
	os.Remove(tmp)
	if _, err := fsutil.CopyFileSync(tmp, f, mbu.MaxPackageBytes+1, 0o640); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, l.StagedPackage()); err != nil {
		return err
	}
	return fsutil.WriteAtomic(l.StagedReady(), []byte(im.d.Now().UTC().Format(time.RFC3339)+"\n"), 0o640)
}

// requeue moves a file that follows a staged agent into the inbox, for the
// new agent to process.
func (im *Importer) requeue(path, name string) Result {
	inbox := im.d.Layout.Inbox()
	if filepath.Dir(path) != inbox {
		dst := filepath.Join(inbox, fsutil.UniqueName(inbox, name))
		if err := os.MkdirAll(inbox, 0o770); err == nil {
			if err := os.Rename(path, dst); err != nil {
				return Result{File: name, Action: ActionRejected, Message: "Could not keep an update for after the agent restarts", Detail: err.Error()}
			}
		}
	}
	return Result{File: name, Action: ActionQueued, Message: "Installs once the new agent is running"}
}

// failed rejects r with a plain message, keeping err for the logs.
func failed(r Result, msg string, err error) Result {
	r.Action, r.Message = ActionRejected, msg
	if err != nil {
		r.Detail = err.Error()
	}
	return r
}

// Busy reports whether an import is running right now.
func (im *Importer) Busy() bool {
	if im.mu.TryLock() {
		im.mu.Unlock()
		return false
	}
	return true
}
