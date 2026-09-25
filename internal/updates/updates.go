// Package updates holds board software (the app and the agent) that the
// release channels found, and installs it in the board's quiet window rather
// than the moment it arrives (docs/architecture.md, section 9.4). While
// something waits, /api/local/status says what and when, and the board app
// puts it on the ticker.
//
// Only background channel updates wait. A package someone brings to the board
// (USB stick, upload, CLI) installs at once, because that person is standing
// there, and a board with no app installs its first one at once, because it
// has nothing else to show.
package updates

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/LensBridge/agent/internal/fsutil"
	"github.com/LensBridge/agent/internal/importer"
	"github.com/LensBridge/agent/internal/mbu"
	"github.com/LensBridge/agent/internal/store"
)

// Window is how long after the install time a waiting update still installs.
// A board that was off at 23:00 but on by 01:00 still updates that night; one
// switched on in the morning waits for the next night.
const Window = 4 * time.Hour

const (
	checkInterval = 30 * time.Second
	requestPoll   = 2 * time.Second
)

// order is the order updates are listed and installed in. The importer sorts a
// batch itself; this only keeps the status stable.
var order = []mbu.Type{mbu.TypeAgent, mbu.TypeApp}

// Update is one waiting update.
type Update struct {
	Type        mbu.Type `json:"type"`
	Version     string   `json:"version"`
	Description string   `json:"description"`
}

// Info is the "updates" object of /api/local/status.
type Info struct {
	Available []Update `json:"available"`
	// InstallTime is the configured install time, "HH:MM" in the board's
	// time zone.
	InstallTime string `json:"installTime"`
	// InstallAt is when the waiting updates install (RFC 3339, in the
	// board's time zone), or null when nothing waits. It is the current time
	// when the window is already open.
	InstallAt  *string `json:"installAt"`
	Installing bool    `json:"installing"`
}

// Outcome is the result of an install-now request (CLI or remote command).
type Outcome struct {
	At string `json:"at"`
	// CheckError is why the release channels could not be asked, if they
	// could not. Updates found earlier still install.
	CheckError string            `json:"checkError,omitempty"`
	Results    []importer.Result `json:"results"`
}

// Deps is what a Scheduler needs.
type Deps struct {
	Layout   store.Layout
	Importer *importer.Importer
	// Check asks the release channels for new software, which they hand to
	// Offer. Nil when the board cannot reach them (no device key).
	Check func(ctx context.Context) error
	// Changed is told whenever Info changes. May be nil.
	Changed      func(Info)
	Hour, Minute int
	Logger       *slog.Logger
	Now          func() time.Time
}

// Scheduler holds waiting updates and installs them.
type Scheduler struct {
	d    Deps
	log  *slog.Logger
	kick chan struct{}

	// installMu is held while packages move in or out of the updates
	// directory, so an Offer never replaces a file the importer is reading.
	installMu sync.Mutex

	mu         sync.Mutex
	pending    map[mbu.Type]*mbu.Manifest
	installing bool
}

// New returns a Scheduler holding whatever an earlier run left waiting.
func New(d Deps) *Scheduler {
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.Logger == nil {
		d.Logger = slog.New(slog.DiscardHandler)
	}
	s := &Scheduler{
		d:       d,
		log:     d.Logger.With("component", "updates"),
		kick:    make(chan struct{}, 1),
		pending: map[mbu.Type]*mbu.Manifest{},
	}
	s.load()
	return s
}

func (s *Scheduler) path(t mbu.Type) string {
	return filepath.Join(s.d.Layout.Updates(), string(t)+".mbu")
}

// load picks up updates downloaded before a restart. One that no longer
// passes (installed from a USB stick meanwhile, say) is dropped.
func (s *Scheduler) load() {
	for _, t := range order {
		p := s.path(t)
		if _, err := os.Stat(p); err != nil {
			continue
		}
		m, err := s.d.Importer.Preflight(p)
		if err == nil && m.Type != t {
			err = fmt.Errorf("it is a %s package", m.Type)
		}
		if err != nil {
			s.log.Info("dropping a waiting update", "file", p, "reason", err)
			_ = os.Remove(p)
			continue
		}
		s.pending[t] = m
		s.log.Info("update waiting", "update", m.Describe())
	}
}

// Pending returns the version of kind waiting to install, or "".
func (s *Scheduler) Pending(kind mbu.Type) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if m := s.pending[kind]; m != nil {
		return m.Version
	}
	return ""
}

// Offer takes a package the release channels downloaded. It is verified and
// checked now, so the board never announces something it would refuse, and
// moved into the updates directory (file must be on the same filesystem).
func (s *Scheduler) Offer(file string) error {
	m, err := s.d.Importer.Preflight(file)
	if err != nil {
		return err
	}
	if m.Type == mbu.TypeApp && m.App.LocalAPI > importer.LocalAPIVersion && s.Pending(mbu.TypeAgent) == "" {
		return fmt.Errorf("board app %s needs a newer agent (local API %d; this agent serves %d); it is offered again once the agent is updated",
			m.Version, m.App.LocalAPI, importer.LocalAPIVersion)
	}

	s.installMu.Lock()
	err = os.MkdirAll(s.d.Layout.Updates(), 0o750)
	if err == nil {
		err = os.Rename(file, s.path(m.Type))
	}
	if err == nil {
		s.mu.Lock()
		s.pending[m.Type] = m
		s.mu.Unlock()
	}
	s.installMu.Unlock()
	if err != nil {
		return fmt.Errorf("could not keep %s for later: %w", m.Describe(), err)
	}

	info := s.Info()
	at := ""
	if info.InstallAt != nil {
		at = *info.InstallAt
	}
	s.log.Info("update available", "update", m.Describe(), "installAt", at)
	s.changed(info)
	select {
	case s.kick <- struct{}{}:
	default:
	}
	return nil
}

// Info is the current status.
func (s *Scheduler) Info() Info {
	now := s.d.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	info := Info{
		Available:   []Update{},
		InstallTime: fmt.Sprintf("%02d:%02d", s.d.Hour, s.d.Minute),
		Installing:  s.installing,
	}
	for _, t := range order {
		if m := s.pending[t]; m != nil {
			info.Available = append(info.Available, Update{Type: t, Version: m.Version, Description: m.Describe()})
		}
	}
	if len(info.Available) > 0 {
		at := s.installAt(now).Format(time.RFC3339)
		info.InstallAt = &at
	}
	return info
}

// Run installs waiting updates when their window opens, and serves the CLI's
// install-now requests, until ctx ends.
func (s *Scheduler) Run(ctx context.Context) {
	go s.watchRequests(ctx)
	t := time.NewTicker(checkInterval)
	defer t.Stop()
	for {
		if s.due() {
			s.InstallNow(ctx)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-s.kick:
		}
	}
}

// CheckAndInstall asks the release channels for new software and installs
// everything waiting, whatever the time. It is the "update now" command.
// ctx bounds the check (a remote command's deadline); an install that has
// started always finishes, because stopping one halfway helps nobody.
func (s *Scheduler) CheckAndInstall(ctx context.Context) Outcome {
	var out Outcome
	if s.d.Check == nil {
		out.CheckError = "this board cannot reach the release channels (no device key)"
	} else if err := s.d.Check(ctx); err != nil {
		out.CheckError = err.Error()
	}
	b, _ := s.InstallNow(context.WithoutCancel(ctx))
	out.Results = b.Results
	if out.Results == nil {
		out.Results = []importer.Result{}
	}
	out.At = s.d.Now().UTC().Format(time.RFC3339)
	return out
}

// InstallNow installs everything waiting, as one batch, whatever the time. It
// reports false when nothing was waiting.
func (s *Scheduler) InstallNow(ctx context.Context) (importer.Batch, bool) {
	s.installMu.Lock()
	defer s.installMu.Unlock()

	s.mu.Lock()
	var files []string
	for _, t := range order {
		if s.pending[t] != nil {
			files = append(files, s.path(t))
		}
	}
	if len(files) == 0 {
		s.mu.Unlock()
		return importer.Batch{}, false
	}
	s.installing = true
	s.mu.Unlock()
	s.changed(s.Info())

	// Source sync: the update screen goes up for software, as it should for
	// anything that changes what the board runs.
	b := s.d.Importer.Import(ctx, importer.SourceSync, files)
	for _, r := range b.Results {
		s.log.Info("update installed", "file", r.File, "action", r.Action, "message", r.Message)
	}
	// Installed, refused or queued behind an agent restart (the importer
	// moves those into the inbox), each is done waiting here. One refused
	// for a passing reason is offered again by the next channel check.
	for _, f := range files {
		if err := os.Remove(f); err != nil && !errors.Is(err, fs.ErrNotExist) {
			s.log.Warn("could not remove an installed update", "file", f, "err", err)
		}
	}

	s.mu.Lock()
	s.pending = map[mbu.Type]*mbu.Manifest{}
	s.installing = false
	s.mu.Unlock()
	s.changed(s.Info())
	return b, true
}

// due reports whether waiting updates should install now: the window is
// open, or the board has no app and so nothing better to do.
func (s *Scheduler) due() bool {
	s.mu.Lock()
	n := len(s.pending)
	s.mu.Unlock()
	if n == 0 {
		return false
	}
	if st, err := s.d.Layout.State().Load(); err == nil && st.AppVersion == "" {
		return true
	}
	open, _ := s.window(s.d.Now())
	return open
}

// installAt is when waiting updates install: now if the window is open, else
// the next time it opens.
func (s *Scheduler) installAt(now time.Time) time.Time {
	if st, err := s.d.Layout.State().Load(); err == nil && st.AppVersion == "" {
		return now.In(s.location())
	}
	open, next := s.window(now)
	if open {
		return now.In(s.location())
	}
	return next
}

// window reports whether an install window is open at now and, if not, when
// the next one opens. A window that opened yesterday evening can still be
// open after midnight.
func (s *Scheduler) window(now time.Time) (open bool, next time.Time) {
	loc := s.location()
	n := now.In(loc)
	for _, day := range []int{-1, 0} {
		start := time.Date(n.Year(), n.Month(), n.Day()+day, s.d.Hour, s.d.Minute, 0, 0, loc)
		if !n.Before(start) && n.Before(start.Add(Window)) {
			return true, start
		}
	}
	next = time.Date(n.Year(), n.Month(), n.Day(), s.d.Hour, s.d.Minute, 0, 0, loc)
	if !n.Before(next) {
		next = time.Date(n.Year(), n.Month(), n.Day()+1, s.d.Hour, s.d.Minute, 0, 0, loc)
	}
	return false, next
}

// location is the board's time zone: the installed content's, which is the
// one its clock face shows, or the system's before any content arrives.
func (s *Scheduler) location() *time.Location {
	if c, err := s.d.Layout.CurrentContent(); err == nil && c.Manifest.Content != nil {
		if loc, err := time.LoadLocation(c.Manifest.Content.Timezone); err == nil {
			return loc
		}
	}
	return time.Local
}

func (s *Scheduler) changed(info Info) {
	if s.d.Changed != nil {
		s.d.Changed(info)
	}
}

// watchRequests serves `musallahboard-agent update now`, which, being root
// and a separate process, asks by dropping a file and waits for the answer.
func (s *Scheduler) watchRequests(ctx context.Context) {
	req, res := s.d.Layout.UpdateRequest(), s.d.Layout.UpdateRequestResult()
	t := time.NewTicker(requestPoll)
	defer t.Stop()
	for {
		if _, err := os.Stat(req); err == nil {
			_ = os.Remove(req)
			s.log.Info("update now requested from the command line")
			out := s.CheckAndInstall(ctx)
			raw, _ := json.MarshalIndent(out, "", "  ")
			if err := fsutil.WriteAtomic(res, append(raw, '\n'), 0o640); err != nil {
				s.log.Warn("could not write the update-now result", "err", err)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
