package offline

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newTestServer returns a server over fresh paths with a fixed clock and an
// SPA build on disk (a plain directory — SPA serving needs no symlink).
func newTestServer(t *testing.T, now time.Time) (*Server, Paths) {
	t.Helper()
	p := testPaths(t)
	files := map[string]string{
		"index.html":           "<!doctype html><title>board</title>",
		"assets/app-3f2a1b.js": "console.log('board')",
		"assets/font-9c.woff2": "wOF2",
		"favicon.ico":          "ico",
	}
	for name, body := range files {
		full := filepath.Join(p.SPA, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	s := NewServer(p, testDevice, nil)
	s.now = func() time.Time { return now }
	return s, p
}

func get(t *testing.T, h http.Handler, target string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
	return rec
}

var midRange = time.Date(2026, 9, 25, 16, 0, 0, 0, time.UTC)

func TestServerNoBundle(t *testing.T) {
	s, _ := newTestServer(t, midRange)

	rec := get(t, s, "/api/musallah/payload?deviceId="+testDevice)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("payload code = %d, want 503", rec.Code)
	}
	if strings.TrimSpace(rec.Body.String()) != `{"message":"no bundle installed"}` {
		t.Errorf("body = %s", rec.Body)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q", cc)
	}

	rec = get(t, s, "/api/local/status")
	var st Status
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil || rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if st.Bundle != nil || st.Mode != "offline" || st.DeviceID != testDevice {
		t.Errorf("status = %+v", st)
	}

	if rec := get(t, s, "/media/"+sha(testImage)+".jpg"); rec.Code != 404 {
		t.Errorf("media without bundle = %d", rec.Code)
	}
}

func TestServerWithBundle(t *testing.T) {
	requireSymlinks(t)
	s, p := newTestServer(t, midRange)
	if _, err := InstallBundle(p, newBundle("2026-09-24", 14, "2026-09-24T14:02:11Z").write(t, t.TempDir()), testDevice); err != nil {
		t.Fatal(err)
	}
	media := "/media/" + sha(testImage) + ".jpg"

	cases := []struct {
		name        string
		target      string
		code        int
		cache       string
		contentType string
		body        string // substring
	}{
		{"today's payload", "/api/musallah/payload?deviceId=" + testDevice, 200, "no-store", "application/json", `"day":"2026-09-25"`},
		{"wrong device", "/api/musallah/payload?deviceId=someone-else", 404, "no-store", "application/json", "unknown device"},
		{"no device", "/api/musallah/payload", 404, "no-store", "", ""},
		{"status", "/api/local/status", 200, "no-store", "application/json", `"servingDay":"2026-09-25"`},
		{"unknown api", "/api/musallah/other", 404, "", "application/json", ""},
		{"media", media, 200, "public, max-age=31536000, immutable", "image/jpeg", string(testImage)},
		{"unknown media", "/media/" + sha([]byte("nope")) + ".jpg", 404, "", "", ""},
		{"media traversal", "/media/../manifest.json", 404, "", "", ""},
		{"media bad name", "/media/manifest.json", 404, "", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := get(t, s, tc.target)
			if rec.Code != tc.code {
				t.Errorf("code = %d, want %d (%s)", rec.Code, tc.code, rec.Body)
			}
			if tc.cache != "" && rec.Header().Get("Cache-Control") != tc.cache {
				t.Errorf("Cache-Control = %q, want %q", rec.Header().Get("Cache-Control"), tc.cache)
			}
			if tc.contentType != "" && !strings.HasPrefix(rec.Header().Get("Content-Type"), tc.contentType) {
				t.Errorf("Content-Type = %q, want %q", rec.Header().Get("Content-Type"), tc.contentType)
			}
			if !strings.Contains(rec.Body.String(), tc.body) {
				t.Errorf("body %q missing %q", rec.Body, tc.body)
			}
		})
	}
}

// Payloads are served verbatim, and today is clamped to the bundle's range.
func TestServerPayloadClampAndVerbatim(t *testing.T) {
	requireSymlinks(t)
	s, p := newTestServer(t, midRange)
	b := newBundle("2026-09-24", 3, "2026-09-24T14:02:11Z")
	b.payloads["2026-09-26"] = []byte("{\"day\":\"2026-09-26\",   \"weather\":null}\n")
	if _, err := InstallBundle(p, b.write(t, t.TempDir()), testDevice); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		now  time.Time
		want string
	}{
		{time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC), `"day":"2026-09-24"`},
		{time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC), "{\"day\":\"2026-09-26\",   \"weather\":null}\n"},
	} {
		s.now = func() time.Time { return tc.now }
		rec := get(t, s, "/api/musallah/payload?deviceId="+testDevice)
		if !strings.Contains(rec.Body.String(), tc.want) {
			t.Errorf("at %s got %q, want %q", tc.now, rec.Body, tc.want)
		}
	}
}

// A new install is served from the next request, without a restart.
func TestServerPicksUpNewBundle(t *testing.T) {
	requireSymlinks(t)
	s, p := newTestServer(t, midRange)
	src := t.TempDir()
	if _, err := InstallBundle(p, newBundle("2026-09-24", 7, "2026-09-24T00:00:00Z").write(t, src), testDevice); err != nil {
		t.Fatal(err)
	}
	if rec := get(t, s, "/api/local/status"); !strings.Contains(rec.Body.String(), `"lastDay":"2026-09-30"`) {
		t.Fatalf("first status: %s", rec.Body)
	}
	if _, err := InstallBundle(p, newBundle("2026-09-25", 14, "2026-09-25T00:00:00Z").write(t, src), testDevice); err != nil {
		t.Fatal(err)
	}
	if rec := get(t, s, "/api/local/status"); !strings.Contains(rec.Body.String(), `"lastDay":"2026-10-08"`) {
		t.Errorf("status after reinstall still old: %s", rec.Body)
	}
	if _, m, _ := s.current(); m.FirstDay != "2026-09-25" {
		t.Errorf("server manifest firstDay = %s", m.FirstDay)
	}
}

func TestServerSPA(t *testing.T) {
	s, _ := newTestServer(t, midRange)
	cases := []struct {
		name, target string
		code         int
		cache        string
		contentType  string
		body         string
	}{
		{"root", "/", 200, "no-cache", "text/html", "<title>board</title>"},
		{"index", "/index.html", 200, "no-cache", "text/html", "<title>board</title>"},
		{"query is ignored", "/?deviceId=x&mode=offline", 200, "no-cache", "text/html", "<title>board</title>"},
		{"hashed asset", "/assets/app-3f2a1b.js", 200, "public, max-age=31536000, immutable", "text/javascript", "console.log"},
		{"font", "/assets/font-9c.woff2", 200, "public, max-age=31536000, immutable", "font/woff2", "wOF2"},
		{"top-level file", "/favicon.ico", 200, "no-cache", "image/x-icon", "ico"},
		{"client route falls back", "/diagnostics/panel", 200, "no-cache", "text/html", "<title>board</title>"},
		{"missing asset is a 404", "/assets/app-old.js", 404, "", "", ""},
		{"traversal falls back, never escapes", "/../../etc/passwd", 200, "no-cache", "text/html", "<title>board</title>"},
		{"directory falls back", "/assets", 200, "no-cache", "text/html", "<title>board</title>"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := get(t, s, tc.target)
			if rec.Code != tc.code {
				t.Fatalf("code = %d, want %d", rec.Code, tc.code)
			}
			if tc.cache != "" && rec.Header().Get("Cache-Control") != tc.cache {
				t.Errorf("Cache-Control = %q, want %q", rec.Header().Get("Cache-Control"), tc.cache)
			}
			if tc.contentType != "" && !strings.HasPrefix(rec.Header().Get("Content-Type"), tc.contentType) {
				t.Errorf("Content-Type = %q, want %q", rec.Header().Get("Content-Type"), tc.contentType)
			}
			if !strings.Contains(rec.Body.String(), tc.body) {
				t.Errorf("body = %q", rec.Body)
			}
		})
	}
}

func TestServerNoSPA(t *testing.T) {
	s, p := newTestServer(t, midRange)
	if err := os.RemoveAll(p.SPA); err != nil {
		t.Fatal(err)
	}
	if rec := get(t, s, "/"); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("code = %d, want 503", rec.Code)
	}
}

func TestServerReadOnly(t *testing.T) {
	s, _ := newTestServer(t, midRange)
	for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, httptest.NewRequest(m, "/api/musallah/payload", nil))
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s = %d, want 405", m, rec.Code)
		}
	}
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodHead, "/", nil))
	if rec.Code != 200 {
		t.Errorf("HEAD / = %d", rec.Code)
	}
}

// Serve answers on a real listener and returns nil once ctx is cancelled.
func TestServeLifecycle(t *testing.T) {
	s, _ := newTestServer(t, midRange)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx, ln) }()

	resp, err := http.Get("http://" + ln.Addr().String() + "/api/local/status")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("status = %d", resp.StatusCode)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Serve returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after cancel")
	}
}
