package offline

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Size limits. MaxBundleBytes is the contract's 200 MB cap on the zip itself;
// the others bound what a small zip can inflate to, so a malformed or hostile
// bundle cannot fill the SD card before its hashes are even checked.
const (
	MaxBundleBytes   = 200 << 20
	maxManifestBytes = 1 << 20
	maxPayloadBytes  = 16 << 20
	maxMediaTotal    = 512 << 20
)

// extractBundle validates the bundle zip at zipPath against Contract 1 and
// writes its contents into stageDir, which must exist and be empty. It writes
// nowhere else. Every file it writes is fsynced; so are the directories.
//
// On error stageDir may hold a partial extraction — the caller discards it.
// Nothing in the zip is ever used as a filesystem path until it has matched
// one of the entry-name patterns, so a name like "../../etc/passwd" or
// "/media/x" is rejected before it can be joined onto anything.
func extractBundle(zipPath, stageDir, deviceID string) (*Manifest, error) {
	fi, err := os.Stat(zipPath)
	if err != nil {
		return nil, err
	}
	if fi.Size() > MaxBundleBytes {
		return nil, fmt.Errorf("%s is %d MB; bundles over %d MB are refused",
			filepath.Base(zipPath), fi.Size()>>20, MaxBundleBytes>>20)
	}

	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		// Includes zip.ErrInsecurePath when GODEBUG asks for it; either way
		// the file is not something we install.
		return nil, fmt.Errorf("%s is not a readable zip file: %w", filepath.Base(zipPath), err)
	}
	defer zr.Close()

	// Pass 1: classify every entry by name. Nothing is read yet.
	var manifestFile *zip.File
	payloads := map[string]*zip.File{} // date -> entry
	media := map[string]*zip.File{}    // "media/<sha>.<ext>" -> entry
	for _, f := range zr.File {
		name := f.Name
		// Bare directory entries for the two allowed directories carry no
		// data; some zip writers emit them and some do not.
		if name == "payloads/" || name == "media/" {
			continue
		}
		if !f.Mode().IsRegular() {
			return nil, fmt.Errorf("bundle entry %q is not a regular file", name)
		}
		switch {
		case name == "manifest.json":
			if manifestFile != nil {
				return nil, fmt.Errorf("bundle contains manifest.json twice")
			}
			manifestFile = f
		case payloadNameRE.MatchString(name):
			date := payloadNameRE.FindStringSubmatch(name)[1]
			if payloads[date] != nil {
				return nil, fmt.Errorf("bundle contains %s twice", name)
			}
			payloads[date] = f
		case mediaNameRE.MatchString(name):
			if media[name] != nil {
				return nil, fmt.Errorf("bundle contains %s twice", name)
			}
			media[name] = f
		default:
			return nil, fmt.Errorf("bundle contains %q, which is not allowed (only manifest.json, payloads/<date>.json and media/<sha256>.<ext>)", name)
		}
	}
	if manifestFile == nil {
		return nil, fmt.Errorf("bundle has no manifest.json")
	}

	rawManifest, err := readEntry(manifestFile, maxManifestBytes)
	if err != nil {
		return nil, err
	}
	m, err := parseManifest(rawManifest)
	if err != nil {
		return nil, err
	}
	if m.DeviceID != deviceID {
		return nil, fmt.Errorf("this bundle is for device %s, but this board is %s (download the bundle for this board from LensBridge)",
			m.DeviceID, deviceID)
	}

	// The payload set must be exactly firstDay..lastDay.
	days := m.Days()
	want := make(map[string]bool, len(days))
	for _, d := range days {
		want[d] = true
		if payloads[d] == nil {
			return nil, fmt.Errorf("bundle is missing payloads/%s.json (the manifest covers %s to %s)", d, m.FirstDay, m.LastDay)
		}
	}
	for _, d := range sortedKeys(payloads) {
		if !want[d] {
			return nil, fmt.Errorf("bundle has payloads/%s.json, outside its range %s to %s", d, m.FirstDay, m.LastDay)
		}
	}

	// The media set must be exactly the manifest's.
	var mediaTotal int64
	for _, e := range m.Media {
		if media[e.Path] == nil {
			return nil, fmt.Errorf("manifest lists %s but the bundle does not contain it", e.Path)
		}
		mediaTotal += e.Bytes
	}
	if mediaTotal > maxMediaTotal {
		return nil, fmt.Errorf("bundle media totals %d MB; at most %d MB is allowed", mediaTotal>>20, maxMediaTotal>>20)
	}
	listed := m.mediaByFile()
	for _, name := range sortedKeys(media) {
		if _, ok := listed[strings.TrimPrefix(name, "media/")]; !ok {
			return nil, fmt.Errorf("bundle contains %s, which the manifest does not list", name)
		}
	}

	// Pass 2: read, check and write. Validation of each file happens before
	// or while it is written into the staging directory, never elsewhere.
	for _, dir := range []string{"payloads", "media"} {
		if err := os.Mkdir(filepath.Join(stageDir, dir), 0o755); err != nil {
			return nil, err
		}
	}
	if err := writeFileSync(filepath.Join(stageDir, "manifest.json"), rawManifest); err != nil {
		return nil, err
	}

	for _, d := range days {
		raw, err := readEntry(payloads[d], maxPayloadBytes)
		if err != nil {
			return nil, err
		}
		if err := checkPayload(raw, listed); err != nil {
			return nil, fmt.Errorf("payloads/%s.json: %w", d, err)
		}
		if err := writeFileSync(filepath.Join(stageDir, "payloads", d+".json"), raw); err != nil {
			return nil, err
		}
	}

	for _, e := range m.Media {
		if err := extractMedia(media[e.Path], e, stageDir); err != nil {
			return nil, err
		}
	}

	for _, dir := range []string{"payloads", "media", ""} {
		if err := syncDir(filepath.Join(stageDir, dir)); err != nil {
			return nil, err
		}
	}
	return m, nil
}

// readEntry reads a whole zip entry, refusing one larger than limit. Reading
// to EOF is also what makes archive/zip verify the entry's CRC.
func readEntry(f *zip.File, limit int64) ([]byte, error) {
	rc, err := f.Open()
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", f.Name, err)
	}
	defer rc.Close()
	raw, err := io.ReadAll(io.LimitReader(rc, limit+1))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", f.Name, err)
	}
	if int64(len(raw)) > limit {
		return nil, fmt.Errorf("%s is larger than %d MB", f.Name, limit>>20)
	}
	return raw, nil
}

// extractMedia streams one media entry to disk, hashing as it goes, and
// checks the result against the manifest. The name was already matched
// against mediaNameRE, so joining it onto stageDir is safe.
func extractMedia(f *zip.File, e MediaEntry, stageDir string) error {
	rc, err := f.Open()
	if err != nil {
		return fmt.Errorf("read %s: %w", e.Path, err)
	}
	defer rc.Close()

	h := sha256.New()
	// One byte over the declared size is enough to tell "too big" apart,
	// and reading to EOF lets archive/zip check the CRC.
	n, err := copyFileSync(filepath.Join(stageDir, filepath.FromSlash(e.Path)), io.TeeReader(rc, h), e.Bytes+1, 0o644)
	if err != nil {
		return fmt.Errorf("extract %s: %w", e.Path, err)
	}
	if n != e.Bytes {
		return fmt.Errorf("%s is %d bytes but the manifest says %d", e.Path, n, e.Bytes)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != e.SHA256 {
		return fmt.Errorf("%s is corrupt: its SHA-256 is %s, the manifest says %s", e.Path, got, e.SHA256)
	}
	return nil
}

// checkPayload requires raw to be a JSON object and every posterUrl anywhere
// in it to name a media file in the bundle.
//
// The walk is structural rather than tied to the payload schema, so a new
// frame type that carries a poster is covered without a change here.
// Contract 1 has the exporter rewrite every posterUrl to /media/<file>; one
// that still points anywhere else would be a blank tile on a board with no
// internet, so it is refused too rather than discovered on screen.
func checkPayload(raw []byte, media map[string]MediaEntry) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return fmt.Errorf("not valid JSON: %w", err)
	}
	if dec.More() {
		return fmt.Errorf("not valid JSON: trailing data after the object")
	}
	if _, ok := v.(map[string]any); !ok {
		return fmt.Errorf("not a JSON object")
	}
	return walkPosterURLs(v, media)
}

func walkPosterURLs(v any, media map[string]MediaEntry) error {
	switch t := v.(type) {
	case map[string]any:
		// Sorted so the first error reported is the same every time.
		for _, k := range sortedKeys(t) {
			child := t[k]
			if k == "posterUrl" {
				if s, ok := child.(string); ok && s != "" {
					if err := checkPosterURL(s, media); err != nil {
						return err
					}
				}
			}
			if err := walkPosterURLs(child, media); err != nil {
				return err
			}
		}
	case []any:
		for _, child := range t {
			if err := walkPosterURLs(child, media); err != nil {
				return err
			}
		}
	}
	return nil
}

func checkPosterURL(s string, media map[string]MediaEntry) error {
	file, ok := strings.CutPrefix(s, "/media/")
	if !ok {
		return fmt.Errorf("posterUrl %q is not a /media/ path; an offline board cannot load images from anywhere else", s)
	}
	if _, ok := media[file]; !ok {
		return fmt.Errorf("posterUrl %q refers to a media file that is not in the bundle", s)
	}
	return nil
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
