package updates

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LensBridge/agent/internal/core/mbu"
	"github.com/LensBridge/agent/internal/core/state"
)

func hexSHA(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func packageBytes(t *testing.T, m mbu.Manifest, src []mbu.Source) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := mbu.Build(&buf, m, src, []ed25519.PrivateKey{releaseKey}); err != nil {
		t.Fatalf("Build: %v", err)
	}
	return buf.Bytes()
}

func appPackageBytes(t *testing.T, version string) []byte {
	return packageBytes(t, mbu.Manifest{Type: mbu.TypeApp, Version: version, App: &mbu.AppInfo{LocalAPI: 2}},
		[]mbu.Source{{Path: "index.html", Data: []byte("<html>" + version)}})
}

func agentPackageBytes(t *testing.T, version string) []byte {
	return packageBytes(t, mbu.Manifest{Type: mbu.TypeAgent, Version: version,
		Agent: &mbu.AgentInfo{Arch: runtime.GOARCH, Binary: "musallahboard-agent"}},
		[]mbu.Source{{Path: "musallahboard-agent", Data: []byte("#!/bin/sh\necho " + version)}})
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
			cs := newChannelServer(t, tc.channel, appPackageBytes(t, tc.channel))
			e := newEnv(t)
			e.s = e.newScheduler(func(d *Deps) { d.AppChannel = cs.URL + "/channel.json" })
			setApp(t, e.l, tc.installed)
			if err := e.s.CheckChannels(context.Background()); err != nil {
				t.Fatal(err)
			}
			if n := cs.downloads.Load(); n != tc.downloads {
				t.Errorf("downloads = %d, want %d", n, tc.downloads)
			}
			if got := e.s.Pending(mbu.TypeApp); got != tc.waiting {
				t.Errorf("waiting = %q, want %q", got, tc.waiting)
			}
			st, _ := e.l.State().Load()
			if st.AppVersion != tc.installed {
				t.Errorf("app version = %q: the channel check installed it", st.AppVersion)
			}
			// A second check does not download what is already waiting.
			if err := e.s.CheckChannels(context.Background()); err != nil {
				t.Fatal(err)
			}
			if n := cs.downloads.Load(); n != tc.downloads {
				t.Errorf("downloads after a second check = %d, want %d", n, tc.downloads)
			}
		})
	}
}

func TestAppChannelShaMismatch(t *testing.T) {
	cs := newChannelServer(t, "2.1.0", appPackageBytes(t, "2.1.0"))
	cs.channel.SHA256 = strings.Repeat("0", 64)
	e := newEnv(t)
	err := e.s.checkChannel(context.Background(), mbu.TypeApp, cs.URL+"/channel.json")
	if err == nil || !strings.Contains(err.Error(), "SHA-256") {
		t.Fatalf("err = %v", err)
	}
	if st, _ := e.l.State().Load(); st.AppVersion != "2.0.0" {
		t.Fatalf("installed %q despite the hash mismatch", st.AppVersion)
	}
}

func TestAppChannelSizeMismatch(t *testing.T) {
	cs := newChannelServer(t, "2.1.0", appPackageBytes(t, "2.1.0"))
	cs.channel.Bytes -= 10
	e := newEnv(t)
	err := e.s.checkChannel(context.Background(), mbu.TypeApp, cs.URL+"/channel.json")
	if err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Fatalf("err = %v", err)
	}
}

func TestChannelValidation(t *testing.T) {
	cs := newChannelServer(t, "2.1", appPackageBytes(t, "2.1.0"))
	e := newEnv(t)
	err := e.s.checkChannel(context.Background(), mbu.TypeApp, cs.URL+"/channel.json")
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
			cs := newChannelServer(t, tc.channel, agentPackageBytes(t, tc.channel))
			e := newEnv(t)
			e.s = e.newScheduler(func(d *Deps) { d.AgentChannel = cs.URL + "/channel.json"; d.AgentVersion = tc.running })
			if tc.rejected != nil {
				e.l.State().Update(func(s *state.State) error { s.AgentVersionsRejected = tc.rejected; return nil })
			}
			e.s.CheckChannels(context.Background())
			if n := cs.downloads.Load(); n != tc.downloads {
				t.Errorf("downloads = %d, want %d", n, tc.downloads)
			}
			if got := e.s.Pending(mbu.TypeAgent); got != tc.waiting {
				t.Errorf("waiting = %q, want %q", got, tc.waiting)
			}
			if _, err := os.Stat(e.l.StagedReady()); err == nil {
				t.Error("the channel check staged the agent instead of leaving it waiting")
			}
		})
	}
}

func TestAutoUpdateOffSkipsChannels(t *testing.T) {
	cs := newChannelServer(t, "2.1.0", appPackageBytes(t, "2.1.0"))
	e := newEnv(t)
	e.s = e.newScheduler(func(d *Deps) { d.AppChannel = cs.URL + "/channel.json"; d.AutoUpdate = false })
	e.s.channelFirstDelay = time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	e.s.Run(ctx)
	if cs.downloads.Load() != 0 {
		t.Fatal("channel followed with auto_update off")
	}
}

// A channel request that gets no answer (a dead pooled connection, a board
// that just lost its network) is given up on and retried once on a new
// connection, instead of waiting minutes for response headers.
func TestChannelFetchRetriesAStuckRequest(t *testing.T) {
	cs := newChannelServer(t, "2.1.0", appPackageBytes(t, "2.1.0"))
	release := make(chan struct{})
	var calls atomic.Int32
	stuck := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			select {
			case <-release:
			case <-r.Context().Done():
			}
			return
		}
		http.Redirect(w, r, cs.URL+"/channel.json", http.StatusFound)
	}))
	defer stuck.Close()
	defer close(release)

	e := newEnv(t)
	e.s.channelTimeout = 200 * time.Millisecond
	ch, err := e.s.fetchChannel(context.Background(), stuck.URL+"/channel.json")
	if err != nil {
		t.Fatal(err)
	}
	if ch.Version != "2.1.0" || calls.Load() != 2 {
		t.Fatalf("version %q after %d calls", ch.Version, calls.Load())
	}
}

// A server that answers is not asked twice.
func TestChannelFetchDoesNotRetryAnAnswer(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.NotFound(w, r)
	}))
	defer srv.Close()
	e := newEnv(t)
	if _, err := e.s.fetchChannel(context.Background(), srv.URL+"/channel.json"); err == nil || calls.Load() != 1 {
		t.Fatalf("err %v after %d calls", err, calls.Load())
	}
}

// A failed check is retried soon, not after the full channel interval.
func TestFailedChannelCheckRetriesSoon(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, "down", http.StatusBadGateway)
	}))
	defer srv.Close()
	e := newEnv(t)
	e.s = e.newScheduler(func(d *Deps) { d.AppChannel = srv.URL + "/channel.json"; d.AutoUpdate = true })
	e.s.channelFirstDelay = time.Millisecond
	e.s.channelRetry = 10 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go e.s.Run(ctx)
	for calls.Load() < 3 {
		if ctx.Err() != nil {
			t.Fatalf("%d checks in 5s; a failure waited the full interval", calls.Load())
		}
		time.Sleep(5 * time.Millisecond)
	}
	if e.s.Info().LastCheckError == "" {
		t.Fatal("the failure was not reported")
	}
}
