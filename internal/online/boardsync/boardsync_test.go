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
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LensBridge/agent/internal/core/config"
	"github.com/LensBridge/agent/internal/core/importer"
	"github.com/LensBridge/agent/internal/core/mbu"
	"github.com/LensBridge/agent/internal/core/store"
	"github.com/LensBridge/agent/internal/core/trust"
	"github.com/LensBridge/agent/internal/online/devauth"
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
	layout store.Layout
	cfg    *config.Config
	syncer *Syncer
}

func boolp(b bool) *bool { return &b }

// newEnv builds a Syncer over a temp store with a real importer that trusts
// contentKey and releaseKey.
func newEnv(t *testing.T, backend string) *env {
	t.Helper()
	l := store.Layout{Root: t.TempDir()}
	cfg := &config.Config{
		DeviceID:     testDevice,
		BackendURL:   backend,
		WebSocketURL: "ws://unused/api/agent/ws",
	}
	ring := trust.NewRing(
		[]ed25519.PublicKey{contentKey.Public().(ed25519.PublicKey)},
		[]ed25519.PublicKey{releaseKey.Public().(ed25519.PublicKey)},
	)
	im := importer.New(importer.Deps{
		Layout:       l,
		Config:       config.InMemory(config.Config{DeviceID: testDevice}),
		AgentVersion: "0.3.0",
		Ring:         func() (*trust.Ring, error) { return ring, nil },
	})
	s := New(Deps{Cfg: cfg, Key: deviceKey, Layout: l, Importer: im, AgentVersion: "0.3.0"})
	return &env{layout: l, cfg: cfg, syncer: s}
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

func TestTriggerFoldsIntoOneSync(t *testing.T) {
	pkg := contentPackage(t, 1000)
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/agent/content-bundle" {
			http.NotFound(w, r)
			return
		}
		hits.Add(1)
		w.Write(pkg)
	}))
	defer srv.Close()
	e := newEnv(t, srv.URL)
	e.syncer.triggerDelay, e.syncer.triggerJitter = 50*time.Millisecond, 100*time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { e.syncer.Run(ctx); close(done) }()
	deadline := time.Now().Add(10 * time.Second)
	for hits.Load() < 1 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	// Triggers in quick succession fold into one sync, and it waits at
	// least the delay.
	asked := time.Now()
	for range 5 {
		e.syncer.Trigger()
		time.Sleep(5 * time.Millisecond)
	}
	for hits.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if waited := time.Since(asked); waited < e.syncer.triggerDelay {
		t.Fatalf("synced %s after the trigger, before the delay", waited)
	}
	// Then long enough for any extra, unfolded sync to show up.
	time.Sleep(4 * (e.syncer.triggerDelay + e.syncer.triggerJitter))
	cancel()
	<-done
	// One at start, one for the burst of triggers.
	if n := hits.Load(); n != 2 {
		t.Fatalf("%d content syncs, want 2", n)
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

func TestContentSyncOff(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer srv.Close()
	e := newEnv(t, srv.URL)
	e.cfg.ContentSyncSet = boolp(false)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	e.syncer.Run(ctx)
	if hits.Load() != 0 {
		t.Fatal("synced with content_sync off")
	}
	if e.syncer.Status().Enabled {
		t.Fatal("status reports sync enabled with content_sync off")
	}
}
