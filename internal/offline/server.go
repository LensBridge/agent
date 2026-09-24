package offline

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Cache-Control values from Contract 2.
const (
	cacheNoStore   = "no-store"
	cacheNoCache   = "no-cache"
	cacheImmutable = "public, max-age=31536000, immutable"
)

// Server serves the board to the kiosk in offline mode: the SPA, today's
// payload, the bundle's media and a status object. It is read-only.
//
// The current symlink is resolved on every request, so a bundle installed by
// `bundle install` is served from the next request on, without a restart.
// Installed bundle directories never change after their rename into place,
// so the parsed manifest is cached per resolved directory.
type Server struct {
	paths    Paths
	deviceID string
	logger   *slog.Logger
	now      func() time.Time

	mu        sync.Mutex
	cachedDir string
	cachedM   *Manifest
}

// NewServer returns a Server for the enrolled deviceID. logger may be nil.
func NewServer(p Paths, deviceID string, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Server{paths: p, deviceID: deviceID, logger: logger, now: time.Now}
}

// Serve accepts connections on ln until ctx is cancelled, then shuts down
// gracefully. The caller creates ln (net.Listen("tcp", ListenAddr)) so that a
// port conflict is reported before anything depends on the server.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	srv := &http.Server{
		Handler:           s,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
	}()
	err := srv.Serve(ln)
	if errors.Is(err, http.ErrServerClosed) {
		<-done
		return nil
	}
	return err
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		writeJSONError(w, http.StatusMethodNotAllowed, "read-only")
		return
	}
	w.Header().Set("X-Content-Type-Options", "nosniff")

	switch p := r.URL.Path; {
	case p == "/api/musallah/payload":
		s.handlePayload(w, r)
	case p == "/api/local/status":
		s.handleStatus(w, r)
	case strings.HasPrefix(p, "/api/"):
		// Not an SPA route: the page parses these as JSON, and an index.html
		// fallback would turn a typo into a confusing parse error.
		writeJSONError(w, http.StatusNotFound, "not found")
	case strings.HasPrefix(p, "/media/"):
		s.handleMedia(w, r)
	default:
		s.handleSPA(w, r)
	}
}

// current returns the served bundle's directory and manifest.
func (s *Server) current() (string, *Manifest, error) {
	dir, err := readLinkAbs(s.paths.Current())
	if errors.Is(err, os.ErrNotExist) {
		return "", nil, ErrNoBundle
	}
	if err != nil {
		return "", nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if dir == s.cachedDir && s.cachedM != nil {
		return dir, s.cachedM, nil
	}
	m, err := readManifest(dir)
	if err != nil {
		return "", nil, err
	}
	s.cachedDir, s.cachedM = dir, m
	return dir, m, nil
}

func (s *Server) handlePayload(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", cacheNoStore)
	dir, m, err := s.current()
	if errors.Is(err, ErrNoBundle) {
		writeJSONError(w, http.StatusServiceUnavailable, "no bundle installed")
		return
	}
	if err != nil {
		s.logger.Error("offline: current bundle unreadable", "err", err)
		writeJSONError(w, http.StatusInternalServerError, "installed bundle is unreadable")
		return
	}
	if r.URL.Query().Get("deviceId") != m.DeviceID {
		writeJSONError(w, http.StatusNotFound, "unknown device")
		return
	}

	day := PickDay(m, s.now())
	// day.Serving is a date we formatted ourselves, not request input.
	raw, err := os.ReadFile(filepath.Join(dir, "payloads", day.Serving+".json"))
	if err != nil {
		s.logger.Error("offline: payload missing from installed bundle", "day", day.Serving, "err", err)
		writeJSONError(w, http.StatusInternalServerError, "payload missing from installed bundle")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(raw)
}

func (s *Server) handleStatus(w http.ResponseWriter, _ *http.Request) {
	st := ReadStatus(s.paths, "offline", s.deviceID, s.now())
	w.Header().Set("Cache-Control", cacheNoStore)
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) handleMedia(w http.ResponseWriter, r *http.Request) {
	file := strings.TrimPrefix(r.URL.Path, "/media/")
	// The name must look exactly like a bundle media file before it goes
	// anywhere near a path; that also rules out "..", "/" and "\".
	if !mediaFileRE.MatchString(file) {
		http.NotFound(w, r)
		return
	}
	dir, m, err := s.current()
	if err != nil {
		http.NotFound(w, r)
		return
	}
	e, ok := m.mediaByFile()[file]
	if !ok {
		http.NotFound(w, r)
		return
	}
	f, err := os.Open(filepath.Join(dir, "media", file))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if e.ContentType != "" {
		w.Header().Set("Content-Type", e.ContentType)
	}
	w.Header().Set("Cache-Control", cacheImmutable)
	http.ServeContent(w, r, file, fi.ModTime(), f)
}

// handleSPA serves the built frontend. Real files are served as-is; any other
// path is a client-side route and gets index.html. Missing files under
// /assets/ are a 404 instead: they are hashed build output, and handing back
// HTML under an immutable cache header for a stale script URL is worse than
// an honest failure.
func (s *Server) handleSPA(w http.ResponseWriter, r *http.Request) {
	// os.DirFS re-resolves s.paths.SPA on every open, so an `app install`
	// swapping that symlink is picked up here immediately. fs.ValidPath
	// rejects "..", so nothing outside the SPA directory is reachable.
	fsys := os.DirFS(s.paths.SPA)
	name := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
	if name == "" {
		name = "index.html"
	}

	if fs.ValidPath(name) && s.serveFile(w, r, fsys, name) {
		return
	}
	if strings.HasPrefix(name, "assets/") {
		http.NotFound(w, r)
		return
	}
	if !s.serveFile(w, r, fsys, "index.html") {
		w.Header().Set("Cache-Control", cacheNoStore)
		http.Error(w, "The board app is not installed on this device.", http.StatusServiceUnavailable)
	}
}

// serveFile serves name from fsys if it is a regular file, and reports
// whether it did.
func (s *Server) serveFile(w http.ResponseWriter, r *http.Request, fsys fs.FS, name string) bool {
	f, err := fsys.Open(name)
	if err != nil {
		return false
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() {
		return false
	}
	rs, ok := f.(io.ReadSeeker)
	if !ok {
		return false
	}

	switch {
	case strings.HasPrefix(name, "assets/"):
		w.Header().Set("Cache-Control", cacheImmutable)
	default:
		// index.html must be revalidated so a new build shows up; anything
		// else at the top level (favicon, manifest) is unhashed, so the same.
		w.Header().Set("Cache-Control", cacheNoCache)
	}
	if ct := contentTypeFor(name); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	http.ServeContent(w, r, path.Base(name), fi.ModTime(), rs)
	return true
}

// extraTypes covers what a Vite build emits that the minimal mime table on a
// fresh Pi OS install may not know. ServeContent would otherwise sniff them,
// and a font sniffed as application/octet-stream is refused by nosniff.
var extraTypes = map[string]string{
	".js":          "text/javascript; charset=utf-8",
	".mjs":         "text/javascript; charset=utf-8",
	".css":         "text/css; charset=utf-8",
	".html":        "text/html; charset=utf-8",
	".json":        "application/json",
	".svg":         "image/svg+xml",
	".woff":        "font/woff",
	".woff2":       "font/woff2",
	".ttf":         "font/ttf",
	".otf":         "font/otf",
	".ico":         "image/x-icon",
	".webmanifest": "application/manifest+json",
	".wasm":        "application/wasm",
}

func contentTypeFor(name string) string {
	ext := strings.ToLower(path.Ext(name))
	if ct, ok := extraTypes[ext]; ok {
		return ct
	}
	return mime.TypeByExtension(ext)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeJSONError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"message": msg})
}
