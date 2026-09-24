package localserver

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/LensBridge/agent/internal/importer"
	"github.com/LensBridge/agent/internal/mbu"
	"github.com/LensBridge/agent/internal/store"
	"github.com/LensBridge/agent/internal/trust"
)

const dev = "3f2a1b4c-5d6e-4f70-8a9b-0c1d2e3f4a5b"

var (
	ck = ed25519.NewKeyFromSeed(bytes.Repeat([]byte{1}, 32))
	rk = ed25519.NewKeyFromSeed(bytes.Repeat([]byte{2}, 32))
)

func pkg(t *testing.T, m mbu.Manifest, src []mbu.Source, k ed25519.PrivateKey) string {
	t.Helper()
	var buf bytes.Buffer
	if err := mbu.Build(&buf, m, src, []ed25519.PrivateKey{k}); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "p.mbu")
	os.WriteFile(p, buf.Bytes(), 0o644)
	return p
}

// installed returns a layout holding content for 2026-09-24..25 and an app.
func installed(t *testing.T) store.Layout {
	t.Helper()
	l := store.Layout{Root: t.TempDir()}
	ring := trust.NewRing([]ed25519.PublicKey{ck.Public().(ed25519.PublicKey)}, []ed25519.PublicKey{rk.Public().(ed25519.PublicKey)})
	im := importer.New(importer.Deps{Layout: l, DeviceID: dev, AgentVersion: "0.2.0",
		Ring: func() (*trust.Ring, error) { return ring, nil }})
	c := pkg(t, mbu.Manifest{Type: mbu.TypeContent, Sequence: 1, DeviceID: dev,
		Content: &mbu.ContentInfo{Timezone: "America/Toronto", FirstDay: "2026-09-24", LastDay: "2026-09-25"}},
		[]mbu.Source{
			{Path: "payloads/2026-09-24.json", Data: []byte(`{"day":24,"weather":null}`)},
			{Path: "payloads/2026-09-25.json", Data: []byte(`{"day":25,"weather":null}`)},
		}, ck)
	a := pkg(t, mbu.Manifest{Type: mbu.TypeApp, Version: "2.0.0", App: &mbu.AppInfo{LocalAPI: 2}},
		[]mbu.Source{{Path: "index.html", Data: []byte("<html>board")}, {Path: "assets/app.js", Data: []byte("js")}}, rk)
	b := im.Import(context.Background(), importer.SourceCLI, []string{c, a})
	if !b.OK() {
		t.Fatalf("setup import: %+v", b.Results)
	}
	return l
}

func get(t *testing.T, s *Server, path string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
	return w
}

func at(day int, hour int) func() time.Time {
	loc, _ := time.LoadLocation("America/Toronto")
	return func() time.Time { return time.Date(2026, 9, day, hour, 0, 0, 0, loc) }
}

func TestPayloadPicksDayAndClamps(t *testing.T) {
	l := installed(t)
	for _, c := range []struct {
		day  int
		want string
	}{{23, `"day":24`}, {24, `"day":24`}, {25, `"day":25`}, {28, `"day":25`}} {
		s := New(Deps{Layout: l, DeviceID: dev, Now: at(c.day, 12)})
		w := get(t, s, "/api/musallah/payload")
		if w.Code != 200 || !strings.Contains(w.Body.String(), c.want) {
			t.Errorf("day %d: %d %s", c.day, w.Code, w.Body)
		}
	}
}

func TestWeatherOverlay(t *testing.T) {
	l := installed(t)
	s := New(Deps{Layout: l, DeviceID: dev, Now: at(24, 12),
		Weather: func() (json.RawMessage, bool) { return json.RawMessage(`{"tempC":12}`), true }})
	if w := get(t, s, "/api/musallah/payload"); !strings.Contains(w.Body.String(), `"weather":{"tempC":12}`) {
		t.Fatalf("overlay missing: %s", w.Body)
	}
}

func TestNoContentNoApp(t *testing.T) {
	s := New(Deps{Layout: store.Layout{Root: t.TempDir()}, DeviceID: dev})
	if w := get(t, s, "/api/musallah/payload"); w.Code != 503 || !strings.Contains(w.Body.String(), "no content installed") {
		t.Errorf("payload without content: %d %s", w.Code, w.Body)
	}
	if w := get(t, s, "/"); w.Code != 503 || !strings.Contains(w.Body.String(), "Waiting for the board app") {
		t.Errorf("app page without app: %d", w.Code)
	}
	var st Status
	json.Unmarshal(get(t, s, "/api/local/status").Body.Bytes(), &st)
	if st.App != nil || st.Content != nil || st.LocalAPI != importer.LocalAPIVersion {
		t.Errorf("status %+v", st)
	}
}

func TestAppServing(t *testing.T) {
	s := New(Deps{Layout: installed(t), DeviceID: dev, Now: at(24, 12)})
	if w := get(t, s, "/"); !strings.Contains(w.Body.String(), "board") || w.Header().Get("Cache-Control") != cacheNoCache {
		t.Errorf("index: %s %v", w.Body, w.Header())
	}
	if w := get(t, s, "/some/route"); !strings.Contains(w.Body.String(), "board") {
		t.Errorf("SPA fallback: %s", w.Body)
	}
	if w := get(t, s, "/assets/app.js"); w.Header().Get("Cache-Control") != cacheImmutable {
		t.Errorf("asset caching: %v", w.Header())
	}
	if w := get(t, s, "/assets/missing.js"); w.Code != 404 {
		t.Errorf("missing asset: %d", w.Code)
	}
	if w := get(t, s, "/.mbu/mbu.json"); strings.Contains(w.Body.String(), "formatVersion") {
		t.Errorf("release metadata served")
	}
	if w := get(t, s, "/api/nope"); w.Code != 404 || !strings.Contains(w.Header().Get("Content-Type"), "json") {
		t.Errorf("unknown api: %d", w.Code)
	}
	if w := get(t, s, "/_mb/updating"); !strings.Contains(w.Body.String(), "mbUpdate") {
		t.Errorf("update screen missing")
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/local/status", nil))
	if w.Code != 405 {
		t.Errorf("POST: %d", w.Code)
	}
}

func TestStatus(t *testing.T) {
	s := New(Deps{Layout: installed(t), DeviceID: dev, AgentVersion: "0.2.0", Now: at(25, 12),
		Sync: func() any { return map[string]bool{"enabled": true} }})
	var st Status
	json.Unmarshal(get(t, s, "/api/local/status").Body.Bytes(), &st)
	if st.App.Version != "2.0.0" || st.Content.FirstDay != "2026-09-24" || *st.DaysRemaining != 0 ||
		*st.StaleDays != 0 || st.Content.Source != "cli" || st.Sync == nil {
		t.Errorf("status %+v", st)
	}
}
