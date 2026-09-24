package store

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/LensBridge/agent/internal/fsutil"
	"github.com/LensBridge/agent/internal/mbu"
)

// Content is an installed content bundle.
type Content struct {
	Dir      string
	Manifest *mbu.Manifest
	// Info is the bundle's install record (source, time).
	Info InstallInfo
	loc  *time.Location
}

// InstallInfo records how a bundle or release arrived.
type InstallInfo struct {
	Source      string `json:"source"`
	InstalledAt string `json:"installedAt"`
}

const installInfoName = "installed.json"

// Location is the bundle's timezone.
func (c *Content) Location() *time.Location { return c.loc }

// PayloadPath is the payload file for date (YYYY-MM-DD, already validated by
// the caller as one of the bundle's days).
func (c *Content) PayloadPath(date string) string {
	return filepath.Join(c.Dir, "payloads", date+".json")
}

// contentCache avoids re-parsing a bundle's manifest on every request:
// installed bundle directories never change after their rename into place.
var contentCache struct {
	sync.Mutex
	dir string
	c   *Content
}

// CurrentContent loads the bundle current points at.
func (l Layout) CurrentContent() (*Content, error) {
	dir, err := fsutil.ReadLinkAbs(l.ContentCurrent())
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNoContent
	}
	if err != nil {
		return nil, err
	}
	contentCache.Lock()
	defer contentCache.Unlock()
	if contentCache.dir == dir && contentCache.c != nil {
		return contentCache.c, nil
	}
	c, err := readContent(dir)
	if err != nil {
		return nil, fmt.Errorf("installed content %s is unreadable: %w", filepath.Base(dir), err)
	}
	contentCache.dir, contentCache.c = dir, c
	return c, nil
}

func readContent(dir string) (*Content, error) {
	raw, err := os.ReadFile(filepath.Join(dir, mbu.ManifestName))
	if err != nil {
		return nil, err
	}
	m, err := mbu.ParseManifest(raw)
	if err != nil {
		return nil, err
	}
	if m.Type != mbu.TypeContent {
		return nil, fmt.Errorf("not a content bundle")
	}
	loc, err := time.LoadLocation(m.Content.Timezone)
	if err != nil {
		return nil, err
	}
	c := &Content{Dir: dir, Manifest: m, loc: loc}
	if raw, err := os.ReadFile(filepath.Join(dir, installInfoName)); err == nil {
		_ = json.Unmarshal(raw, &c.Info)
	}
	return c, nil
}

// MediaPath is where the media file with base name file lives in the store.
func (l Layout) MediaPath(file string) string { return filepath.Join(l.MediaDir(), file) }

// HaveMedia reports whether the store holds a media file with this sha256.
func (l Layout) HaveMedia(sha string) bool {
	matches, _ := filepath.Glob(filepath.Join(l.MediaDir(), sha+".*"))
	for _, m := range matches {
		if mbu.MediaFileRE.MatchString(filepath.Base(m)) {
			return true
		}
	}
	return false
}

// MediaHashes lists the sha256 of every media file in the store.
func (l Layout) MediaHashes() []string {
	entries, _ := os.ReadDir(l.MediaDir())
	var out []string
	for _, e := range entries {
		if mbu.MediaFileRE.MatchString(e.Name()) {
			out = append(out, strings.SplitN(e.Name(), ".", 2)[0])
		}
	}
	sort.Strings(out)
	return out
}

// InstallContent installs an authenticated content package and makes it
// current. changed reports whether its payloads differ from what was current
// before (a sync that only moved the window forward by nothing, or re-sent
// the same content, is not worth a repaint).
func (l Layout) InstallContent(pkg *mbu.Package, source string, now time.Time) (changed bool, err error) {
	m := pkg.Manifest
	for _, d := range []string{l.BundlesDir(), l.MediaDir()} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			return false, err
		}
	}
	unlock, err := fsutil.LockDir(l.contentDir())
	if err != nil {
		return false, err
	}
	defer unlock()

	// Media first: into the shared store, each file verified as it is
	// written and only then renamed to its hash name.
	media := make(map[string]bool)
	for _, f := range m.Files {
		if !strings.HasPrefix(f.Path, "media/") {
			continue
		}
		name := strings.TrimPrefix(f.Path, "media/")
		media[name] = true
		if _, err := os.Stat(l.MediaPath(name)); err == nil {
			continue
		}
		if !pkg.Has(f.Path) {
			return false, fmt.Errorf("%s is neither in the package nor on this board", f.Path)
		}
		if err := l.storeMedia(pkg, f.Path, name); err != nil {
			return false, err
		}
	}
	if err := fsutil.SyncDir(l.MediaDir()); err != nil {
		return false, err
	}

	stage, err := os.MkdirTemp(l.BundlesDir(), ".staging-")
	if err != nil {
		return false, err
	}
	committed := false
	defer func() {
		if !committed {
			os.RemoveAll(stage)
		}
	}()
	if err := os.Chmod(stage, 0o750); err != nil {
		return false, err
	}
	if err := os.Mkdir(filepath.Join(stage, "payloads"), 0o750); err != nil {
		return false, err
	}
	days, _ := m.Content.Days()
	for _, d := range days {
		p := "payloads/" + d + ".json"
		raw, err := pkg.ReadFile(p, mbu.MaxPayloadBytes)
		if err != nil {
			return false, err
		}
		if err := CheckPayload(raw, media); err != nil {
			return false, fmt.Errorf("%s: %w", p, err)
		}
		if err := fsutil.WriteFileSync(filepath.Join(stage, "payloads", d+".json"), raw, 0o640); err != nil {
			return false, err
		}
	}
	info, _ := json.Marshal(InstallInfo{Source: source, InstalledAt: now.UTC().Format(time.RFC3339)})
	for name, data := range map[string][]byte{
		mbu.ManifestName:  pkg.RawManifest,
		mbu.SignatureName: pkg.RawSig,
		installInfoName:   info,
	} {
		if err := fsutil.WriteFileSync(filepath.Join(stage, name), data, 0o640); err != nil {
			return false, err
		}
	}
	for _, d := range []string{filepath.Join(stage, "payloads"), stage} {
		if err := fsutil.SyncDir(d); err != nil {
			return false, err
		}
	}

	prev, _ := l.CurrentContent()
	name := fsutil.UniqueName(l.BundlesDir(), strconv.FormatInt(m.Sequence, 10))
	if err := os.Rename(stage, filepath.Join(l.BundlesDir(), name)); err != nil {
		return false, err
	}
	committed = true
	if err := fsutil.SyncDir(l.BundlesDir()); err != nil {
		return false, err
	}
	if err := fsutil.SwapSymlink(l.ContentCurrent(), filepath.Join("bundles", name)); err != nil {
		return false, err
	}

	changed = prev == nil || payloadsDiffer(prev.Manifest, m)
	prevName := ""
	if prev != nil {
		prevName = filepath.Base(prev.Dir)
	}
	if _, err := fsutil.Prune(l.BundlesDir(), name, prevName); err != nil {
		return changed, fmt.Errorf("content installed, but removing old bundles failed: %w", err)
	}
	if err := l.gcMedia(); err != nil {
		return changed, fmt.Errorf("content installed, but removing unused media failed: %w", err)
	}
	return changed, nil
}

func (l Layout) storeMedia(pkg *mbu.Package, path, name string) error {
	tmp, err := os.CreateTemp(l.MediaDir(), ".incoming-")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := pkg.CopyFile(path, tmp); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o640); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), l.MediaPath(name))
}

// gcMedia deletes media no kept bundle references. Callers hold the content
// lock.
func (l Layout) gcMedia() error {
	used := map[string]bool{}
	bundles, err := os.ReadDir(l.BundlesDir())
	if err != nil {
		return err
	}
	for _, b := range bundles {
		c, err := readContent(filepath.Join(l.BundlesDir(), b.Name()))
		if err != nil {
			// An unreadable kept bundle: keep every file rather than guess.
			return nil
		}
		for _, f := range c.Manifest.Files {
			if strings.HasPrefix(f.Path, "media/") {
				used[strings.TrimPrefix(f.Path, "media/")] = true
			}
		}
	}
	entries, err := os.ReadDir(l.MediaDir())
	if err != nil {
		return err
	}
	for _, e := range entries {
		if used[e.Name()] || e.Name() == ".lock" {
			continue
		}
		if err := os.RemoveAll(filepath.Join(l.MediaDir(), e.Name())); err != nil {
			return err
		}
	}
	return nil
}

func payloadsDiffer(a, b *mbu.Manifest) bool {
	hashes := func(m *mbu.Manifest) map[string]string {
		out := map[string]string{}
		for _, f := range m.Files {
			if strings.HasPrefix(f.Path, "payloads/") {
				out[f.Path] = f.SHA256
			}
		}
		return out
	}
	ha, hb := hashes(a), hashes(b)
	if len(ha) != len(hb) {
		return true
	}
	for k, v := range ha {
		if hb[k] != v {
			return true
		}
	}
	return false
}

// CheckPayload requires raw to be one JSON object and every non-empty
// posterUrl anywhere in it to name a media file of the bundle. The walk is
// structural rather than tied to the payload schema, so a new frame type that
// carries a poster is covered without a change here. A posterUrl pointing
// anywhere else would be a blank tile on a board with no internet, or a
// request to an address the content key's holder chose.
func CheckPayload(raw []byte, media map[string]bool) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return fmt.Errorf("not valid JSON: %w", err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return fmt.Errorf("not valid JSON: trailing data after the object")
	}
	if _, ok := v.(map[string]any); !ok {
		return fmt.Errorf("not a JSON object")
	}
	return walkPosterURLs(v, media)
}

func walkPosterURLs(v any, media map[string]bool) error {
	switch t := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if k == "posterUrl" {
				if s, ok := t[k].(string); ok && s != "" {
					file, ok := strings.CutPrefix(s, "/media/")
					if !ok || !media[file] {
						return fmt.Errorf("posterUrl %q is not a media file of this bundle", s)
					}
				}
			}
			if err := walkPosterURLs(t[k], media); err != nil {
				return err
			}
		}
	case []any:
		for _, c := range t {
			if err := walkPosterURLs(c, media); err != nil {
				return err
			}
		}
	}
	return nil
}

// sha256Hex is used by tests and by callers checking stored media.
func sha256Hex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}
