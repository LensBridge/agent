// Package boardsync keeps an online board's local state fresh from the
// network (docs/architecture.md, sections 7 and 9): content from the
// LensBridge backend, the live weather overlay, and new board app and agent
// releases from the release channels.
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
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/LensBridge/agent/internal/config"
	"github.com/LensBridge/agent/internal/importer"
	"github.com/LensBridge/agent/internal/store"
)

// Timings from the contract. They are fields on the Syncer (set from these in
// New) so tests can shrink them.
const (
	ContentInterval   = 30 * time.Minute
	TriggerDelay      = 10 * time.Second
	ContentMinBackoff = time.Minute
	ContentMaxBackoff = 30 * time.Minute
	WeatherInterval   = 30 * time.Minute
	WeatherMaxAge     = 3 * time.Hour
	ChannelFirstDelay = 2 * time.Minute
	ChannelInterval   = 6 * time.Hour
	RefreshMinBackoff = time.Second
	RefreshMaxBackoff = 5 * time.Minute

	// MaxHaveMedia caps the haveMedia list. A store bigger than this only
	// costs a larger download; the backend includes whatever is not listed.
	MaxHaveMedia = 2000
	// maxErrorBody bounds how much of an error response is read for its
	// message.
	maxErrorBody = 4 << 10
	// maxChannelBytes bounds a channel pointer file.
	maxChannelBytes = 1 << 20
	// maxWeatherPayload bounds the live payload the weather is taken from.
	maxWeatherPayload = 16 << 20
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
	weatherInterval, channelFirstDelay, channelInterval   time.Duration
	refreshMinBackoff, refreshMaxBackoff                  time.Duration

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
		client = NewHTTPClient()
	}
	return &Syncer{
		d:                 d,
		log:               log.With("component", "sync"),
		client:            client,
		trigger:           make(chan struct{}, 1),
		now:               time.Now,
		contentInterval:   ContentInterval,
		triggerDelay:      TriggerDelay,
		minBackoff:        ContentMinBackoff,
		maxBackoff:        ContentMaxBackoff,
		weatherInterval:   WeatherInterval,
		channelFirstDelay: ChannelFirstDelay,
		channelInterval:   ChannelInterval,
		refreshMinBackoff: RefreshMinBackoff,
		refreshMaxBackoff: RefreshMaxBackoff,
	}
}

// NewHTTPClient is the client every sync request uses: a bounded connect so
// a board with no route gives up fast, and an overall limit long enough for
// a large package on a slow link.
func NewHTTPClient() *http.Client {
	return &http.Client{
		Timeout: 10 * time.Minute,
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           (&net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			TLSHandshakeTimeout:   15 * time.Second,
			ResponseHeaderTimeout: 2 * time.Minute,
			IdleConnTimeout:       90 * time.Second,
			MaxIdleConns:          4,
			ForceAttemptHTTP2:     true,
		},
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
		run(s.refreshLoop)
		run(s.weatherLoop)
	} else {
		s.log.Info("content sync is off (content_sync = false); the board only takes content from USB, uploads and the CLI")
	}
	if s.d.Cfg.AutoUpdate() {
		run(s.channelLoop)
	} else {
		s.log.Info("release channels are off (auto_update = false)")
	}
	wg.Wait()
}

// Trigger asks for a content sync in TriggerDelay. Calls within that window
// fold into one sync: the backend's refresh channel fires once per admin
// edit, and an admin editing five posters should cost one download.
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

// Weather returns the latest weather object from the live payload, if it was
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
			// refresh messages must not postpone the sync forever.
			if until := deadline.Sub(s.now()); until > s.triggerDelay {
				deadline = s.now().Add(s.triggerDelay)
				resetTimer(timer, s.triggerDelay)
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
	signRequest(req, body, s.d.Cfg.DeviceID, s.d.Key, s.now())
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
	if _, err := saveCapped(file, resp.Body, maxPackage, nil); err != nil {
		if errors.Is(err, errTooLarge) {
			return err
		}
		return &syncError{short: "download from the backend was interrupted", detail: err}
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

// FetchWeather fetches the live payload and keeps its weather object. A
// payload whose weather is not an object (null: the backend has none) clears
// the overlay; a failed fetch keeps the old one until it ages out.
func (s *Syncer) FetchWeather(ctx context.Context) error {
	u := s.backend("/api/musallah/payload") + "?deviceId=" + urlQueryEscape(s.d.Cfg.DeviceID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := s.do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if err := responseError(resp); err != nil {
		return err
	}
	var payload map[string]json.RawMessage
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxWeatherPayload)).Decode(&payload); err != nil {
		return fmt.Errorf("live payload is not a JSON object: %w", err)
	}
	w := payload["weather"]
	s.mu.Lock()
	defer s.mu.Unlock()
	if isJSONObject(w) {
		s.weather = append(json.RawMessage(nil), w...)
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
