package offline

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

// Limits on an SPA build. A Vite build of the board is a few MB; these only
// stop a wrong directory or archive from filling the disk.
const (
	maxAppBytes = 500 << 20
	maxAppFiles = 20000
)

// AppResult describes a successful app install.
type AppResult struct {
	// Name is the new release's directory under <SPA>.d/.
	Name     string
	Previous string
	Removed  []string
}

// InstallApp installs a built frontend from src — a directory, or a .tar.gz
// / .tgz of one — so that p.SPA serves it. The build must have index.html at
// its root, or inside a single top-level directory (a tarball of dist/).
//
// Same approach as InstallBundle: copy into <SPA>.d/.staging-<random>, fsync,
// rename to <SPA>.d/<timestamp>, then atomically repoint the p.SPA symlink and
// prune to current + previous. The first install on a board where p.SPA is a
// plain directory moves that directory into <SPA>.d/ first; that one step is
// not atomic, which is acceptable because it happens once, at provisioning.
func InstallApp(p Paths, src string) (*AppResult, error) {
	releases := p.appReleases()
	if err := os.MkdirAll(releases, 0o755); err != nil {
		return nil, fmt.Errorf("create %s: %w", releases, err)
	}
	unlock, err := lockDir(releases)
	if err != nil {
		return nil, err
	}
	defer unlock()

	stage, err := os.MkdirTemp(releases, ".staging-")
	if err != nil {
		return nil, fmt.Errorf("create staging directory: %w", err)
	}
	defer os.RemoveAll(stage) // a no-op once the release has been renamed out
	if err := os.Chmod(stage, 0o755); err != nil {
		return nil, err
	}

	fi, err := os.Stat(src)
	if err != nil {
		return nil, err
	}
	switch {
	case fi.IsDir():
		err = copyTree(src, stage)
	case strings.HasSuffix(src, ".tar.gz") || strings.HasSuffix(src, ".tgz"):
		err = extractTarGz(src, stage)
	default:
		err = fmt.Errorf("%s is neither a directory nor a .tar.gz", src)
	}
	if err != nil {
		return nil, err
	}

	root, err := appRoot(stage)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(root, 0o755); err != nil {
		return nil, err
	}

	previous, err := appCurrentName(p)
	if err != nil {
		return nil, err
	}
	name := uniqueName(releases, time.Now().UTC().Format("20060102T150405Z"))
	if err := os.Rename(root, filepath.Join(releases, name)); err != nil {
		return nil, fmt.Errorf("move app into place: %w", err)
	}
	if err := syncDir(releases); err != nil {
		return nil, err
	}

	if err := adoptPlainSPADir(p, &previous); err != nil {
		return nil, err
	}
	if err := swapSymlink(p.SPA, filepath.Join(filepath.Base(releases), name)); err != nil {
		return nil, err
	}

	res := &AppResult{Name: name, Previous: previous}
	res.Removed, err = prune(releases, name, previous, ".lock")
	if err != nil {
		return res, fmt.Errorf("app installed, but cleaning up old builds failed: %w", err)
	}
	return res, nil
}

// appCurrentName is the release p.SPA points at, or "" if it is not a symlink
// (not installed yet, or a plain directory from before InstallApp existed).
func appCurrentName(p Paths) (string, error) {
	fi, err := os.Lstat(p.SPA)
	if errors.Is(err, os.ErrNotExist) || (err == nil && fi.Mode()&os.ModeSymlink == 0) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	target, err := readLinkAbs(p.SPA)
	if err != nil {
		return "", err
	}
	return filepath.Base(target), nil
}

// adoptPlainSPADir moves a pre-existing real directory at p.SPA into the
// releases directory so a symlink can replace it, and records it as previous.
func adoptPlainSPADir(p Paths, previous *string) error {
	fi, err := os.Lstat(p.SPA)
	if errors.Is(err, os.ErrNotExist) || (err == nil && fi.Mode()&os.ModeSymlink != 0) {
		return nil
	}
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return fmt.Errorf("%s exists and is not a directory or symlink", p.SPA)
	}
	name := uniqueName(p.appReleases(), "adopted")
	if err := os.Rename(p.SPA, filepath.Join(p.appReleases(), name)); err != nil {
		return fmt.Errorf("move existing %s aside: %w", p.SPA, err)
	}
	*previous = name
	return nil
}

// appRoot finds the directory in stage that holds index.html: stage itself,
// or its only subdirectory.
func appRoot(stage string) (string, error) {
	if isRegular(filepath.Join(stage, "index.html")) {
		return stage, nil
	}
	entries, err := os.ReadDir(stage)
	if err != nil {
		return "", err
	}
	if len(entries) == 1 && entries[0].IsDir() {
		sub := filepath.Join(stage, entries[0].Name())
		if isRegular(filepath.Join(sub, "index.html")) {
			return sub, nil
		}
	}
	return "", fmt.Errorf("the app build has no index.html at its top level (point at the build output directory, e.g. dist/)")
}

func isRegular(p string) bool {
	fi, err := os.Lstat(p)
	return err == nil && fi.Mode().IsRegular()
}

// appBudget enforces the size and file-count limits across one install.
type appBudget struct{ bytes, files int64 }

func (b *appBudget) add(n int64) error {
	b.files++
	b.bytes += n
	if b.files > maxAppFiles {
		return fmt.Errorf("the app build has more than %d files", maxAppFiles)
	}
	if b.bytes > maxAppBytes {
		return fmt.Errorf("the app build is larger than %d MB", maxAppBytes>>20)
	}
	return nil
}

// copyTree copies the regular files and directories under src into dst.
// Symlinks and special files are refused: a build output has none, and
// following one could copy something from outside the build.
func copyTree(src, dst string) error {
	var budget appBudget
	var dirs []string
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		switch {
		case d.IsDir():
			if rel != "." {
				if err := os.Mkdir(target, 0o755); err != nil {
					return err
				}
			}
			dirs = append(dirs, target)
			return nil
		case d.Type().IsRegular():
			fi, err := d.Info()
			if err != nil {
				return err
			}
			if err := budget.add(fi.Size()); err != nil {
				return err
			}
			f, err := os.Open(p)
			if err != nil {
				return err
			}
			defer f.Close()
			n, err := copyFileSync(target, f, fi.Size()+1, 0o644)
			if err != nil {
				return fmt.Errorf("copy %s: %w", rel, err)
			}
			if n != fi.Size() {
				return fmt.Errorf("copy %s: file changed while being copied", rel)
			}
			return nil
		default:
			return fmt.Errorf("%s is a symlink or special file; the app build must contain only files and directories", rel)
		}
	})
	if err != nil {
		return err
	}
	return syncDirs(dirs)
}

// extractTarGz unpacks a gzipped tar into dst. Only regular files and
// directories are accepted, and every name is checked before it is joined
// onto dst, so an entry cannot land outside it.
func extractTarGz(src, dst string) error {
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("%s is not a gzip file: %w", filepath.Base(src), err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)

	var budget appBudget
	dirs := []string{dst}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("read %s: %w", filepath.Base(src), err)
		}
		name, err := safeArchiveName(h.Name)
		if err != nil {
			return err
		}
		if name == "" { // "./" — the archive's own root
			continue
		}
		target := filepath.Join(dst, filepath.FromSlash(name))
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			dirs = append(dirs, target)
		case tar.TypeReg:
			if err := budget.add(h.Size); err != nil {
				return err
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			n, err := copyFileSync(target, tr, h.Size+1, 0o644)
			if err != nil {
				return fmt.Errorf("extract %s: %w", name, err)
			}
			if n != h.Size {
				return fmt.Errorf("extract %s: truncated", name)
			}
			dirs = append(dirs, filepath.Dir(target))
		case tar.TypeXGlobalHeader:
			// pax metadata, not a file
		default:
			return fmt.Errorf("%s in the archive is a link or special file; the app build must contain only files and directories", h.Name)
		}
	}
	return syncDirs(dirs)
}

// safeArchiveName normalises an archive entry name and rejects any that
// could escape the extraction directory. "" means the archive root itself.
func safeArchiveName(name string) (string, error) {
	if strings.Contains(name, `\`) || strings.HasPrefix(name, "/") {
		return "", fmt.Errorf("archive entry %q has an unsafe name", name)
	}
	clean := path.Clean(name)
	if clean == "." {
		return "", nil
	}
	if !fs.ValidPath(clean) || (len(clean) >= 2 && clean[1] == ':') {
		return "", fmt.Errorf("archive entry %q has an unsafe name", name)
	}
	return clean, nil
}

func syncDirs(dirs []string) error {
	seen := map[string]bool{}
	for _, d := range dirs {
		if seen[d] {
			continue
		}
		seen[d] = true
		if err := syncDir(d); err != nil {
			return err
		}
	}
	return nil
}
