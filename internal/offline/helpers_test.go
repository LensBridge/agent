package offline

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	// Tests run on Windows dev boxes too; don't depend on a system zoneinfo.
	_ "time/tzdata"
)

const testDevice = "3f2a1b4c-0000-4000-8000-000000000001"

// requireSymlinks skips the test where the OS will not let this process make
// symlinks — Windows without Developer Mode. Production only runs on Linux,
// where the atomic swap depends on them; the code is not weakened for this.
func requireSymlinks(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	if err := os.Symlink("x", filepath.Join(dir, "probe")); err != nil {
		t.Skipf("symlinks unavailable on this machine (%v); the install swap needs them", err)
	}
}

// bundleSpec is a bundle under construction. newBundle returns a valid one;
// tests break it in exactly one way and build the zip.
type bundleSpec struct {
	manifest   Manifest
	noManifest bool
	payloads   map[string][]byte // date -> JSON
	media      map[string][]byte // "media/<sha>.<ext>" -> bytes
	extra      []zipEntry        // written verbatim, after everything else
}

type zipEntry struct {
	name string
	data []byte
}

// testImage is the one poster every generated payload refers to.
var testImage = []byte("\xff\xd8\xff\xe0 not really a jpeg")

func sha(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// newBundle returns a valid bundle for testDevice covering n days from first,
// generated at gen.
func newBundle(first string, n int, gen string) *bundleSpec {
	f, _ := time.Parse(dateLayout, first)
	last := f.AddDate(0, 0, n-1).Format(dateLayout)
	h := sha(testImage)
	mediaPath := "media/" + h + ".jpg"

	b := &bundleSpec{
		manifest: Manifest{
			FormatVersion: 1,
			DeviceID:      testDevice,
			Timezone:      "America/Toronto",
			GeneratedAt:   gen,
			FirstDay:      first,
			LastDay:       last,
			Media: []MediaEntry{{
				Path: mediaPath, SHA256: h, Bytes: int64(len(testImage)), ContentType: "image/jpeg",
			}},
		},
		payloads: map[string][]byte{},
		media:    map[string][]byte{mediaPath: testImage},
	}
	for d := f; n > 0; d, n = d.AddDate(0, 0, 1), n-1 {
		day := d.Format(dateLayout)
		b.payloads[day] = payloadFor(day, "/media/"+h+".jpg")
	}
	return b
}

// payloadFor is a cut-down MusallahBoardPayload: enough shape to exercise the
// posterUrl walk, with the date in it so tests can tell days apart.
func payloadFor(day, posterURL string) []byte {
	p := map[string]any{
		"day":     day,
		"weather": nil,
		"frames": []any{
			map[string]any{"frameType": "POSTER", "frameConfig": map[string]any{"posterUrl": posterURL}},
			map[string]any{"frameType": "PRAYER", "frameConfig": map[string]any{"title": "Salah"}},
		},
	}
	raw, _ := json.Marshal(p)
	return raw
}

// write builds the zip in dir and returns its path.
func (b *bundleSpec) write(t *testing.T, dir string) string {
	t.Helper()
	p := filepath.Join(dir, fmt.Sprintf("bundle-%d.zip", time.Now().UnixNano()))
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	add := func(name string, data []byte) {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if !b.noManifest {
		raw, err := json.Marshal(b.manifest)
		if err != nil {
			t.Fatal(err)
		}
		add("manifest.json", raw)
	}
	for _, d := range sortedKeys(b.payloads) {
		add("payloads/"+d+".json", b.payloads[d])
	}
	for _, m := range sortedKeys(b.media) {
		add(m, b.media[m])
	}
	for _, e := range b.extra {
		add(e.name, e.data)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return p
}

// listDir returns the sorted entry names of dir.
func listDir(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names
}

func testPaths(t *testing.T) Paths {
	t.Helper()
	dir := t.TempDir()
	return Paths{Root: filepath.Join(dir, "offline"), SPA: filepath.Join(dir, "board")}
}
