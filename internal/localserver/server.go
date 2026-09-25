// Package localserver is the agent's HTTP server for the kiosk on
// 127.0.0.1:8080 (docs/architecture.md, section 7): the board app, today's
// payload from the installed content, media, status, install events and the
// update screen.
//
// Every board is served from here, online or not. The server is read-only and
// loopback-only; installs arrive through the importer, never through it.
package localserver

import (
	"context"
	"encoding/json"
	"errors"
	"html"
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
	"time"

	"github.com/LensBridge/agent/internal/clock"
	"github.com/LensBridge/agent/internal/events"
	"github.com/LensBridge/agent/internal/mbu"
	"github.com/LensBridge/agent/internal/store"
	"github.com/LensBridge/agent/internal/updates"
	"github.com/LensBridge/agent/internal/updatescreen"
	updateanim "github.com/LensBridge/agent/update-anim"
)

// ListenAddr is loopback only: nothing off the box has any business here.
const ListenAddr = "127.0.0.1:8080"

// BaseURL is what the kiosk loads.
const BaseURL = "http://" + ListenAddr + "/"

// WeatherMaxAge is how long an overlaid weather object stays on screen.
const WeatherMaxAge = 3 * time.Hour

const (
	cacheNoStore   = "no-store"
	cacheNoCache   = "no-cache"
	cacheImmutable = "public, max-age=31536000, immutable"
)

// Deps is what the server reads.
type Deps struct {
	Layout       store.Layout
	DeviceID     string
	AgentVersion string
	Hub          *events.Hub
	// Sync returns the sync status object for /api/local/status. May be nil.
	Sync func() any
	// Weather returns a fresh weather object, if any. May be nil.
	Weather func() (json.RawMessage, bool)
	// UpdateActive reports whether the update screen is up. May be nil.
	UpdateActive func() bool
	// Updates reports the software waiting to install. May be nil.
	Updates func() updates.Info
	// Clock says whether the board's clock can be believed. May be nil.
	Clock func() clock.Info
	// ServicePort and USBImport are the board's offline routes (agent.toml).
	ServicePort, USBImport bool
	Logger                 *slog.Logger
	Now                    func() time.Time
}

// Server serves the kiosk.
type Server struct{ d Deps }

// New returns a Server.
func New(d Deps) *Server {
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.Logger == nil {
		d.Logger = slog.New(slog.DiscardHandler)
	}
	if d.Hub == nil {
		d.Hub = &events.Hub{}
	}
	return &Server{d: d}
}

// Serve accepts connections on ln until ctx ends. The caller creates ln so a
// port conflict is reported before anything depends on the server.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	srv := &http.Server{Handler: s, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 2 * time.Minute}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
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
		w.Header().Set("Cache-Control", cacheNoStore)
		writeJSON(w, http.StatusOK, s.Status())
	case p == "/api/local/events":
		s.d.Hub.ServeHTTP(w, r)
	case strings.HasPrefix(p, "/api/"):
		writeJSONError(w, http.StatusNotFound, "not found")
	case strings.HasPrefix(p, "/media/"):
		s.handleMedia(w, r)
	case p == updatescreen.Path:
		w.Header().Set("Cache-Control", cacheNoStore)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(updateanim.HTML)
	default:
		s.handleApp(w, r)
	}
}

// Status is the current /api/local/status object.
func (s *Server) Status() Status {
	st := BuildStatus(s.d.Layout, s.d.DeviceID, s.d.AgentVersion, s.d.Now())
	st.ServicePort, st.USBImport = s.d.ServicePort, s.d.USBImport
	if s.d.Sync != nil {
		st.Sync = s.d.Sync()
	}
	if s.d.UpdateActive != nil {
		st.Update.Active = s.d.UpdateActive()
	}
	if s.d.Updates != nil {
		info := s.d.Updates()
		st.Updates = &info
	}
	if s.d.Clock != nil {
		c := s.d.Clock()
		st.Clock = &c
	}
	return st
}

func (s *Server) handlePayload(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Cache-Control", cacheNoStore)
	c, err := s.d.Layout.CurrentContent()
	if errors.Is(err, store.ErrNoContent) {
		writeJSONError(w, http.StatusServiceUnavailable, "no content installed")
		return
	}
	if err != nil {
		s.d.Logger.Error("installed content unreadable", "err", err)
		writeJSONError(w, http.StatusInternalServerError, "installed content is unreadable")
		return
	}
	day := c.PickDay(s.d.Now())
	// day.Serving is a date we formatted ourselves, not request input.
	raw, err := os.ReadFile(c.PayloadPath(day.Serving))
	if err != nil {
		s.d.Logger.Error("payload missing from installed content", "day", day.Serving, "err", err)
		writeJSONError(w, http.StatusInternalServerError, "payload missing from installed content")
		return
	}
	if s.d.Weather != nil {
		if weather, ok := s.d.Weather(); ok {
			raw = overlayWeather(raw, weather)
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(raw)
}

// overlayWeather replaces the payload's weather with a live one. Only the one
// key changes; if the payload cannot be re-encoded it is served untouched.
func overlayWeather(raw, weather []byte) []byte {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return raw
	}
	obj["weather"] = weather
	out, err := json.Marshal(obj)
	if err != nil {
		return raw
	}
	return out
}

func (s *Server) handleMedia(w http.ResponseWriter, r *http.Request) {
	file := strings.TrimPrefix(r.URL.Path, "/media/")
	// Must look exactly like a store file before it goes near a path; that
	// also rules out "..", "/" and "\".
	if !mbu.MediaFileRE.MatchString(file) {
		http.NotFound(w, r)
		return
	}
	f, err := os.Open(s.d.Layout.MediaPath(file))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() {
		http.NotFound(w, r)
		return
	}
	ct := ""
	if c, err := s.d.Layout.CurrentContent(); err == nil {
		ct = c.Manifest.Content.MediaType(file)
	}
	if ct == "" {
		ct = contentTypeFor(file)
	}
	if ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.Header().Set("Cache-Control", cacheImmutable)
	http.ServeContent(w, r, file, fi.ModTime(), f)
}

// handleApp serves the current app release. Real files are served as they
// are; any other path is a client-side route and gets index.html. A missing
// /assets/ file is a 404 instead: it is hashed build output, and HTML under an
// immutable cache header for a stale script URL is worse than an honest miss.
func (s *Server) handleApp(w http.ResponseWriter, r *http.Request) {
	a, err := s.d.Layout.CurrentApp()
	if err != nil {
		w.Header().Set("Cache-Control", cacheNoStore)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, s.noAppPage())
		return
	}
	fsys := os.DirFS(a.Dir)
	name := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
	if name == "" {
		name = "index.html"
	}
	// .mbu/ holds the release's manifest; it is not part of the build.
	hidden := strings.HasPrefix(name, ".") || strings.Contains(name, "/.")
	if !hidden && fs.ValidPath(name) && serveFile(w, r, fsys, name) {
		return
	}
	if strings.HasPrefix(name, "assets/") {
		http.NotFound(w, r)
		return
	}
	if !serveFile(w, r, fsys, "index.html") {
		http.Error(w, "the installed board app has no index.html", http.StatusInternalServerError)
	}
}

func serveFile(w http.ResponseWriter, r *http.Request, fsys fs.FS, name string) bool {
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
	if strings.HasPrefix(name, "assets/") {
		w.Header().Set("Cache-Control", cacheImmutable)
	} else {
		w.Header().Set("Cache-Control", cacheNoCache)
	}
	if ct := contentTypeFor(name); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	http.ServeContent(w, r, path.Base(name), fi.ModTime(), rs)
	return true
}

// extraTypes covers what a Vite build emits that the minimal mime table on a
// fresh Pi OS may not know. A font sniffed as octet-stream is refused under
// nosniff.
var extraTypes = map[string]string{
	".js": "text/javascript; charset=utf-8", ".mjs": "text/javascript; charset=utf-8",
	".css": "text/css; charset=utf-8", ".html": "text/html; charset=utf-8",
	".json": "application/json", ".svg": "image/svg+xml", ".woff": "font/woff",
	".woff2": "font/woff2", ".ttf": "font/ttf", ".otf": "font/otf", ".ico": "image/x-icon",
	".webmanifest": "application/manifest+json", ".wasm": "application/wasm",
	".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".webp": "image/webp",
	".gif": "image/gif", ".avif": "image/avif",
}

func contentTypeFor(name string) string {
	ext := strings.ToLower(filepath.Ext(name))
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

// noAppPage is shown until a board app is installed. It polls, so the board
// appears on its own once an app package lands. It suggests only the offline
// routes this board accepts.
func (s *Server) noAppPage() string {
	var routes []string
	if s.d.USBImport {
		routes = append(routes, "plug in a USB stick holding a MusallahBoard update")
	}
	if s.d.ServicePort {
		routes = append(routes, "connect a laptop or phone to the board's ethernet port and open http://10.77.0.1/")
	}
	offline := "Without internet, it needs to be set up for USB sticks or its ethernet service port first."
	if len(routes) > 0 {
		offline = "Without internet, " + strings.Join(routes, ", or ") + "."
	}
	return strings.Replace(noAppHTML, "{{offline}}", html.EscapeString(offline), 1)
}

const noAppHTML = `<!doctype html><html lang="en"><head><meta charset="utf-8">
<title>MusallahBoard</title>
<style>html,body{height:100%;margin:0;background:#082D5D;color:#fff;font-family:system-ui,sans-serif}
main{height:100%;display:grid;place-items:center;text-align:center;padding:6vmin;box-sizing:border-box}
h1{font-weight:300;font-size:5vmin;margin:0 0 2.5vmin}p{font-size:2.6vmin;line-height:1.45;opacity:.8;max-width:60ch;margin:0 auto}</style>
</head><body><main><div><h1>Waiting for the board app</h1>
<p>This board has no board app installed yet. Online boards download it automatically within a few minutes.
{{offline}}</p></div></main>
<script>setInterval(function(){fetch('/api/local/status',{cache:'no-store'}).then(function(r){return r.json()})
.then(function(s){if(s&&s.app)location.reload()}).catch(function(){})},5000)</script>
</body></html>`
