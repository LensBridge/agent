// Package uploadserver is the service-port upload server (docs/architecture.md,
// section 9.5): a laptop or phone plugged into the board's ethernet port opens
// http://10.77.0.1/ in a browser, or runs mbpush or the Android app, and
// uploads .mbu packages.
//
// There is no authentication, by design. Every package is signed, content is
// bound to this board and nothing installs backwards, so the most an
// anonymous uploader can do is make the board check a file and refuse it.
// Sizes and one-import-at-a-time bound that. What the server does guard
// against is a web page on the uploader's laptop reaching it through DNS
// rebinding: requests must name the board's own address in Host.
package uploadserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"mime/multipart"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"time"

	"github.com/LensBridge/agent/internal/fsutil"
	"github.com/LensBridge/agent/internal/importer"
	"github.com/LensBridge/agent/internal/localserver"
	"github.com/LensBridge/agent/internal/mbu"
	"github.com/LensBridge/agent/internal/notice"
	"github.com/LensBridge/agent/internal/store"
)

// ServiceIP is the board's address on the service port; setup.sh and the
// service-port CLI create it.
const ServiceIP = "10.77.0.1"

// ListenAddr is where the server listens.
const ListenAddr = ServiceIP + ":80"

const (
	maxParts      = 8
	maxTotalBytes = 1 << 30
)

// ClockReport is what the clock keeper says about an uploader's time.
type ClockReport struct {
	DriftSeconds int64  `json:"driftSeconds"`
	Adjusted     bool   `json:"adjusted"`
	Note         string `json:"note"`
}

// Deps is what the server needs.
type Deps struct {
	Layout       store.Layout
	DeviceID     string
	AgentVersion string
	Importer     *importer.Importer
	// ApplyClientTime is the clock keeper's decision on X-MB-Client-Time.
	// May be nil.
	ApplyClientTime func(clientUnix int64, b importer.Batch) ClockReport
	RTCPresent      func() bool
	UpdateActive    func() bool
	Logger          *slog.Logger
	Now             func() time.Time
}

// Server is the upload server.
type Server struct{ d Deps }

// New returns a Server.
func New(d Deps) *Server {
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.Logger == nil {
		d.Logger = slog.New(slog.DiscardHandler)
	}
	return &Server{d: d}
}

// Serve accepts connections on ln until ctx ends.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	srv := &http.Server{
		Handler:           s,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
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

var allowedHosts = map[string]bool{
	ServiceIP: true, ServiceIP + ":80": true, "musallahboard.local": true, "musallahboard.local:80": true,
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
	if !allowedHosts[r.Host] {
		writeJSON(w, http.StatusMisdirectedRequest, map[string]string{
			"message": "open this page as http://" + ServiceIP + "/",
		})
		return
	}
	switch {
	case r.URL.Path == "/" && (r.Method == http.MethodGet || r.Method == http.MethodHead):
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; connect-src 'self'; form-action 'none'")
		_, _ = io.WriteString(w, uploadPage)
	case r.URL.Path == "/api/status" && r.Method == http.MethodGet:
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, http.StatusOK, s.status())
	case r.URL.Path == "/api/import" && r.Method == http.MethodPost:
		s.handleImport(w, r)
	default:
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "not found"})
	}
}

// Status is GET /api/status.
type Status struct {
	DeviceID      string                   `json:"deviceId"`
	AgentVersion  string                   `json:"agentVersion"`
	App           *localserver.AppInfo     `json:"app"`
	Content       *localserver.ContentInfo `json:"content"`
	Today         string                   `json:"today"`
	DaysRemaining *int                     `json:"daysRemaining"`
	StaleDays     *int                     `json:"staleDays"`
	Clock         struct {
		Unix     int64  `json:"unix"`
		Timezone string `json:"timezone"`
	} `json:"clock"`
	RTC    bool                   `json:"rtc"`
	Update localserver.UpdateInfo `json:"update"`
}

func (s *Server) status() Status {
	now := s.d.Now()
	ls := localserver.BuildStatus(s.d.Layout, s.d.DeviceID, s.d.AgentVersion, now)
	st := Status{DeviceID: ls.DeviceID, AgentVersion: ls.AgentVersion, App: ls.App, Content: ls.Content,
		Today: ls.Today, DaysRemaining: ls.DaysRemaining, StaleDays: ls.StaleDays}
	st.Clock.Unix = now.Unix()
	st.Clock.Timezone, _ = now.Zone()
	if s.d.RTCPresent != nil {
		st.RTC = s.d.RTCPresent()
	}
	if s.d.UpdateActive != nil {
		st.Update.Active = s.d.UpdateActive()
	}
	return st
}

// ImportResponse is POST /api/import's body.
type ImportResponse struct {
	Results []importer.Result `json:"results"`
	// Notice is what the board showed about the upload.
	Notice  *notice.Notice `json:"notice,omitempty"`
	Clock   *ClockReport   `json:"clock,omitempty"`
	Message string         `json:"message,omitempty"`
}

var safeNameRE = regexp.MustCompile(`[^A-Za-z0-9._-]`)

func (s *Server) handleImport(w http.ResponseWriter, r *http.Request) {
	// The uploader stamps its clock when it starts sending; receiving and
	// installing can take minutes. Measure the offset now and apply it to the
	// board's clock at the end, rather than the stale absolute time.
	received := s.d.Now()
	mt, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mt != "multipart/form-data" || params["boundary"] == "" {
		writeJSON(w, http.StatusBadRequest, ImportResponse{Message: "send the packages as multipart/form-data, one 'package' part per file"})
		return
	}
	// Refuse early rather than receive a gigabyte and then say no.
	if !s.idle() {
		writeJSON(w, http.StatusConflict, ImportResponse{Message: "the board is already installing an update; try again in a minute"})
		return
	}
	inbox := s.d.Layout.Inbox()
	if err := os.MkdirAll(inbox, 0o770); err != nil {
		writeJSON(w, http.StatusInternalServerError, ImportResponse{Message: err.Error()})
		return
	}
	// A hidden directory, so the inbox runner does not pick the files up
	// while they are still arriving.
	dir, err := os.MkdirTemp(inbox, ".upload-")
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, ImportResponse{Message: err.Error()})
		return
	}
	defer os.RemoveAll(dir)

	r.Body = http.MaxBytesReader(w, r.Body, maxTotalBytes+(1<<20))
	mr := multipart.NewReader(r.Body, params["boundary"])
	var files []string
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			writeJSON(w, http.StatusBadRequest, ImportResponse{Message: "the upload was cut off or malformed: " + err.Error()})
			return
		}
		if part.FormName() != "package" {
			part.Close()
			continue
		}
		if len(files) == maxParts {
			writeJSON(w, http.StatusRequestEntityTooLarge, ImportResponse{Message: fmt.Sprintf("at most %d packages per upload", maxParts)})
			return
		}
		name := safeNameRE.ReplaceAllString(filepath.Base(part.FileName()), "_")
		if name == "" || name == "." || name[0] == '.' {
			name = "package.mbu"
		}
		dst := filepath.Join(dir, fsutil.UniqueName(dir, name))
		n, err := fsutil.CopyFileSync(dst, part, mbu.MaxPackageBytes+1, 0o640)
		part.Close()
		if err != nil {
			writeJSON(w, http.StatusBadRequest, ImportResponse{Message: "receiving " + name + " failed: " + err.Error()})
			return
		}
		if n > mbu.MaxPackageBytes {
			writeJSON(w, http.StatusRequestEntityTooLarge, ImportResponse{Message: name + " is larger than the 512 MiB limit"})
			return
		}
		files = append(files, dst)
	}
	if len(files) == 0 {
		writeJSON(w, http.StatusBadRequest, ImportResponse{Message: "no 'package' part in the upload"})
		return
	}

	// The import outlives a client that gives up waiting: an install that
	// has started should finish.
	b, err := s.d.Importer.TryImport(context.WithoutCancel(r.Context()), importer.SourceUpload, files)
	if errors.Is(err, importer.ErrBusy) {
		writeJSON(w, http.StatusConflict, ImportResponse{Message: "the board is already installing an update; try again in a minute"})
		return
	}
	resp := ImportResponse{Results: b.Results, Notice: &b.Notice}
	if v := r.Header.Get("X-MB-Client-Time"); v != "" && s.d.ApplyClientTime != nil {
		if unix, err := strconv.ParseInt(v, 10, 64); err == nil {
			unix += int64(s.d.Now().Sub(received) / time.Second)
			rep := s.d.ApplyClientTime(unix, b)
			resp.Clock = &rep
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// idle is a cheap pre-check; TryImport is the authoritative one.
func (s *Server) idle() bool { return !s.d.Importer.Busy() }

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
