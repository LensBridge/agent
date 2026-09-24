// Package offline is the device side of MusallahBoard's offline mode: it
// validates and installs content bundles carried to the board on a laptop,
// works out which day's payload to show, and serves the board SPA, payloads
// and media to the kiosk from 127.0.0.1 when the board has no internet.
//
// docs/offline.md is the contract. "Contract 1" defines the bundle zip checked
// here; "Contract 2" defines the filesystem layout, the HTTP endpoints and the
// install steps. The backend exporter and the frontend are built against the
// same document, so behaviour here should change only alongside it.
//
// Nothing in this package makes a network call other than accepting local
// HTTP connections.
package offline

import (
	"errors"
	"path/filepath"
)

// Defaults for a real board. Every function takes a Paths so tests can point
// them at a temp dir.
const (
	DefaultRoot   = "/var/lib/musallahboard/offline"
	DefaultSPADir = "/usr/share/musallahboard/board"

	// ListenAddr is loopback only: the server is for the kiosk on the same
	// box, and nothing on the service-port cable has any business reaching it.
	// kioskurl.OfflineBaseURL must agree.
	ListenAddr = "127.0.0.1:8080"
)

// ErrNoBundle means no bundle has been installed yet (current does not exist).
var ErrNoBundle = errors.New("no bundle installed")

// Paths locates the on-disk state. The zero value is not usable; use
// DefaultPaths or fill both fields.
type Paths struct {
	// Root holds bundles/ and the current symlink.
	Root string
	// SPA is the built frontend (index.html, assets/...). May be a symlink,
	// which is how InstallApp swaps it atomically.
	SPA string
}

// DefaultPaths returns the production layout from docs/offline.md.
func DefaultPaths() Paths {
	return Paths{Root: DefaultRoot, SPA: DefaultSPADir}
}

// Bundles is the directory holding each extracted bundle.
func (p Paths) Bundles() string { return filepath.Join(p.Root, "bundles") }

// Current is the symlink to the bundle being served.
func (p Paths) Current() string { return filepath.Join(p.Root, "current") }

// appReleases holds SPA builds; p.SPA is a symlink into it once InstallApp
// has run.
func (p Paths) appReleases() string { return p.SPA + ".d" }
