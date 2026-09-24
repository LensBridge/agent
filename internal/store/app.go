package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/LensBridge/agent/internal/fsutil"
	"github.com/LensBridge/agent/internal/mbu"
)

// App is an installed board app release.
type App struct {
	Dir      string
	Manifest *mbu.Manifest
}

// CurrentApp loads the release current points at.
func (l Layout) CurrentApp() (*App, error) {
	dir, err := fsutil.ReadLinkAbs(l.AppCurrent())
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNoApp
	}
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(filepath.Join(dir, ".mbu", mbu.ManifestName))
	if err != nil {
		return nil, fmt.Errorf("installed app %s is unreadable: %w", filepath.Base(dir), err)
	}
	m, err := mbu.ParseManifest(raw)
	if err != nil {
		return nil, fmt.Errorf("installed app %s is unreadable: %w", filepath.Base(dir), err)
	}
	return &App{Dir: dir, Manifest: m}, nil
}

// InstallApp installs an authenticated app package and makes it current.
// The release's own manifest is kept in <release>/.mbu/, a name no package
// path can have (segments may not start with a dot), so it cannot collide
// with a build file or be served.
func (l Layout) InstallApp(pkg *mbu.Package, source string, now time.Time) error {
	if err := os.MkdirAll(l.AppReleases(), 0o750); err != nil {
		return err
	}
	unlock, err := fsutil.LockDir(l.appDir())
	if err != nil {
		return err
	}
	defer unlock()

	stage, err := os.MkdirTemp(l.AppReleases(), ".staging-")
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			os.RemoveAll(stage)
		}
	}()
	if err := os.Chmod(stage, 0o750); err != nil {
		return err
	}

	dirs := map[string]bool{stage: true}
	for _, f := range pkg.Manifest.Files {
		// f.Path passed mbu.ValidPath, so the join stays inside stage.
		dst := filepath.Join(stage, filepath.FromSlash(f.Path))
		if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
			return err
		}
		dirs[filepath.Dir(dst)] = true
		out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o640)
		if err != nil {
			return err
		}
		if err := pkg.CopyFile(f.Path, out); err != nil {
			out.Close()
			return err
		}
		if err := out.Sync(); err != nil {
			out.Close()
			return err
		}
		if err := out.Close(); err != nil {
			return err
		}
	}
	meta := filepath.Join(stage, ".mbu")
	if err := os.Mkdir(meta, 0o750); err != nil {
		return err
	}
	dirs[meta] = true
	info, _ := json.Marshal(InstallInfo{Source: source, InstalledAt: now.UTC().Format(time.RFC3339)})
	for name, data := range map[string][]byte{
		mbu.ManifestName: pkg.RawManifest, mbu.SignatureName: pkg.RawSig, installInfoName: info,
	} {
		if err := fsutil.WriteFileSync(filepath.Join(meta, name), data, 0o640); err != nil {
			return err
		}
	}
	for d := range dirs {
		if err := fsutil.SyncDir(d); err != nil {
			return err
		}
	}

	prev, _ := l.CurrentApp()
	name := fsutil.UniqueName(l.AppReleases(), pkg.Manifest.Version)
	if err := os.Rename(stage, filepath.Join(l.AppReleases(), name)); err != nil {
		return err
	}
	committed = true
	if err := fsutil.SyncDir(l.AppReleases()); err != nil {
		return err
	}
	if err := fsutil.SwapSymlink(l.AppCurrent(), filepath.Join("releases", name)); err != nil {
		return err
	}
	prevName := ""
	if prev != nil {
		prevName = filepath.Base(prev.Dir)
	}
	if _, err := fsutil.Prune(l.AppReleases(), name, prevName); err != nil {
		return fmt.Errorf("app installed, but removing old releases failed: %w", err)
	}
	return nil
}
