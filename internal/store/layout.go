// Package store is the board's installed state on disk (docs/architecture.md,
// section 6): the content bundles and their media store, the app releases,
// and the paths the rest of the agent shares (inbox, staged agent, state).
//
// Installs here take packages that mbu.Open has already authenticated; the
// importer decides whether they may be installed at all. Every install stages,
// fsyncs, renames and swaps a symlink, so a power cut leaves the previous
// state serving.
package store

import (
	"errors"
	"path/filepath"

	"github.com/LensBridge/agent/internal/state"
)

// DefaultRoot is the daemon's StateDirectory.
const DefaultRoot = "/var/lib/musallahboard"

var (
	// ErrNoContent means no content bundle is installed.
	ErrNoContent = errors.New("no content installed")
	// ErrNoApp means no app release is installed.
	ErrNoApp = errors.New("no board app installed")
)

// Layout locates everything under one root, so tests can use a temp dir.
type Layout struct{ Root string }

// Default is the production layout.
func Default() Layout { return Layout{Root: DefaultRoot} }

func (l Layout) State() state.File       { return state.File{Path: filepath.Join(l.Root, "state.json")} }
func (l Layout) contentDir() string      { return filepath.Join(l.Root, "content") }
func (l Layout) MediaDir() string        { return filepath.Join(l.contentDir(), "media") }
func (l Layout) BundlesDir() string      { return filepath.Join(l.contentDir(), "bundles") }
func (l Layout) ContentCurrent() string  { return filepath.Join(l.contentDir(), "current") }
func (l Layout) appDir() string          { return filepath.Join(l.Root, "app") }
func (l Layout) AppReleases() string     { return filepath.Join(l.appDir(), "releases") }
func (l Layout) AppCurrent() string      { return filepath.Join(l.appDir(), "current") }
func (l Layout) AgentStagedDir() string  { return filepath.Join(l.Root, "agent", "staged") }
func (l Layout) AgentLastUpdate() string { return filepath.Join(l.Root, "agent", "last-update.json") }

// AgentPendingNotice is the outcome of the batch that staged an agent, left
// for the agent that starts next to show (package agentupdate).
func (l Layout) AgentPendingNotice() string {
	return filepath.Join(l.Root, "agent", "pending-notice.json")
}
func (l Layout) Inbox() string        { return filepath.Join(l.Root, "inbox") }
func (l Layout) InboxResults() string { return filepath.Join(l.Inbox(), "results") }

// Updates holds software downloaded from the release channels until its
// install window (section 9.4), and the CLI's install-now request.
func (l Layout) Updates() string       { return filepath.Join(l.Root, "updates") }
func (l Layout) UpdateRequest() string { return filepath.Join(l.Updates(), "install-now") }
func (l Layout) UpdateRequestResult() string {
	return filepath.Join(l.Updates(), "install-now.result.json")
}

// StagedPackage and StagedReady are the self-update hand-off (section 12).
func (l Layout) StagedPackage() string { return filepath.Join(l.AgentStagedDir(), "package.mbu") }
func (l Layout) StagedReady() string   { return filepath.Join(l.AgentStagedDir(), "ready") }
