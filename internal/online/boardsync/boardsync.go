// Package boardsync keeps an enrolled board's content fresh from the
// LensBridge backend (docs/architecture.md, sections 7 and 9): the content
// package and the live weather overlay. (New board app and agent releases
// come from the release channels, package updates, which need no backend.)
//
// Nothing here is trusted. Everything that changes the board arrives as a
// signed package and goes through the same importer as a USB stick or an
// upload; the network only decides how fresh the board is. A board with no
// network simply fails these fetches quietly and keeps serving what it has.
package boardsync

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/LensBridge/agent/internal/core/config"
	"github.com/LensBridge/agent/internal/core/fsutil"
	"github.com/LensBridge/agent/internal/core/importer"
	"github.com/LensBridge/agent/internal/core/mbu"
	"github.com/LensBridge/agent/internal/core/netinfo"
	"github.com/LensBridge/agent/internal/core/store"
	"github.com/LensBridge/agent/internal/online/devauth"
)

// Timings from the contract. They are fields on the Syncer (set from these in
// New) so tests can shrink them.
const (
	ContentInterval = 30 * time.Minute
	// TriggerDelay and TriggerJitter: a requested sync (the backend's
	// content_changed frame, config.refresh) runs TriggerDelay plus up to
	// TriggerJitter later, so a fleet told at once does not sync at once.
	TriggerDelay      = 10 * time.Second
	TriggerJitter     = 20 * time.Second
	ContentMinBackoff = time.Minute
	ContentMaxBackoff = 30 * time.Minute
	WeatherInterval   = 30 * time.Minute
	WeatherMaxAge     = 3 * time.Hour

	// MaxHaveMedia caps the haveMedia list. A store bigger than this only
	// costs a larger download; the backend includes whatever is not listed.
	MaxHaveMedia = 2000
	// maxErrorBody bounds how much of an error response is read for its
	// message.
	maxErrorBody = 4 << 10
	// maxWeatherResponse bounds GET /api/agent/weather's answer.
	maxWeatherResponse = 1 << 20
)

// Deps is what a Syncer needs.
type Deps struct {
	Cfg          *config.Config
	Key          ed25519.PrivateKey
	Layout       store.Layout
	Importer     *importer.Importer
	AgentVersion string
	Logger       *slog.Logger
	// HTTP is optional; New builds one with sane timeouts.
	HTTP *http.Client
}

// Status is the "sync" object of /api/local/status.
type Status struct {
	Enabled       bool       `json:"enabled"`
	LastAttemptAt *time.Time `json:"lastAttemptAt"`
	LastSuccessAt *time.Time `json:"lastSuccessAt"`
	LastError     *string    `json:"lastError"`
}

// Syncer runs the background sync loops.
type Syncer struct {
	d       Deps
	log     *slog.Logger
	client  *http.Client
	trigger chan struct{}
	now     func() time.Time

	contentInterval, triggerDelay, minBackoff, maxBackoff time.Duration
	triggerJitter                                         time.Duration
	weatherInterval                                       time.Duration

	mu        sync.Mutex
	status    Status
	weather   json.RawMessage
	weatherAt time.Time
}

// New returns a Syncer. Call Run to start it.
func New(d Deps) *Syncer {
	log := d.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	client := d.HTTP
	if client == nil {
		client = netinfo.NewHTTPClient()
	}
	return &Syncer{
		d:               d,
		log:             log.With("component", "sync"),
		client:          client,
		trigger:         make(chan struct{}, 1),
		now:             time.Now,
		contentInterval: ContentInterval,
		triggerDelay:    TriggerDelay,
		triggerJitter:   TriggerJitter,
		minBackoff:      ContentMinBackoff,
		maxBackoff:      ContentMaxBackoff,
		weatherInterval: WeatherInterval,
	}
}

// Run runs every enabled loop until ctx ends.
func (s *Syncer) Run(ctx context.Context) {
	s.cleanStaleDownloads()
	var wg sync.WaitGroup
	run := func(f func(context.Context)) {
		wg.Add(1)
		go func() { defer wg.Done(); f(ctx) }()
	}
	if s.d.Cfg.ContentSync() {
		run(s.contentLoop)
		run(s.weatherLoop)
	} else {
		s.log.Info("content sync is off (content_sync = false); the board only takes content from USB, uploads and the CLI")
	}
	wg.Wait()
}

// Trigger asks for a content sync in TriggerDelay plus up to TriggerJitter.
// Calls before it runs fold into one sync: an admin editing five posters
// should cost one download.
func (s *Syncer) Trigger() {
	select {
	case s.trigger <- struct{}{}:
	default:
	}
}

// Status reports the content sync state.
func (s *Syncer) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.status
	st.Enabled = s.d.Cfg.ContentSync()
	return st
}

// Weather returns the latest weather object from the backend, if it was
// fetched less than WeatherMaxAge ago.
func (s *Syncer) Weather() (json.RawMessage, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.weather == nil || s.now().Sub(s.weatherAt) >= WeatherMaxAge {
		return nil, false
	}
	return s.weather, true
}

// ── content ──────────────────────────────────────────────────────────────────

func (s *Syncer) contentLoop(ctx context.Context) {
	timer := time.NewTimer(0) // sync at start
	defer timer.Stop()
	deadline := s.now()
	backoff := s.minBackoff
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.trigger:
			// Bring the next sync forward to at most triggerDelay away, but
			// never push an already-closer one back: a steady stream of
			// triggers must not postpone the sync forever.
			d := s.triggerDelay
			if s.triggerJitter > 0 {
				d += time.Duration(rand.Int63n(int64(s.triggerJitter)))
			}
			if until := deadline.Sub(s.now()); until > d {
				deadline = s.now().Add(d)
				resetTimer(timer, d)
			}
			continue
		case <-timer.C:
		}

		wait := s.contentInterval
		if err := s.SyncContent(ctx); err != nil {
			if ctx.Err() != nil {
				return
			}
			wait = backoff
			backoff = min(backoff*2, s.maxBackoff)
		} else {
			backoff = s.minBackoff
		}
		deadline = s.now().Add(wait)
		resetTimer(timer, wait)
	}
}

func resetTimer(t *time.Timer, d time.Duration) {
	if !t.Stop() {
		select {
		case <-t.C:
		default:
		}
	}
	t.Reset(d)
}

// SyncContent fetches a content package from the backend and imports it,
// recording the outcome in Status. It is exported for the daemon's tests and
// for a future "sync now" command; the loop calls it on schedule.
func (s *Syncer) SyncContent(ctx context.Context) error {
	start := s.now()
	s.mu.Lock()
	s.status.LastAttemptAt = &start
	s.mu.Unlock()

	err := s.syncContent(ctx)

	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		msg := err.Error()
		s.status.LastError = &msg
		s.log.Warn("content sync failed", "err", msg)
		return err
	}
	done := s.now()
	s.status.LastSuccessAt = &done
	s.status.LastError = nil
	return nil
}

// syncError is a failure with a short message for the status page and a
// longer one for the journal.
type syncError struct {
	short  string
	detail error
}

func (e *syncError) Error() string { return e.short }
func (e *syncError) Unwrap() error { return e.detail }

func (s *Syncer) syncContent(ctx context.Context) error {
	have := s.d.Layout.MediaHashes()
	if len(have) > MaxHaveMedia {
		have = have[:MaxHaveMedia]
	}
	if have == nil {
		have = []string{}
	}
	body, err := json.Marshal(struct {
		Days      int      `json:"days"`
		HaveMedia []string `json:"haveMedia"`
	}{s.d.Cfg.ContentDays(), have})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.backend("/api/agent/content-bundle"), strings.NewReader(string(body)))
	if err != nil {
		return fmt.Errorf("bad backend_url in the config: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/vnd.musallahboard.mbu")
	devauth.Sign(req, body, s.d.Cfg.DeviceID, s.d.Key, s.now())
	resp, err := s.do(req)
	if err != nil {
		s.log.Debug("content sync request failed", "err", err)
		return &syncError{short: "backend unreachable", detail: err}
	}
	defer resp.Body.Close()
	if err := responseError(resp); err != nil {
		return err
	}

	dir, err := s.downloadDir()
	if err != nil {
		return fmt.Errorf("cannot prepare the download: %w", err)
	}
	defer os.RemoveAll(dir)
	file := filepath.Join(dir, "content.mbu")
	// One byte past the limit tells "exactly at the limit" from "too big".
	n, err := fsutil.CopyFileSync(file, resp.Body, mbu.MaxPackageBytes+1, 0o640)
	if err != nil {
		return &syncError{short: "download from the backend was interrupted", detail: err}
	}
	if n > mbu.MaxPackageBytes {
		return fmt.Errorf("the backend's package is larger than %d MiB, the most a package may be", mbu.MaxPackageBytes>>20)
	}

	b := s.d.Importer.Import(ctx, importer.SourceSync, []string{file})
	return batchError(b)
}

// batchError turns a sync batch's results into a status error, or nil when
// the package installed or was already installed.
func batchError(b importer.Batch) error {
	for _, r := range b.Results {
		switch r.Action {
		case importer.ActionRejected:
			return fmt.Errorf("the package from the backend was refused: %s", r.Message)
		case importer.ActionSkipped:
			return fmt.Errorf("the backend sent a package for another board: %s", r.Message)
		}
	}
	if len(b.Results) == 0 {
		return fmt.Errorf("the backend's package produced no result")
	}
	return nil
}

// responseError maps a non-200 backend response to a short message. 401 gets
// its own wording because the two usual causes need a person: the device was
// revoked, or its clock is so far off the signature timestamp is refused.
func responseError(resp *http.Response) error {
	if resp.StatusCode == http.StatusOK {
		return nil
	}
	msg := errorMessage(resp)
	if resp.StatusCode == http.StatusUnauthorized {
		s := "backend refused this board (401): the device may have been revoked, or the board's clock is wrong"
		if msg != "" {
			s += " (" + msg + ")"
		}
		return errors.New(s)
	}
	if msg == "" {
		return fmt.Errorf("backend returned %d", resp.StatusCode)
	}
	return fmt.Errorf("backend returned %d: %s", resp.StatusCode, msg)
}

// errorMessage reads {"message": "..."} from an error body, or a short plain
// text body, and "" otherwise.
func errorMessage(resp *http.Response) string {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	var m struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(raw, &m) == nil && m.Message != "" {
		return oneLine(m.Message, 200)
	}
	if ct := resp.Header.Get("Content-Type"); strings.HasPrefix(ct, "text/plain") {
		return oneLine(string(raw), 200)
	}
	return ""
}

func oneLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > max {
		s = s[:max] + "..."
	}
	return s
}

// backend joins path onto the configured backend URL.
func (s *Syncer) backend(path string) string {
	return strings.TrimRight(s.d.Cfg.BackendURL, "/") + path
}

func (s *Syncer) do(req *http.Request) (*http.Response, error) {
	req.Header.Set("User-Agent", "musallahboard-agent/"+s.d.AgentVersion)
	return s.client.Do(req)
}

// downloadDir makes a fresh hidden directory in the inbox for one download.
// Hidden entries are ignored by the inbox runner, so a half-written package
// is never picked up; it is on the same filesystem as the inbox, and it is
// removed after the import.
func (s *Syncer) downloadDir() (string, error) {
	inbox := s.d.Layout.Inbox()
	if err := os.MkdirAll(inbox, 0o770); err != nil {
		return "", err
	}
	return os.MkdirTemp(inbox, ".sync-")
}

// cleanStaleDownloads removes .sync-* directories a previous run left behind
// when it was killed mid-download.
func (s *Syncer) cleanStaleDownloads() {
	matches, _ := filepath.Glob(filepath.Join(s.d.Layout.Inbox(), ".sync-*"))
	for _, m := range matches {
		os.RemoveAll(m)
	}
}

// ── weather ──────────────────────────────────────────────────────────────────

func (s *Syncer) weatherLoop(ctx context.Context) {
	for {
		if err := s.FetchWeather(ctx); err != nil && ctx.Err() == nil {
			s.log.Debug("weather fetch failed", "err", err)
		}
		if !sleep(ctx, s.weatherInterval) {
			return
		}
	}
}

// FetchWeather asks the backend for the current weather (GET
// /api/agent/weather, device-authenticated) and keeps it. A null weather (the
// backend has none) clears the overlay; a failed fetch keeps the old one until
// it ages out.
func (s *Syncer) FetchWeather(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.backend("/api/agent/weather"), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	devauth.Sign(req, nil, s.d.Cfg.DeviceID, s.d.Key, s.now())
	resp, err := s.do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if err := responseError(resp); err != nil {
		return err
	}
	var body struct {
		Weather json.RawMessage `json:"weather"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxWeatherResponse)).Decode(&body); err != nil {
		return fmt.Errorf("weather response is not JSON: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if isJSONObject(body.Weather) {
		s.weather = append(json.RawMessage(nil), body.Weather...)
		s.weatherAt = s.now()
	} else {
		s.weather = nil
	}
	return nil
}

func isJSONObject(raw json.RawMessage) bool {
	var m map[string]json.RawMessage
	return len(raw) > 0 && json.Unmarshal(raw, &m) == nil && m != nil
}

// ── shared ───────────────────────────────────────────────────────────────────

func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
