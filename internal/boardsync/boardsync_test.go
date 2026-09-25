package boardsync

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/LensBridge/agent/internal/config"
	"github.com/LensBridge/agent/internal/devauth"
	"github.com/LensBridge/agent/internal/importer"
	"github.com/LensBridge/agent/internal/mbu"
	"github.com/LensBridge/agent/internal/state"
	"github.com/LensBridge/agent/internal/store"
	"github.com/LensBridge/agent/internal/trust"
	"github.com/LensBridge/agent/internal/updates"
)

const testDevice = "3f2a1b4c-5d6e-4f70-8a9b-0c1d2e3f4a5b"

func seedKey(b byte) ed25519.PrivateKey {
	return ed25519.NewKeyFromSeed(bytes.Repeat([]byte{b}, 32))
}

var (
	contentKey = seedKey(1)
	releaseKey = seedKey(2)
	deviceKey  = seedKey(3)
)

type env struct {
	layout  store.Layout
	cfg     *config.Config
	syncer  *Syncer
	updates *updates.Scheduler
}

func strp(s string) *string { return &s }
func boolp(b bool) *bool    { return &b }

// newEnv builds a Syncer over a temp store with a real importer that trusts
// contentKey and releaseKey. Channels are disabled unless a test sets them.
func newEnv(t *testing.T, backend string) *env {
	t.Helper()
	l := store.Layout{Root: t.TempDir()}
	cfg := &config.Config{
		DeviceID:        testDevice,
		BackendURL:      backend,
		WebSocketURL:    "ws://unused/api/agent/ws",
		AppChannelSet:   strp(""),
		AgentChannelSet: strp(""),
	}
	ring := trust.NewRing(
		[]ed25519.PublicKey{contentKey.Public().(ed25519.PublicKey)},
		[]ed25519.PublicKey{releaseKey.Public().(ed25519.PublicKey)},
	)
	im := importer.New(importer.Deps{
		Layout:       l,
		DeviceID:     testDevice,
		AgentVersion: "0.3.0",
		Ring:         func() (*trust.Ring, error) { return ring, nil },
	})
	u := updates.New(updates.Deps{Layout: l, Importer: im, Hour: 23})
	s := New(Deps{Cfg: cfg, Key: deviceKey, Layout: l, Importer: im, Updates: u, AgentVersion: "0.3.0"})
	return &env{layout: l, cfg: cfg, syncer: s, updates: u}
}

func build(t *testing.T, m mbu.Manifest, src []mbu.Source, key ed25519.PrivateKey) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := mbu.Build(&buf, m, src, []ed25519.PrivateKey{key}); err != nil {
		t.Fatalf("Build: %v", err)
	}
	return buf.Bytes()
}

func contentPackage(t *testing.T, seq int64) []byte {
	m := mbu.Manifest{
		Type: mbu.TypeContent, Sequence: seq, DeviceID: testDevice,
		Content: &mbu.ContentInfo{Timezone: "America/Toronto", FirstDay: "2026-09-24", LastDay: "2026-09-25"},
	}
	src := []mbu.Source{
		{Path: "payloads/2026-09-24.json", Data: []byte(`{"frames":[],"weather":null}`)},
		{Path: "payloads/2026-09-25.json", Data: []byte(`{"frames":[],"weather":null}`)},
	}
	return build(t, m, src, contentKey)
}

func appPackage(t *testing.T, version string) []byte {
	m := mbu.Manifest{Type: mbu.TypeApp, Version: version, App: &mbu.AppInfo{LocalAPI: 2}}
	return build(t, m, []mbu.Source{{Path: "index.html", Data: []byte("<html>" + version)}}, releaseKey)
}

func agentPackage(t *testing.T, version string) []byte {
	m := mbu.Manifest{Type: mbu.TypeAgent, Version: version,
		Agent: &mbu.AgentInfo{Arch: runtime.GOARCH, Binary: "musallahboard-agent"}}
	return build(t, m, []mbu.Source{{Path: "musallahboard-agent", Data: []byte("#!/bin/sh\necho " + version)}}, releaseKey)
}

func TestSyncContentSignedAndImported(t *testing.T) {
	pkg := contentPackage(t, 1000)
	var gotBody struct {
		Days      int      `json:"days"`
		HaveMedia []string `json:"haveMedia"`
	}
	haveSHA := strings.Repeat("ab", 32)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/agent/content-bundle" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &gotBody); err != nil {
			t.Errorf("body: %v", err)
		}
		if r.Header.Get(devauth.HeaderDeviceID) != testDevice {
			t.Errorf("device id header = %q", r.Header.Get(devauth.HeaderDeviceID))
		}
		ts, err := strconv.ParseInt(r.Header.Get(devauth.HeaderTimestamp), 10, 64)
		if err != nil || time.Since(time.UnixMilli(ts)).Abs() > time.Minute {
			t.Errorf("timestamp header = %q", r.Header.Get(devauth.HeaderTimestamp))
		}
		sig, _ := base64.StdEncoding.DecodeString(r.Header.Get(devauth.HeaderSignature))
		msg := devauth.Message(r.Method, r.URL.Path, testDevice, ts, body)
		if !ed25519.Verify(deviceKey.Public().(ed25519.PublicKey), msg, sig) {
			t.Error("signature does not verify over the received body")
		}
		if !strings.HasSuffix(string(msg), hexSHA(body)) {
			t.Error("message does not end in the body hash")
		}
		w.Header().Set("Content-Type", "application/vnd.musallahboard.mbu")
		w.Write(pkg)
	}))
	defer srv.Close()

	e := newEnv(t, srv.URL+"/")
	// A media file already in the store must be offered as haveMedia.
	os.MkdirAll(e.layout.MediaDir(), 0o750)
	os.WriteFile(filepath.Join(e.layout.MediaDir(), haveSHA+".png"), []byte("x"), 0o640)

	if err := e.syncer.SyncContent(context.Background()); err != nil {
		t.Fatalf("SyncContent: %v", err)
	}
	if gotBody.Days != 7 {
		t.Errorf("days = %d, want 7", gotBody.Days)
	}
	if len(gotBody.HaveMedia) != 1 || gotBody.HaveMedia[0] != haveSHA {
		t.Errorf("haveMedia = %v", gotBody.HaveMedia)
	}
	c, err := e.layout.CurrentContent()
	if err != nil || c.Manifest.Sequence != 1000 || c.Info.Source != "sync" {
		t.Fatalf("installed content = %+v, %v", c, err)
	}
	st := e.syncer.Status()
	if !st.Enabled || st.LastSuccessAt == nil || st.LastAttemptAt == nil || st.LastError != nil {
		t.Errorf("status = %+v", st)
	}
	if left, _ := filepath.Glob(filepath.Join(e.layout.Inbox(), ".sync-*")); len(left) != 0 {
		t.Errorf("download dirs left behind: %v", left)
	}
	// The same package again is "unchanged", which is still a success.
	if err := e.syncer.SyncContent(context.Background()); err != nil {
		t.Fatalf("second SyncContent: %v", err)
	}
}

func hexSHA(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func TestSyncContentErrors(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   string
	}{
		{503, `{"message":"no content signing key configured"}`, "backend returned 503: no content signing key configured"},
		{401, `{"message":"unknown device"}`, "revoked, or the board's clock is wrong"},
		{500, `<html>oops</html>`, "backend returned 500"},
	}
	for _, tc := range cases {
		t.Run(strconv.Itoa(tc.status), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				io.WriteString(w, tc.body)
			}))
			defer srv.Close()
			e := newEnv(t, srv.URL)
			err := e.syncer.SyncContent(context.Background())
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
			st := e.syncer.Status()
			if st.LastError == nil || *st.LastError != err.Error() || st.LastSuccessAt != nil {
				t.Errorf("status = %+v", st)
			}
		})
	}
}

func TestSyncContentUnreachable(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	e := newEnv(t, url)
	err := e.syncer.SyncContent(context.Background())
	if err == nil || err.Error() != "backend unreachable" {
		t.Fatalf("err = %v", err)
	}
}

func TestSyncContentRejectedPackage(t *testing.T) {
	// Signed by the release key: authentic to nobody for content.
	m := mbu.Manifest{Type: mbu.TypeContent, Sequence: 5, DeviceID: testDevice,
		Content: &mbu.ContentInfo{Timezone: "UTC", FirstDay: "2026-09-24", LastDay: "2026-09-24"}}
	pkg := build(t, m, []mbu.Source{{Path: "payloads/2026-09-24.json", Data: []byte(`{}`)}}, releaseKey)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(pkg) }))
	defer srv.Close()
	e := newEnv(t, srv.URL)
	err := e.syncer.SyncContent(context.Background())
	if err == nil || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("err = %v", err)
	}
}

func TestContentBackoff(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/agent/content-bundle" {
			hits.Add(1)
		}
		w.WriteHeader(503)
	}))
	defer srv.Close()
	e := newEnv(t, srv.URL)
	s := e.syncer
	s.minBackoff, s.maxBackoff = 20*time.Millisecond, 80*time.Millisecond
	s.contentInterval = time.Hour
	ctx, cancel := context.WithTimeout(context.Background(), 330*time.Millisecond)
	defer cancel()
	s.contentLoop(ctx)
	// Attempts at about 0, 20, 60, 140, 220, 300 ms: doubling, then capped.
	if n := hits.Load(); n < 4 || n > 8 {
		t.Fatalf("%d attempts in 330 ms, want about 6", n)
	}
}

func TestRefreshTriggersDebouncedSync(t *testing.T) {
	pkg := contentPackage(t, 1000)
	var hits atomic.Int32
	var mu sync.Mutex
	var sockets []*websocket.Conn
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/agent/content-bundle":
			hits.Add(1)
			w.Write(pkg)
		case "/api/refresh-musallahboard":
			if r.URL.Query().Get("deviceId") != testDevice {
				t.Errorf("refresh deviceId = %q", r.URL.Query().Get("deviceId"))
			}
			c, err := websocket.Accept(w, r, nil)
			if err != nil {
				return
			}
			mu.Lock()
			sockets = append(sockets, c)
			mu.Unlock()
			// Three messages in quick succession fold into one sync.
			for range 3 {
				c.Write(r.Context(), websocket.MessageText, []byte("refresh"))
			}
			c.Read(r.Context()) // hold open until the client goes
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	e := newEnv(t, srv.URL)
	e.syncer.triggerDelay = 50 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { e.syncer.Run(ctx); close(done) }()
	// Wait for the second sync rather than giving the whole exchange a fixed
	// budget: under -race on a busy CI runner, start-up sync, WebSocket dial
	// and debounce together can take longer than any small constant.
	deadline := time.Now().Add(10 * time.Second)
	for hits.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	// Then long enough for any extra, unfolded sync to show up.
	time.Sleep(10 * e.syncer.triggerDelay)
	cancel()
	<-done
	mu.Lock()
	for _, c := range sockets {
		c.CloseNow()
	}
	mu.Unlock()
	// One at start, one for the burst of refresh messages.
	if n := hits.Load(); n != 2 {
		t.Fatalf("%d content syncs, want 2", n)
	}
}

func TestRefreshURL(t *testing.T) {
	for in, want := range map[string]string{
		"https://api.example":       "wss://api.example/api/refresh-musallahboard?deviceId=" + testDevice,
		"http://10.0.0.2:8080/":     "ws://10.0.0.2:8080/api/refresh-musallahboard?deviceId=" + testDevice,
		"https://api.example/base/": "wss://api.example/base/api/refresh-musallahboard?deviceId=" + testDevice,
	} {
		got, err := refreshURL(in, testDevice)
		if err != nil || got != want {
			t.Errorf("refreshURL(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := refreshURL("ftp://x", testDevice); err == nil {
		t.Error("ftp scheme accepted")
	}
}

func TestWeather(t *testing.T) {
	body := `{"weather":{"tempC":12,"icon":"sun"},"fetchedAt":"2026-09-24T12:00:00Z"}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/agent/weather" || r.Method != http.MethodGet {
			t.Errorf("unexpected %s %s", r.Method, r.URL)
		}
		if r.Header.Get("X-MB-Device-Id") != testDevice || r.Header.Get("X-MB-Signature") == "" {
			t.Errorf("weather request is not device-signed: %v", r.Header)
		}
		io.WriteString(w, body)
	}))
	defer srv.Close()
	e := newEnv(t, srv.URL)
	s := e.syncer
	now := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }

	if _, ok := s.Weather(); ok {
		t.Fatal("weather before any fetch")
	}
	if err := s.FetchWeather(context.Background()); err != nil {
		t.Fatal(err)
	}
	w, ok := s.Weather()
	if !ok || string(w) != `{"tempC":12,"icon":"sun"}` {
		t.Fatalf("weather = %s, %v", w, ok)
	}
	now = now.Add(2*time.Hour + 59*time.Minute)
	if _, ok := s.Weather(); !ok {
		t.Fatal("weather expired early")
	}
	now = now.Add(time.Minute)
	if _, ok := s.Weather(); ok {
		t.Fatal("weather older than 3 h still served")
	}

	// No weather object clears it.
	body = `{"weather":null}`
	s.FetchWeather(context.Background())
	if _, ok := s.Weather(); ok {
		t.Fatal("null weather kept an old value")
	}
	body = `{"weather":[1,2]}`
	s.FetchWeather(context.Background())
	if _, ok := s.Weather(); ok {
		t.Fatal("non-object weather accepted")
	}
}

// channelServer serves a channel file at /channel.json and the package at
// /pkg.mbu, counting package downloads.
type channelServer struct {
	*httptest.Server
	downloads atomic.Int32
	channel   Channel
	pkg       []byte
}

func newChannelServer(t *testing.T, version string, pkg []byte) *channelServer {
	cs := &channelServer{pkg: pkg}
	cs.channel = Channel{Version: version, URL: "pkg.mbu", SHA256: hexSHA(pkg), Bytes: int64(len(pkg))}
	cs.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/channel.json":
			json.NewEncoder(w).Encode(cs.channel)
		case "/pkg.mbu":
			cs.downloads.Add(1)
			w.Write(cs.pkg)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(cs.Close)
	return cs
}

func setAppVersion(t *testing.T, l store.Layout, v string) {
	if _, err := l.State().Update(func(s *state.State) error { s.AppVersion = v; return nil }); err != nil {
		t.Fatal(err)
	}
}

func TestAppChannel(t *testing.T) {
	cases := []struct {
		name      string
		installed string
		channel   string
		downloads int32
		waiting   string
	}{
		{"newer waits", "2.0.0", "2.1.0", 1, "2.1.0"},
		{"nothing installed waits (and installs at once)", "", "2.1.0", 1, "2.1.0"},
		{"equal skips", "2.1.0", "2.1.0", 0, ""},
		{"older skips", "2.2.0", "2.1.0", 0, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cs := newChannelServer(t, tc.channel, appPackage(t, tc.channel))
			e := newEnv(t, "http://unused")
			e.cfg.AppChannelSet = strp(cs.URL + "/channel.json")
			setAppVersion(t, e.layout, tc.installed)
			if err := e.syncer.CheckChannels(context.Background()); err != nil {
				t.Fatal(err)
			}
			if n := cs.downloads.Load(); n != tc.downloads {
				t.Errorf("downloads = %d, want %d", n, tc.downloads)
			}
			if got := e.updates.Pending(mbu.TypeApp); got != tc.waiting {
				t.Errorf("waiting = %q, want %q", got, tc.waiting)
			}
			st, _ := e.layout.State().Load()
			if st.AppVersion != tc.installed {
				t.Errorf("app version = %q: the channel check installed it", st.AppVersion)
			}
			// A second check does not download what is already waiting.
			if err := e.syncer.CheckChannels(context.Background()); err != nil {
				t.Fatal(err)
			}
			if n := cs.downloads.Load(); n != tc.downloads {
				t.Errorf("downloads after a second check = %d, want %d", n, tc.downloads)
			}
		})
	}
}

func TestAppChannelShaMismatch(t *testing.T) {
	cs := newChannelServer(t, "2.1.0", appPackage(t, "2.1.0"))
	cs.channel.SHA256 = strings.Repeat("0", 64)
	e := newEnv(t, "http://unused")
	e.cfg.AppChannelSet = strp(cs.URL + "/channel.json")
	err := e.syncer.checkChannel(context.Background(), mbu.TypeApp, cs.URL+"/channel.json")
	if err == nil || !strings.Contains(err.Error(), "SHA-256") {
		t.Fatalf("err = %v", err)
	}
	if st, _ := e.layout.State().Load(); st.AppVersion != "" {
		t.Fatalf("installed %q despite the hash mismatch", st.AppVersion)
	}
}

func TestAppChannelSizeMismatch(t *testing.T) {
	cs := newChannelServer(t, "2.1.0", appPackage(t, "2.1.0"))
	cs.channel.Bytes -= 10
	e := newEnv(t, "http://unused")
	err := e.syncer.checkChannel(context.Background(), mbu.TypeApp, cs.URL+"/channel.json")
	if err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Fatalf("err = %v", err)
	}
}

func TestChannelValidation(t *testing.T) {
	cs := newChannelServer(t, "2.1", appPackage(t, "2.1.0"))
	e := newEnv(t, "http://unused")
	err := e.syncer.checkChannel(context.Background(), mbu.TypeApp, cs.URL+"/channel.json")
	if err == nil || !strings.Contains(err.Error(), "MAJOR.MINOR.PATCH") {
		t.Fatalf("err = %v", err)
	}
}

func TestAgentChannel(t *testing.T) {
	cases := []struct {
		name      string
		running   string
		channel   string
		rejected  []string
		downloads int32
		waiting   string
	}{
		{"newer waits", "0.3.0", "0.4.0", nil, 1, "0.4.0"},
		{"equal skips", "0.3.0", "0.3.0", nil, 0, ""},
		{"older skips", "0.3.0", "0.2.9", nil, 0, ""},
		{"rejected skips", "0.3.0", "0.4.0", []string{"0.4.0"}, 0, ""},
		{"dev build skips", "dev", "0.4.0", nil, 0, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cs := newChannelServer(t, tc.channel, agentPackage(t, tc.channel))
			e := newEnv(t, "http://unused")
			e.cfg.AgentChannelSet = strp(cs.URL + "/channel.json")
			e.syncer.d.AgentVersion = tc.running
			if tc.rejected != nil {
				e.layout.State().Update(func(s *state.State) error { s.AgentVersionsRejected = tc.rejected; return nil })
			}
			e.syncer.CheckChannels(context.Background())
			if n := cs.downloads.Load(); n != tc.downloads {
				t.Errorf("downloads = %d, want %d", n, tc.downloads)
			}
			if got := e.updates.Pending(mbu.TypeAgent); got != tc.waiting {
				t.Errorf("waiting = %q, want %q", got, tc.waiting)
			}
			if _, err := os.Stat(e.layout.StagedReady()); err == nil {
				t.Error("the channel check staged the agent instead of leaving it waiting")
			}
		})
	}
}

func TestAutoUpdateOffSkipsChannels(t *testing.T) {
	cs := newChannelServer(t, "2.1.0", appPackage(t, "2.1.0"))
	e := newEnv(t, "http://127.0.0.1:1")
	e.cfg.AppChannelSet = strp(cs.URL + "/channel.json")
	e.cfg.AutoUpdateSet = boolp(false)
	e.cfg.ContentSyncSet = boolp(false)
	e.syncer.channelFirstDelay = time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	e.syncer.Run(ctx)
	if cs.downloads.Load() != 0 {
		t.Fatal("channel followed with auto_update off")
	}
	if e.syncer.Status().Enabled {
		t.Fatal("status reports sync enabled with content_sync off")
	}
}
