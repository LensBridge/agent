package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// ── Clock ────────────────────────────────────────────────────────────────────

func TestCompareClocks(t *testing.T) {
	base := time.Date(2026, 9, 24, 14, 0, 0, 0, time.UTC)
	epochOffset := time.Unix(1, 0).Sub(base)
	cases := []struct {
		name      string
		rtt       time.Duration // round trip of the status request
		boardAt   time.Duration // board's reading, relative to base
		wantDrift time.Duration
		wantSet   bool
	}{
		{"in step", 0, 0, 0, false},
		{"5 s behind is tolerated", 0, -5 * time.Second, -5 * time.Second, false},
		{"5 s ahead is tolerated", 0, 5 * time.Second, 5 * time.Second, false},
		{"6 s behind is set", 0, -6 * time.Second, -6 * time.Second, true},
		{"6 s ahead is set", 0, 6 * time.Second, 6 * time.Second, true},
		{"days behind (no RTC, power cut)", 0, -3 * 24 * time.Hour, -3 * 24 * time.Hour, true},
		{"board near the epoch", 0, epochOffset, epochOffset, true},
		// A slow link: the laptop's time is the midpoint of the call, so a
		// board that answered in step is not mistaken for 4 s behind.
		{"slow link, in step", 8 * time.Second, 4 * time.Second, 0, false},
		{"slow link, behind", 8 * time.Second, -3 * time.Second, -7 * time.Second, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := compareClocks(base, base.Add(tc.rtt), base.Add(tc.boardAt).Unix())
			if c.Drift != tc.wantDrift || c.NeedsSet != tc.wantSet {
				t.Errorf("got drift %v set %v, want %v %v", c.Drift, c.NeedsSet, tc.wantDrift, tc.wantSet)
			}
		})
	}
}

// Sub-second noise on the laptop side must not tip a 5 s drift over.
func TestCompareClocksTruncatesLaptopTime(t *testing.T) {
	sent := time.Date(2026, 9, 24, 14, 0, 0, 900_000_000, time.UTC)
	c := compareClocks(sent, sent, sent.Unix()-5)
	if c.NeedsSet || c.Drift != -5*time.Second {
		t.Errorf("got %+v", c)
	}
}

func TestDescribeDrift(t *testing.T) {
	cases := map[time.Duration]string{
		0:                      "in step with this laptop",
		400 * time.Millisecond: "in step with this laptop",
		-3 * time.Second:       "3 s behind this laptop",
		7 * time.Second:        "7 s ahead of this laptop",
		-10 * time.Minute:      "10 min behind this laptop",
		-5 * time.Hour:         "5 h behind this laptop",
		-72 * time.Hour:        "3 days behind this laptop",
	}
	for d, want := range cases {
		if got := describeDrift(d); got != want {
			t.Errorf("describeDrift(%v) = %q, want %q", d, got, want)
		}
	}
}

func TestDescribeClockReport(t *testing.T) {
	cases := []struct {
		drift    int64
		adjusted bool
		want     string
	}{
		{-180, true, "The board's clock was 3 min behind this laptop. It has been set to this laptop's time."},
		{2, false, "The board's clock is 2 s ahead of this laptop."},
		{0, false, "The board's clock is in step with this laptop."},
		{-86400 * 200, false, "The board's clock is 200 days behind this laptop, and was not changed."},
	}
	for _, tc := range cases {
		if got := describeClockReport(tc.drift, tc.adjusted); got != tc.want {
			t.Errorf("describeClockReport(%d, %v) = %q, want %q", tc.drift, tc.adjusted, got, tc.want)
		}
	}
}

// ── Arguments ────────────────────────────────────────────────────────────────

func TestParseArgs(t *testing.T) {
	cases := []struct {
		name    string
		args    []string
		check   func(options) bool
		wantErr bool
	}{
		{"files, defaults", []string{"a.mbu", "b.MBU"}, func(o options) bool {
			return o.command == "push" && o.host == defaultHost && slices.Equal(o.files, []string{"a.mbu", "b.MBU"})
		}, false},
		{"flags after the files", []string{"a.mbu", "--host", "musallahboard.local"}, func(o options) bool {
			return o.host == "musallahboard.local" && slices.Equal(o.files, []string{"a.mbu"})
		}, false},
		{"status", []string{"status"}, func(o options) bool { return o.command == "status" }, false},
		{"fetch defaults", []string{"fetch"}, func(o options) bool {
			return o.command == "fetch" && o.dir == "." && o.arch == "arm64" &&
				o.appChannel == defaultAppChannel &&
				o.agentChannel == "https://github.com/LensBridge/agent/releases/latest/download/agent-channel-arm64.json"
		}, false},
		{"fetch amd64 into a dir", []string{"fetch", "--arch=amd64", "--dir", "out"}, func(o options) bool {
			return o.dir == "out" && strings.HasSuffix(o.agentChannel, "agent-channel-amd64.json")
		}, false},
		{"ssh", []string{"--ssh", "ibra@board.local", "-i", "k", "a.mbu"}, func(o options) bool {
			return o.ssh == "ibra@board.local" && o.identity == "k" && o.command == "push"
		}, false},
		{"nothing", nil, nil, true},
		{"not a package", []string{"bundle.zip"}, nil, true},
		{"status plus a file", []string{"status", "a.mbu"}, nil, true},
		{"fetch over ssh", []string{"fetch", "--ssh", "a@b"}, nil, true},
		{"ssh without user", []string{"--ssh", "board.local", "a.mbu"}, nil, true},
		{"bad arch", []string{"fetch", "--arch", "armhf"}, nil, true},
		{"unknown flag", []string{"--hots", "x", "a.mbu"}, nil, true},
		{"empty host", []string{"--host=", "status"}, nil, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseArgs(tc.args)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("accepted %v: %+v", tc.args, got)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !tc.check(got) {
				t.Errorf("got %+v", got)
			}
		})
	}
}

func TestSSHArgs(t *testing.T) {
	o := options{ssh: "ibra@" + defaultHost, identity: "key"}
	args := strings.Join(sshArgs(o, remoteAgent+" import -"), " ")
	for _, want := range []string{"-i key", "StrictHostKeyChecking=no", "UserKnownHostsFile=",
		"ibra@10.77.0.1 sudo -n musallahboard-agent import -"} {
		if !strings.Contains(args, want) {
			t.Errorf("ssh args %q missing %q", args, want)
		}
	}
	o.checkHostKey = true
	if args := strings.Join(sshArgs(o, "x"), " "); strings.Contains(args, "StrictHostKeyChecking") {
		t.Errorf("--check-host-key still disables checking: %s", args)
	}
	o = options{ssh: "ibra@board.local"}
	if args := strings.Join(sshArgs(o, "x"), " "); strings.Contains(args, "StrictHostKeyChecking") {
		t.Errorf("host keys unchecked on a named host: %s", args)
	}
}

// ── Upload ───────────────────────────────────────────────────────────────────

type receivedPart struct {
	field, file string
	body        []byte
}

// fakeBoard is an upload server that records what it was sent.
type fakeBoard struct {
	mu         sync.Mutex
	parts      []receivedPart
	clientTime string
	host       string

	status   int
	response any
}

func (b *fakeBoard) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/api/import":
		mr, err := r.MultipartReader()
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		b.mu.Lock()
		b.clientTime = r.Header.Get("X-MB-Client-Time")
		b.host = r.Host
		b.mu.Unlock()
		for {
			p, err := mr.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			body, _ := io.ReadAll(p)
			b.mu.Lock()
			b.parts = append(b.parts, receivedPart{p.FormName(), p.FileName(), body})
			b.mu.Unlock()
		}
	case r.Method == http.MethodGet && r.URL.Path == "/api/status":
	default:
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if b.status != 0 {
		w.WriteHeader(b.status)
	}
	json.NewEncoder(w).Encode(b.response)
}

func writeFile(t *testing.T, dir, name string, data []byte) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestPushSendsMultipartAndPrintsResults(t *testing.T) {
	dir := t.TempDir()
	app := writeFile(t, dir, "musallahboard-app-2.1.0.mbu", bytes.Repeat([]byte("A"), 300_000))
	content := writeFile(t, dir, "musallahboard-content-3f2a1b4c-2026-09-24.mbu", []byte("content bytes"))

	board := &fakeBoard{response: map[string]any{
		"results": []map[string]any{
			{"file": "musallahboard-app-2.1.0.mbu", "type": "app", "version": "2.1.0",
				"action": "installed", "message": "Installed board app 2.1.0"},
			{"file": "musallahboard-content-3f2a1b4c-2026-09-24.mbu", "type": "content",
				"action": "unchanged", "message": "Content already installed"},
		},
		"clock": map[string]any{"driftSeconds": -180, "adjusted": true, "note": "saved to the RTC"},
	}}
	srv := httptest.NewServer(board)
	defer srv.Close()

	c := newBoardClient(srv.URL)
	fixed := time.Unix(1790258531, 0)
	c.now = func() time.Time { return fixed }
	var out bytes.Buffer
	if err := runPush(c, []string{app, content}, &out); err != nil {
		t.Fatalf("runPush: %v\n%s", err, out.String())
	}

	if len(board.parts) != 2 {
		t.Fatalf("board got %d parts, want 2", len(board.parts))
	}
	for i, want := range []string{app, content} {
		p := board.parts[i]
		data, _ := os.ReadFile(want)
		if p.field != "package" || p.file != filepath.Base(want) || !bytes.Equal(p.body, data) {
			t.Errorf("part %d: field %q file %q, %d bytes; want package %q, %d bytes",
				i, p.field, p.file, len(p.body), filepath.Base(want), len(data))
		}
	}
	if board.clientTime != strconv.FormatInt(fixed.Unix(), 10) {
		t.Errorf("X-MB-Client-Time = %q", board.clientTime)
	}
	for _, want := range []string{
		"Installed          musallahboard-app-2.1.0.mbu",
		"Installed board app 2.1.0",
		"Already installed  musallahboard-content-3f2a1b4c-2026-09-24.mbu",
		"The board's clock was 3 min behind this laptop. It has been set to this laptop's time.",
		"(saved to the RTC)",
		"Update complete",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output missing %q:\n%s", want, out.String())
		}
	}
}

func TestPushFailsWhenAPackageIsRejected(t *testing.T) {
	dir := t.TempDir()
	f := writeFile(t, dir, "old.mbu", []byte("x"))
	board := &fakeBoard{response: map[string]any{
		"results": []map[string]any{
			{"file": "old.mbu", "type": "content", "action": "rejected", "message": "older than what is installed"},
		},
	}}
	srv := httptest.NewServer(board)
	defer srv.Close()

	var out bytes.Buffer
	err := runPush(newBoardClient(srv.URL), []string{f}, &out)
	if err == nil || !strings.Contains(err.Error(), "1 package(s) were not installed") {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(out.String(), "NOT INSTALLED      old.mbu") || !strings.Contains(out.String(), "older than what is installed") {
		t.Errorf("output:\n%s", out.String())
	}
}

func TestPushStagedAgentMentionsRestart(t *testing.T) {
	f := writeFile(t, t.TempDir(), "musallahboard-agent-0.3.0-arm64.mbu", []byte("x"))
	board := &fakeBoard{response: map[string]any{
		"results": []map[string]any{{"file": filepath.Base(f), "type": "agent", "action": "staged"}},
	}}
	srv := httptest.NewServer(board)
	defer srv.Close()
	var out bytes.Buffer
	if err := runPush(newBoardClient(srv.URL), []string{f}, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "restarts it by itself") {
		t.Errorf("output:\n%s", out.String())
	}
}

func TestPushErrors(t *testing.T) {
	f := writeFile(t, t.TempDir(), "a.mbu", []byte("x"))
	cases := []struct {
		status int
		want   string
	}{
		{http.StatusConflict, "already installing another upload"},
		{http.StatusMisdirectedRequest, "only answers to 10.77.0.1 or musallahboard.local"},
		{http.StatusBadRequest, "the board answered 400 Bad Request: part is not a package"},
	}
	for _, tc := range cases {
		t.Run(strconv.Itoa(tc.status), func(t *testing.T) {
			srv := httptest.NewServer(&fakeBoard{status: tc.status, response: map[string]string{"message": "part is not a package"}})
			defer srv.Close()
			var out bytes.Buffer
			err := runPush(newBoardClient(srv.URL), []string{f}, &out)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

// A board that answers before reading the upload (busy) must not hang mbpush.
func TestPushBoardAnswersEarly(t *testing.T) {
	f := writeFile(t, t.TempDir(), "big.mbu", bytes.Repeat([]byte("B"), 8<<20))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		fmt.Fprint(w, `{"message":"busy"}`)
	}))
	defer srv.Close()
	done := make(chan error, 1)
	go func() { done <- runPush(newBoardClient(srv.URL), []string{f}, io.Discard) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected an error")
		}
	case <-time.After(20 * time.Second):
		t.Fatal("runPush hung")
	}
}

func TestPushUnreachableBoard(t *testing.T) {
	f := writeFile(t, t.TempDir(), "a.mbu", []byte("x"))
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close() // nothing listens there any more
	err := runPush(newBoardClient(url), []string{f}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "could not reach the board") ||
		!strings.Contains(err.Error(), "Nothing on the board has changed") {
		t.Errorf("err = %v", err)
	}
}

func TestPushRefusesOversizedBatchBeforeSending(t *testing.T) {
	dir := t.TempDir()
	// Sparse files: the size check must not need to read them.
	var files []string
	for i := 0; i < 3; i++ {
		p := filepath.Join(dir, fmt.Sprintf("p%d.mbu", i))
		fh, err := os.Create(p)
		if err != nil {
			t.Fatal(err)
		}
		if err := fh.Truncate(400 << 20); err != nil {
			t.Fatal(err)
		}
		fh.Close()
		files = append(files, p)
	}
	err := runPush(newBoardClient("http://192.0.2.1"), files, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "two goes") {
		t.Errorf("err = %v", err)
	}
}

// ── Status ───────────────────────────────────────────────────────────────────

func TestStatus(t *testing.T) {
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	seven, zero := 7, 0
	st := map[string]any{
		"deviceId": "3f2a1b4c-5d6e-4f70-8a9b-0c1d2e3f4a5b", "agentVersion": "0.3.0",
		"app": map[string]any{"version": "2.1.0"},
		"content": map[string]any{"firstDay": "2026-09-24", "lastDay": "2026-10-07",
			"timezone": "America/Toronto", "source": "usb"},
		"today": "2026-09-30", "daysRemaining": seven, "staleDays": zero,
		"clock":  map[string]any{"unix": now.Add(-10 * time.Minute).Unix(), "timezone": "America/Toronto"},
		"rtc":    true,
		"update": map[string]any{"active": false},
	}
	srv := httptest.NewServer(&fakeBoard{response: st})
	defer srv.Close()
	c := newBoardClient(srv.URL)
	c.now = func() time.Time { return now }
	var out bytes.Buffer
	if err := runStatus(c, &out); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"3f2a1b4c-5d6e-4f70-8a9b-0c1d2e3f4a5b",
		"Board app   2.1.0",
		"2026-09-24 to 2026-10-07 (America/Toronto), from a USB stick",
		"Today is 2026-09-30: 7 days of content left after today.",
		"10 min behind this laptop (time zone America/Toronto)",
		"Sending a package with mbpush sets it",
		"RTC         fitted",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output missing %q:\n%s", want, out.String())
		}
	}
}

func TestStatusNothingInstalled(t *testing.T) {
	var out bytes.Buffer
	printStatus(&out, defaultHost, boardStatus{DeviceID: "d"}, time.Now(), time.Now())
	for _, want := range []string{"Board app   not installed", "Content     none installed", "RTC         none"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output missing %q:\n%s", want, out.String())
		}
	}
	stale := 3
	var st boardStatus
	if err := json.Unmarshal([]byte(`{"content":{"firstDay":"2026-09-01","lastDay":"2026-09-14","source":"sync"}}`), &st); err != nil {
		t.Fatal(err)
	}
	st.StaleDays = &stale
	out.Reset()
	printStatus(&out, defaultHost, st, time.Now(), time.Now())
	if !strings.Contains(out.String(), "OUT OF DATE: it ran out 3 days ago, so the board is repeating 2026-09-14.") {
		t.Errorf("output:\n%s", out.String())
	}
}

// ── Fetch ────────────────────────────────────────────────────────────────────

// releaseServer serves two channel files and the packages they point at.
func releaseServer(t *testing.T, app, agent []byte, tamper bool) *httptest.Server {
	t.Helper()
	sum := func(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
	mux := http.NewServeMux()
	var srv *httptest.Server
	mux.HandleFunc("/app-channel.json", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(channel{Version: "2.1.0", URL: srv.URL + "/dl/musallahboard-app-2.1.0.mbu",
			SHA256: sum(app), Bytes: int64(len(app))})
	})
	mux.HandleFunc("/agent-channel-arm64.json", func(w http.ResponseWriter, r *http.Request) {
		// Relative, to check it resolves against the channel URL.
		json.NewEncoder(w).Encode(channel{Version: "0.3.0", URL: "dl/musallahboard-agent-0.3.0-arm64.mbu",
			SHA256: sum(agent), Bytes: int64(len(agent))})
	})
	mux.HandleFunc("/dl/musallahboard-app-2.1.0.mbu", func(w http.ResponseWriter, r *http.Request) { w.Write(app) })
	mux.HandleFunc("/dl/musallahboard-agent-0.3.0-arm64.mbu", func(w http.ResponseWriter, r *http.Request) {
		if tamper {
			w.Write(append([]byte("X"), agent[1:]...))
			return
		}
		w.Write(agent)
	})
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func fetchOptions(srv *httptest.Server, dir string) options {
	return options{command: "fetch", dir: dir, arch: "arm64",
		appChannel: srv.URL + "/app-channel.json", agentChannel: srv.URL + "/agent-channel-arm64.json"}
}

func TestFetch(t *testing.T) {
	app, agent := bytes.Repeat([]byte("app"), 1000), []byte("agent binary package")
	srv := releaseServer(t, app, agent, false)
	dir := filepath.Join(t.TempDir(), "visit")

	var out bytes.Buffer
	if err := runFetch(srv.Client(), fetchOptions(srv, dir), &out); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	for name, want := range map[string][]byte{
		"musallahboard-app-2.1.0.mbu":         app,
		"musallahboard-agent-0.3.0-arm64.mbu": agent,
	} {
		got, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil || !bytes.Equal(got, want) {
			t.Errorf("%s: %v, %d bytes", name, err, len(got))
		}
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 2 {
		t.Errorf("left behind: %v", entries)
	}

	// Again: nothing to download.
	out.Reset()
	if err := runFetch(srv.Client(), fetchOptions(srv, dir), &out); err != nil {
		t.Fatal(err)
	}
	if strings.Count(out.String(), "already downloaded") != 2 {
		t.Errorf("second fetch:\n%s", out.String())
	}
}

func TestFetchRejectsBadDownload(t *testing.T) {
	srv := releaseServer(t, []byte("app"), []byte("agent"), true)
	dir := t.TempDir()
	var out bytes.Buffer
	err := runFetch(srv.Client(), fetchOptions(srv, dir), &out)
	if err == nil || !strings.Contains(err.Error(), "1 download(s) failed") {
		t.Fatalf("err = %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "does not match the release channel's sha256") {
		t.Errorf("output:\n%s", out.String())
	}
	entries, _ := os.ReadDir(dir)
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if !slices.Equal(names, []string{"musallahboard-app-2.1.0.mbu"}) {
		t.Errorf("files = %v; the bad download (or its .part) was kept", names)
	}
}

func TestFetchChannelValidation(t *testing.T) {
	cases := map[string]channel{
		"bad version":  {Version: "latest", URL: "x.mbu", SHA256: strings.Repeat("a", 64), Bytes: 1},
		"bad sha":      {Version: "1.0.0", URL: "x.mbu", SHA256: "abc", Bytes: 1},
		"too big":      {Version: "1.0.0", URL: "x.mbu", SHA256: strings.Repeat("a", 64), Bytes: maxPackageBytes + 1},
		"not a .mbu":   {Version: "1.0.0", URL: "x.exe", SHA256: strings.Repeat("a", 64), Bytes: 1},
		"path escape":  {Version: "1.0.0", URL: "../..", SHA256: strings.Repeat("a", 64), Bytes: 1},
		"other scheme": {Version: "1.0.0", URL: "file:///etc/x.mbu", SHA256: strings.Repeat("a", 64), Bytes: 1},
	}
	for name, ch := range cases {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				json.NewEncoder(w).Encode(ch)
			}))
			defer srv.Close()
			if _, err := fetchChannel(srv.Client(), srv.URL+"/c.json", t.TempDir()); err == nil {
				t.Errorf("accepted %+v", ch)
			}
		})
	}
}
