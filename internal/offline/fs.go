package offline

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
)

// writeFileSync writes data to path and fsyncs it before returning.
func writeFileSync(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// copyFileSync copies at most limit bytes from r to a new file at path,
// fsyncs it, and returns how many bytes were written. The caller decides
// whether a copy that hit the limit is an error.
func copyFileSync(path string, r io.Reader, limit int64, mode os.FileMode) (int64, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return 0, err
	}
	n, err := io.Copy(f, io.LimitReader(r, limit))
	if err != nil {
		f.Close()
		return n, err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return n, err
	}
	return n, f.Close()
}

// uniqueName returns base, or base.2, base.3, ... — the first that does not
// exist in dir.
func uniqueName(dir, base string) string {
	name := base
	for i := 2; ; i++ {
		if _, err := os.Lstat(filepath.Join(dir, name)); os.IsNotExist(err) {
			return name
		}
		name = base + "." + strconv.Itoa(i)
	}
}

// swapSymlink points link at target atomically: a new symlink is made beside
// it and renamed over it, so a reader resolving link sees the old target or
// the new one and never nothing. The containing directory is fsynced so the
// swap survives a power cut.
func swapSymlink(link, target string) error {
	tmp := link + ".tmp"
	// A leftover from an install that died between these two steps.
	_ = os.Remove(tmp)
	if err := os.Symlink(target, tmp); err != nil {
		return fmt.Errorf("create symlink %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, link); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("replace %s: %w", link, err)
	}
	return syncDir(filepath.Dir(link))
}

// readLinkAbs resolves a single level of symlink at link to an absolute
// path. A relative target is taken relative to link's directory.
func readLinkAbs(link string) (string, error) {
	target, err := os.Readlink(link)
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(filepath.Dir(link), target)
	}
	return filepath.Clean(target), nil
}

// prune deletes every entry of dir whose name is not in keep, and returns
// the names it removed, sorted. Stale .staging-* directories from an install
// that died go the same way; callers hold the install lock, so no staging
// directory can belong to a live install.
func prune(dir string, keep ...string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	keepSet := make(map[string]bool, len(keep))
	for _, k := range keep {
		if k != "" {
			keepSet[k] = true
		}
	}
	var removed []string
	for _, e := range entries {
		if keepSet[e.Name()] {
			continue
		}
		if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
			return removed, fmt.Errorf("remove old %s: %w", e.Name(), err)
		}
		removed = append(removed, e.Name())
	}
	sort.Strings(removed)
	return removed, nil
}
