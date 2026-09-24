package offline

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// InstallResult describes a successful bundle install.
type InstallResult struct {
	Manifest *Manifest
	// Name is the new bundle's directory under bundles/.
	Name string
	// Previous is the bundle that was current before, or "" if none was.
	Previous string
	// Removed lists what pruning deleted from bundles/.
	Removed []string
}

// InstallBundle validates the zip at zipPath for deviceID and, only if every
// check passes, makes it the bundle being served. The sequence is the one in
// Contract 2: extract into bundles/.staging-<random>, fsync, rename to
// bundles/<generatedAt>, swap the current symlink atomically, then prune to
// current + previous.
//
// Until the symlink swap, nothing a reader of current can see has changed, so
// a failure (or a power cut) at any earlier step leaves the previous bundle
// serving. A failure after the swap — only pruning — is reported but the new
// bundle is already live.
func InstallBundle(p Paths, zipPath, deviceID string) (*InstallResult, error) {
	bundles := p.Bundles()
	if err := os.MkdirAll(bundles, 0o755); err != nil {
		return nil, fmt.Errorf("create %s: %w", bundles, err)
	}
	unlock, err := lockDir(p.Root)
	if err != nil {
		return nil, err
	}
	defer unlock()

	stage, err := os.MkdirTemp(bundles, ".staging-")
	if err != nil {
		return nil, fmt.Errorf("create staging directory: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = os.RemoveAll(stage)
		}
	}()
	// MkdirTemp makes it 0700. Installs run as root and the daemon that
	// serves the bundle does not, so it has to be traversable.
	if err := os.Chmod(stage, 0o755); err != nil {
		return nil, err
	}

	m, err := extractBundle(zipPath, stage, deviceID)
	if err != nil {
		return nil, err
	}

	previous, err := currentName(p)
	if err != nil {
		return nil, err
	}

	name := uniqueName(bundles, m.compactStamp())
	final := filepath.Join(bundles, name)
	if err := os.Rename(stage, final); err != nil {
		return nil, fmt.Errorf("move bundle into place: %w", err)
	}
	committed = true
	if err := syncDir(bundles); err != nil {
		return nil, err
	}

	// Relative, so the offline root could be moved or bind-mounted without
	// breaking the link.
	if err := swapSymlink(p.Current(), filepath.Join("bundles", name)); err != nil {
		return nil, err
	}

	res := &InstallResult{Manifest: m, Name: name, Previous: previous}
	res.Removed, err = prune(bundles, name, previous)
	if err != nil {
		return res, fmt.Errorf("bundle installed, but cleaning up old bundles failed: %w", err)
	}
	return res, nil
}

// currentName returns the directory name under bundles/ that current points
// at, or "" if there is no current bundle.
func currentName(p Paths) (string, error) {
	target, err := readLinkAbs(p.Current())
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read %s: %w", p.Current(), err)
	}
	return filepath.Base(target), nil
}

// CurrentBundle resolves the current symlink and loads that bundle's
// manifest. It returns ErrNoBundle if nothing is installed.
func CurrentBundle(p Paths) (dir string, m *Manifest, err error) {
	dir, err = readLinkAbs(p.Current())
	if errors.Is(err, os.ErrNotExist) {
		return "", nil, ErrNoBundle
	}
	if err != nil {
		return "", nil, err
	}
	m, err = readManifest(dir)
	if err != nil {
		return "", nil, fmt.Errorf("installed bundle %s is unreadable: %w", filepath.Base(dir), err)
	}
	return dir, m, nil
}
