// Package fsutil holds the small filesystem primitives every installer on the
// board is built from: fsynced writes, atomic replace, atomic symlink swap and
// an exclusive directory lock.
//
// Every install in the agent follows one shape: write into a staging
// directory, fsync the files and the directory, rename into place, then swap a
// symlink. A power cut at any point before the swap leaves the old state
// serving; after it, the new. These helpers are that shape's parts.
package fsutil

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
)

// WriteFileSync creates path (which must not exist), writes data and fsyncs it.
func WriteFileSync(path string, data []byte, mode os.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
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

// CopyFileSync copies at most limit bytes from r to a new file at path, fsyncs
// it, and returns how many bytes were written. The caller decides whether a
// copy that reached the limit is an error.
func CopyFileSync(path string, r io.Reader, limit int64, mode os.FileMode) (int64, error) {
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

// WriteAtomic replaces path with data: a temp file beside it is written,
// fsynced and renamed over it, then the directory is fsynced. Readers see the
// old content or the new, never a partial file.
func WriteAtomic(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-")
	if err != nil {
		return err
	}
	tmp := f.Name()
	fail := func(err error) error {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if _, err := f.Write(data); err != nil {
		return fail(err)
	}
	if err := f.Chmod(mode); err != nil {
		return fail(err)
	}
	if err := f.Sync(); err != nil {
		return fail(err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return SyncDir(dir)
}

// UniqueName returns base, or base.2, base.3, ...: the first that does not
// exist in dir.
func UniqueName(dir, base string) string {
	name := base
	for i := 2; ; i++ {
		if _, err := os.Lstat(filepath.Join(dir, name)); os.IsNotExist(err) {
			return name
		}
		name = base + "." + strconv.Itoa(i)
	}
}

// SwapSymlink points link at target atomically: a new symlink is made beside
// it and renamed over it, so a reader resolving link sees the old target or
// the new one and never nothing. The containing directory is fsynced so the
// swap survives a power cut.
func SwapSymlink(link, target string) error {
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
	return SyncDir(filepath.Dir(link))
}

// ReadLinkAbs resolves one level of symlink at link to an absolute path. A
// relative target is taken relative to link's directory.
func ReadLinkAbs(link string) (string, error) {
	target, err := os.Readlink(link)
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(filepath.Dir(link), target)
	}
	return filepath.Clean(target), nil
}

// Prune deletes every entry of dir whose name is not in keep, and returns the
// names it removed, sorted. Callers hold the directory's install lock, so a
// leftover .staging-* directory cannot belong to a live install and goes too.
func Prune(dir string, keep ...string) ([]string, error) {
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
