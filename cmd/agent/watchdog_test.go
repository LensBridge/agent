package main

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestWatchdogPingsOnlyWhileLive(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hb := newHeartbeat()
	var pings atomic.Int32
	interval := 80 * time.Millisecond
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		runWatchdog(ctx, slog.New(slog.DiscardHandler), hb, interval, func() { pings.Add(1) })
	}()

	// Beating: pings.
	for range 10 {
		hb.beat()
		time.Sleep(interval / 4)
	}
	if pings.Load() == 0 {
		t.Fatal("no pings while live")
	}
	// Stalled: once the heartbeat is older than interval, no more pings.
	time.Sleep(interval + interval/2)
	before := pings.Load()
	time.Sleep(3 * interval)
	if after := pings.Load(); after != before {
		t.Errorf("pinged %d times with a stale heartbeat", after-before)
	}
	// Live again: pings resume.
	deadline := time.Now().Add(2 * time.Second)
	for pings.Load() == before && time.Now().Before(deadline) {
		hb.beat()
		time.Sleep(interval / 8)
	}
	if pings.Load() == before {
		t.Error("pings did not resume")
	}
	cancel()
	wg.Wait()
}

func TestProbeLoopBeatsOnlyOnAnswer(t *testing.T) {
	var ok atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !ok.Load() {
			http.Error(w, "no", http.StatusInternalServerError)
			return
		}
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hb := newHeartbeat()
	start := *hb.last.Load()
	done := make(chan struct{})
	go func() { probeLoop(ctx, hb, srv.URL, 10*time.Millisecond); close(done) }()

	time.Sleep(60 * time.Millisecond)
	if !hb.last.Load().Equal(start) {
		t.Fatal("a failing server counted as a heartbeat")
	}
	ok.Store(true)
	deadline := time.Now().Add(2 * time.Second)
	for hb.last.Load().Equal(start) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if hb.last.Load().Equal(start) {
		t.Fatal("an answering server did not count as a heartbeat")
	}
	cancel()
	<-done
}
