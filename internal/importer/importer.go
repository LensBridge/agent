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
	ActionUnchanged = "unchanged"
	ActionStaged    = "staged"
	ActionSkipped   = "skipped"
	ActionRejected  = "rejected"
	ActionQueued    = "queued"
)

// Result is the outcome for one package.
type Result struct {
	File     string   `json:"file"`
	Type     mbu.Type `json:"type,omitempty"`
	Version  string   `json:"version,omitempty"`
	Sequence int64    `json:"sequence,omitempty"`
	Action   string   `json:"action"`
	Message  string   `json:"message"`
}

// Batch is the outcome of one import.
type Batch struct {
	Results []Result `json:"results"`
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
	Begin(ctx context.Context)
	Caption(ctx context.Context, text string)
	// Finish shows the outcome and returns the kiosk to the board after a
	// countdown. restarting means the agent is about to be replaced.
	Finish(ctx context.Context, ok bool, detail string, restarting bool)
}

// Events is told about installs the page should react to.
type Events interface {
	ContentChanged(sequence int64)
	AppChanged(version string)
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
	Logger *slog.Logger
	Now    func() time.Time
}

// Importer serialises imports.
type Importer struct {
	d  Deps
	mu sync.Mutex
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

func (im *Importer) run(ctx context.Context, src Source, files []string) Batch {
	var b Batch
	items := make([]item, 0, len(files))
	for _, f := range files {
		m, _ := mbu.Peek(f)
		items = append(items, item{path: f, peek: m})
	}
	sort.SliceStable(items, func(i, j int) bool { return rank(peekType(items[i])) < rank(peekType(items[j])) })

	// The screen goes up for anything a person brought to the board, unless
	// everything in it is someone else's content. Background sync only puts
	// it up for software.
	screen := false
	for _, it := range items {
		if it.peek != nil && it.peek.Type == mbu.TypeContent && it.peek.DeviceID != "" && it.peek.DeviceID != im.d.DeviceID {
			continue
		}
		if src != SourceSync || peekType(it) != mbu.TypeContent {
			screen = true
		}
	}
	shown := false
	show := func(caption string) {
		if im.d.Screen == nil || !screen {
			return
		}
		if !shown {
			im.d.Screen.Begin(ctx)
			shown = true
		}
		im.d.Screen.Caption(ctx, caption)
	}

	ring, ringErr := im.d.Ring()
	for i, it := range items {
		name := filepath.Base(it.path)
		if b.AgentStaged {
			b.Results = append(b.Results, im.requeue(it.path, name))
			continue
		}
		if it.peek != nil && it.peek.Type == mbu.TypeContent && it.peek.DeviceID != "" && it.peek.DeviceID != im.d.DeviceID {
			b.Results = append(b.Results, Result{File: name, Type: mbu.TypeContent, Action: ActionSkipped,
				Message: "content for another board (" + shortID(it.peek.DeviceID) + "); left alone"})
			continue
		}
		label := "update"
		if it.peek != nil {
			label = it.peek.Describe()
		}
		show(fmt.Sprintf("Checking %s (%d of %d)", label, i+1, len(items)))
		var r Result
		if ringErr != nil {
			r = reject(name, fmt.Errorf("cannot read the trust store: %w", ringErr))
		} else {
			r = im.one(ctx, src, it.path, name, ring, &b, show)
		}
		im.d.Logger.Info("import", "source", src, "file", name, "type", r.Type, "action", r.Action, "message", r.Message)
		b.Results = append(b.Results, r)
	}

	if shown {
		ok := b.OK()
		detail := summary(b)
		im.d.Screen.Finish(ctx, ok, detail, b.AgentStaged)
	}
	return b
}

func peekType(it item) mbu.Type {
	if it.peek == nil {
		return mbu.TypeContent
	}
	return it.peek.Type
}

func (im *Importer) one(ctx context.Context, src Source, path, name string, ring *trust.Ring, b *Batch, show func(string)) Result {
	l := im.d.Layout
	pkg, err := mbu.Open(path, ring, mbu.OpenOptions{HaveMedia: l.HaveMedia})
	if err != nil {
		return reject(name, err)
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
		return reject(name, err)
	}
	res := Result{File: name, Type: m.Type, Version: m.Version, Sequence: m.Sequence}
	now := im.d.Now()

	switch m.Type {
	case mbu.TypeContent:
		if m.DeviceID != im.d.DeviceID {
			res.Action, res.Message = ActionSkipped, "content for another board ("+shortID(m.DeviceID)+"); left alone"
			return res
		}
		switch {
		case m.Sequence < st.ContentSequence:
			return rejectAs(res, "this content is older than what is installed")
		case m.Sequence == st.ContentSequence:
			res.Action, res.Message = ActionUnchanged, "this content is already installed"
			return res
		}
		show("Installing " + m.Describe())
		changed, err := l.InstallContent(pkg, string(src), now)
		if err != nil && !strings.Contains(err.Error(), "content installed, but") {
			return rejectAs(res, err.Error())
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
		res.Action, res.Message = ActionInstalled, "Installed "+m.Describe()

	case mbu.TypeApp:
		if m.App.LocalAPI > LocalAPIVersion {
			return rejectAs(res, fmt.Sprintf("board app %s needs a newer agent (local API %d; this agent serves %d): install the agent update first",
				m.Version, m.App.LocalAPI, LocalAPIVersion))
		}
		switch c := mbu.CompareVersions(m.Version, st.AppVersion); {
		case c < 0:
			return rejectAs(res, "board app "+m.Version+" is older than the installed "+st.AppVersion)
		case c == 0:
			res.Action, res.Message = ActionUnchanged, "board app "+m.Version+" is already installed"
			return res
		}
		show("Installing " + m.Describe())
		if err := l.InstallApp(pkg, string(src), now); err != nil && !strings.Contains(err.Error(), "app installed, but") {
			return rejectAs(res, err.Error())
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
		res.Action, res.Message = ActionInstalled, "Installed "+m.Describe()

	case mbu.TypeAgent:
		if m.Agent.Arch != runtime.GOARCH {
			return rejectAs(res, fmt.Sprintf("agent %s is built for %s; this board is %s", m.Version, m.Agent.Arch, runtime.GOARCH))
		}
		if st.AgentRejected(m.Version) {
			return rejectAs(res, "agent "+m.Version+" failed to start on this board before; it will not be retried")
		}
		switch c := mbu.CompareVersions(m.Version, im.d.AgentVersion); {
		case c < 0:
			return rejectAs(res, "agent "+m.Version+" is older than the running "+im.d.AgentVersion)
		case c == 0:
			res.Action, res.Message = ActionUnchanged, "agent "+m.Version+" is already running"
			return res
		}
		show("Preparing " + m.Describe())
		if err := im.stageAgent(path); err != nil {
			return rejectAs(res, err.Error())
		}
		b.AgentStaged = true
		res.Action, res.Message = ActionStaged, "Installing "+m.Describe()+"; the agent restarts in a moment"
	}
	return res
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
				return Result{File: name, Action: ActionRejected, Message: "could not queue it behind the agent update: " + err.Error()}
			}
		}
	}
	return Result{File: name, Action: ActionQueued, Message: "installs after the agent update restarts the agent"}
}

func reject(name string, err error) Result {
	return Result{File: name, Action: ActionRejected, Message: err.Error()}
}

func rejectAs(r Result, msg string) Result {
	r.Action, r.Message = ActionRejected, msg
	return r
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// summary is the one line the update screen shows under its outcome.
func summary(b Batch) string {
	var installed, rejected []string
	for _, r := range b.Results {
		switch r.Action {
		case ActionInstalled, ActionStaged:
			installed = append(installed, r.Message)
		case ActionRejected:
			rejected = append(rejected, r.File+": "+r.Message)
		}
	}
	if len(rejected) > 0 {
		return rejected[0]
	}
	if len(installed) > 0 {
		return strings.Join(installed, " · ")
	}
	return "Nothing new to install"
}

// Busy reports whether an import is running right now.
func (im *Importer) Busy() bool {
	if im.mu.TryLock() {
		im.mu.Unlock()
		return false
	}
	return true
}
