package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/LensBridge/agent/internal/core/config"
	"github.com/LensBridge/agent/internal/core/importer"
	"github.com/LensBridge/agent/internal/core/store"
	"github.com/LensBridge/agent/internal/core/trust"
	"github.com/LensBridge/agent/internal/core/uploadserver"
)

// realBoard is the board's own upload server, so these tests hold mbpush to
// the real request and response shapes rather than a copy of them. The test
// server's address is 127.0.0.1:<port>, so requests are re-addressed to the
// board's name for the Host guard.
func realBoard(t *testing.T, apply func(int64) uploadserver.ClockReport) (*httptest.Server, *importer.Importer) {
	t.Helper()
	l := store.Layout{Root: t.TempDir()}
	im := importer.New(importer.Deps{Layout: l, Config: config.InMemory(config.Config{DeviceID: "3f2a"}), AgentVersion: "0.3.0",
		Ring: func() (*trust.Ring, error) { return trust.NewRing(nil, nil), nil }})
	s := uploadserver.New(uploadserver.Deps{Layout: l, DeviceID: func() string { return "3f2a" }, AgentVersion: "0.3.0", Importer: im,
		ApplyClockOnlyTime: apply})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Host = uploadserver.ServiceIP
		s.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, im
}

func TestClockSetsTheBoardsClock(t *testing.T) {
	fixed := time.Unix(1790258531, 0)
	var got int64
	srv, _ := realBoard(t, func(unix int64) uploadserver.ClockReport {
		got = unix
		return uploadserver.ClockReport{DriftSeconds: -7200, Adjusted: true, Note: "the board's clock was 2 hours behind and has been set to this device's time"}
	})
	c := newBoardClient(srv.URL)
	c.now = func() time.Time { return fixed }
	var out bytes.Buffer
	if err := runClock(c, &out); err != nil {
		t.Fatalf("runClock: %v\n%s", err, out.String())
	}
	if got != fixed.Unix() {
		t.Errorf("board got client time %d, want %d", got, fixed.Unix())
	}
	for _, want := range []string{
		"The board's clock was 2 h behind this laptop. It has been set to this laptop's time.",
		"(the board's clock was 2 hours behind",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output missing %q:\n%s", want, out.String())
		}
	}
}

func TestClockNotChanged(t *testing.T) {
	srv, _ := realBoard(t, func(int64) uploadserver.ClockReport {
		return uploadserver.ClockReport{DriftSeconds: 3600, Note: "more than 90 days past the newest update package"}
	})
	var out bytes.Buffer
	err := runClock(newBoardClient(srv.URL), &out)
	if err == nil || !strings.Contains(err.Error(), "was not changed") {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(out.String(), "60 min ahead of this laptop, and was not changed") {
		t.Errorf("output:\n%s", out.String())
	}

	// In step: nothing to change, and not a failure.
	srv, _ = realBoard(t, func(int64) uploadserver.ClockReport {
		return uploadserver.ClockReport{DriftSeconds: 2, Note: "the board's clock agrees with this device"}
	})
	if err := runClock(newBoardClient(srv.URL), &out); err != nil {
		t.Fatal(err)
	}
}

func TestClockErrors(t *testing.T) {
	// Busy installing: the real server's 409.
	srv, im := realBoard(t, func(int64) uploadserver.ClockReport {
		t.Error("clock set during an import")
		return uploadserver.ClockReport{}
	})
	release, held := make(chan struct{}), make(chan struct{})
	go im.TryExclusive(func() { close(held); <-release })
	<-held
	err := runClock(newBoardClient(srv.URL), &bytes.Buffer{})
	close(release)
	if err == nil || !strings.Contains(err.Error(), "installing an update right now") {
		t.Errorf("busy: %v", err)
	}

	// An agent from before POST /api/clock.
	old, _ := realBoard(t, nil)
	if err := runClock(newBoardClient(old.URL), &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "too old to set its clock") {
		t.Errorf("old agent: %v", err)
	}

	// The wrong name: the real Host guard.
	guarded := httptest.NewServer(uploadserver.New(uploadserver.Deps{}))
	defer guarded.Close()
	if err := runClock(newBoardClient(guarded.URL), &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "only answers to 10.77.0.1") {
		t.Errorf("wrong host: %v", err)
	}
}

func TestClockSendsTimeHeader(t *testing.T) {
	var header string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/clock" || r.ContentLength > 0 {
			t.Errorf("%s %s, %d bytes", r.Method, r.URL.Path, r.ContentLength)
		}
		header = r.Header.Get("X-MB-Client-Time")
		w.Write([]byte(`{"clock":{"driftSeconds":0,"adjusted":false,"note":"agrees"}}`))
	}))
	defer srv.Close()
	c := newBoardClient(srv.URL)
	c.now = func() time.Time { return time.Unix(1790000000, 0) }
	if err := runClock(c, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if header != strconv.Itoa(1790000000) {
		t.Errorf("X-MB-Client-Time = %q", header)
	}
}
