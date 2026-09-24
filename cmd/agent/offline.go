package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-systemd/v22/daemon"

	"github.com/LensBridge/agent/internal/cdp"
	"github.com/LensBridge/agent/internal/config"
	"github.com/LensBridge/agent/internal/kioskurl"
	"github.com/LensBridge/agent/internal/offline"
)

const (
	// kioskRecoverWindow is how long after startup the agent watches for the
	// kiosk sitting on Chromium's own error page. On boot the kiosk and the
	// agent start in parallel, and a kiosk that wins the race finds nothing on
	// 127.0.0.1:8080 yet. Unlike the online board there is no later network
	// event to nudge it, so it would stay on the error page until someone
	// intervened.
	kioskRecoverWindow   = 2 * time.Minute
	kioskRecoverInterval = 5 * time.Second
)

// startOffline runs offline mode: the local HTTP server for the kiosk and the
// offline kiosk URL. It makes no outbound network calls; the only connections
// it makes are to Chromium's DevTools port on loopback.
//
// The listener is bound before kiosk-url is written, so a kiosk restarted by
// that write always finds the server already accepting.
func startOffline(ctx context.Context, logger *slog.Logger, cfg *config.Config, wg *sync.WaitGroup) error {
	paths := offline.DefaultPaths()

	ln, err := net.Listen("tcp", offline.ListenAddr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", offline.ListenAddr, err)
	}
	srv := offline.NewServer(paths, cfg.DeviceID, logger)
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := srv.Serve(ctx, ln); err != nil {
			// Without the server the board has nothing to show; let systemd
			// restart us rather than idle with the watchdog still happy.
			logger.Error("offline server stopped", "err", err)
			os.Exit(1)
		}
	}()

	st := offline.ReadStatus(paths, config.ModeOffline, cfg.DeviceID, time.Now())
	switch {
	case st.Error != "":
		logger.Error("installed offline bundle is unreadable", "err", st.Error)
	case st.Bundle == nil:
		logger.Warn("offline mode with no bundle installed; the board will show an error until one is pushed")
	default:
		logger.Info("offline mode",
			"listen", offline.ListenAddr,
			"deviceId", cfg.DeviceID,
			"firstDay", st.Bundle.FirstDay,
			"lastDay", st.Bundle.LastDay,
			"servingDay", *st.ServingDay,
			"daysRemaining", *st.DaysRemaining,
			"staleDays", *st.StaleDays,
		)
	}

	if err := kioskurl.WriteOffline(kioskurl.DefaultOutPath, cfg.DeviceID); err != nil {
		logger.Warn("could not write offline kiosk url (kiosk will wait)", "err", err)
	} else {
		logger.Info("kiosk url written", "path", kioskurl.DefaultOutPath, "mode", config.ModeOffline)
	}

	_, _ = daemon.SdNotify(false, daemon.SdNotifyReady)
	_, _ = daemon.SdNotify(false, "STATUS=offline mode")

	wg.Add(1)
	go func() {
		defer wg.Done()
		recoverKioskErrorPage(ctx, logger, cdp.New(""), kioskurl.OfflineURL(cfg.DeviceID))
	}()
	return nil
}

// recoverKioskErrorPage watches the kiosk for a short while after startup and,
// if it is showing Chromium's network-error page, navigates it to url. It
// stops as soon as the board has loaded from the local server.
func recoverKioskErrorPage(ctx context.Context, logger *slog.Logger, c *cdp.Client, url string) {
	deadline := time.NewTimer(kioskRecoverWindow)
	defer deadline.Stop()
	tick := time.NewTicker(kioskRecoverInterval)
	defer tick.Stop()

	for {
		callCtx, cancel := context.WithTimeout(ctx, kioskRecoverInterval)
		target, err := c.FirstPageTarget(callCtx)
		if err == nil {
			switch {
			case strings.HasPrefix(target.URL, kioskurl.OfflineBaseURL):
				cancel()
				return
			case strings.HasPrefix(target.URL, "chrome-error:"):
				logger.Info("kiosk is on an error page; pointing it at the local server")
				if err := c.Call(callCtx, target, "Page.navigate", map[string]any{"url": url}, nil); err != nil {
					logger.Debug("kiosk navigate failed", "err", err)
				}
			}
		}
		cancel()

		select {
		case <-tick.C:
		case <-deadline.C:
			return
		case <-ctx.Done():
			return
		}
	}
}
