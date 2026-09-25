package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-systemd/v22/daemon"

	"github.com/LensBridge/agent/internal/boardsync"
	"github.com/LensBridge/agent/internal/cdp"
	"github.com/LensBridge/agent/internal/clock"
	"github.com/LensBridge/agent/internal/commands"
	"github.com/LensBridge/agent/internal/config"
	"github.com/LensBridge/agent/internal/events"
	"github.com/LensBridge/agent/internal/importer"
	"github.com/LensBridge/agent/internal/keystore"
	"github.com/LensBridge/agent/internal/kioskurl"
	"github.com/LensBridge/agent/internal/localserver"
	"github.com/LensBridge/agent/internal/store"
	"github.com/LensBridge/agent/internal/trust"
	"github.com/LensBridge/agent/internal/updates"
	"github.com/LensBridge/agent/internal/updatescreen"
	"github.com/LensBridge/agent/internal/uploadserver"
	"github.com/LensBridge/agent/internal/version"
	"github.com/LensBridge/agent/internal/wsclient"
)

// kioskWatchInterval is how often the daemon checks that Chromium is not
// sitting on its own error page. The kiosk is ordered after the agent, so
// this is a backstop, not the mechanism.
const kioskWatchInterval = 30 * time.Second

// startBoard starts everything an enrolled board runs (docs/architecture.md,
// section 3). The local server comes first and is the only part whose failure
// is fatal: without it the board has nothing to show. Everything that needs
// the network or the device key degrades to "not running" instead, because a
// board that cannot reach LensBridge must still show what it has.
func startBoard(ctx context.Context, logger *slog.Logger, cfg *config.Config, safeMode bool, wg *sync.WaitGroup) error {
	layout := store.Default()
	if err := os.MkdirAll(layout.Inbox(), 0o770); err != nil {
		logger.Warn("could not create inbox", "err", err)
	}

	hub := &events.Hub{}
	cdpClient := cdp.New("")
	screen := updatescreen.New(cdpClient, strings.TrimSuffix(localserver.BaseURL, "/"), logger)
	imp := importer.New(importer.Deps{
		Layout:       layout,
		DeviceID:     cfg.DeviceID,
		AgentVersion: version.Version,
		Ring:         func() (*trust.Ring, error) { return trust.LoadRing(trust.DefaultPath) },
		Screen:       screen,
		Events:       hub,
		Logger:       logger,
	})
	keeper := clock.New(layout, logger)

	priv, keyErr := keystore.Load(cfg.KeyPath)
	if keyErr != nil {
		logger.Error("device key unreadable: serving installed content only (no sync, no remote commands)",
			"err", keyErr, "keyPath", cfg.KeyPath)
	}
	hour, minute := cfg.UpdateTime()
	var syncer *boardsync.Syncer
	sched := updates.New(updates.Deps{
		Layout: layout, Importer: imp, Hour: hour, Minute: minute, Logger: logger,
		Changed: func(info updates.Info) { hub.UpdatesChanged(info) },
		Check: func(ctx context.Context) error {
			if syncer == nil {
				return errors.New("this board cannot reach the release channels (device key unreadable)")
			}
			return syncer.CheckChannels(ctx)
		},
	})
	if keyErr == nil {
		syncer = boardsync.New(boardsync.Deps{
			Cfg: cfg, Key: priv, Layout: layout, Importer: imp, Updates: sched,
			AgentVersion: version.Version, Logger: logger,
		})
	}

	local := localserver.New(localserver.Deps{
		Layout: layout, DeviceID: cfg.DeviceID, AgentVersion: version.Version, Hub: hub,
		Sync: func() any {
			if syncer == nil {
				return map[string]any{"enabled": false, "lastError": "device key unreadable"}
			}
			return syncer.Status()
		},
		Weather: func() (json.RawMessage, bool) {
			if syncer == nil {
				return nil, false
			}
			return syncer.Weather()
		},
		UpdateActive: screen.Active,
		Updates:      sched.Info,
		Logger:       logger,
	})
	ln, err := net.Listen("tcp", localserver.ListenAddr)
	if err != nil {
		return err
	}
	goRun(wg, func() {
		if err := local.Serve(ctx, ln); err != nil {
			// Without the server the board shows nothing; let systemd restart
			// us rather than idle with the watchdog still happy.
			logger.Error("local server stopped", "err", err)
			os.Exit(1)
		}
	})

	// Only now can the kiosk find something at the URL it is given.
	if err := kioskurl.WriteLocal(kioskurl.DefaultOutPath); err != nil {
		logger.Warn("could not write kiosk url", "err", err)
	}
	_, _ = daemon.SdNotify(false, daemon.SdNotifyReady)
	_, _ = daemon.SdNotify(false, "STATUS=serving the board on "+localserver.ListenAddr)

	logInstalled(logger, layout, cfg)
	goRun(wg, func() { screen.Resume(ctx) })
	goRun(wg, func() { watchKiosk(ctx, logger, cdpClient) })
	goRun(wg, func() { imp.RunInbox(ctx) })
	goRun(wg, func() { keeper.Run(ctx) })
	goRun(wg, func() { sched.Run(ctx) })

	if cfg.ServicePort() {
		startUploadServer(ctx, logger, cfg, layout, imp, keeper, screen, wg)
	}

	if syncer != nil {
		goRun(wg, func() { syncer.Run(ctx) })
		startCommandChannel(ctx, logger, cfg, priv, safeMode, cdpClient, syncer, sched, wg)
	}
	return nil
}

func startUploadServer(ctx context.Context, logger *slog.Logger, cfg *config.Config, layout store.Layout,
	imp *importer.Importer, keeper *clock.Keeper, screen *updatescreen.Screen, wg *sync.WaitGroup) {
	ln, err := uploadserver.Listen()
	if err != nil {
		logger.Error("could not start the upload server; USB sticks still work", "addr", uploadserver.ListenAddr, "err", err)
		return
	}
	srv := uploadserver.New(uploadserver.Deps{
		Layout: layout, DeviceID: cfg.DeviceID, AgentVersion: version.Version, Importer: imp,
		ApplyClientTime: func(unix int64, b importer.Batch) uploadserver.ClockReport {
			r := keeper.ApplyClientTime(unix, b)
			return uploadserver.ClockReport{DriftSeconds: r.DriftSeconds, Adjusted: r.Adjusted, Note: r.Note}
		},
		RTCPresent:   keeper.RTCPresent,
		UpdateActive: screen.Active,
		Logger:       logger,
	})
	logger.Info("upload server listening", "addr", uploadserver.ListenAddr)
	goRun(wg, func() {
		if err := srv.Serve(ctx, ln); err != nil {
			logger.Error("upload server stopped", "err", err)
		}
	})
}

// startCommandChannel holds the backend WebSocket (telemetry and remote
// commands) open. config.refresh also asks the content syncer for a sync now;
// update.install_now checks the release channels and installs at once.
func startCommandChannel(ctx context.Context, logger *slog.Logger, cfg *config.Config, priv []byte,
	safeMode bool, cdpClient *cdp.Client, syncer *boardsync.Syncer, sched *updates.Scheduler, wg *sync.WaitGroup) {
	wsClient := wsclient.New(cfg, priv, logger, version.Version, safeMode)
	wsClient.SetPageProber(cdpClient)
	registry := commands.NewRegistry()
	registry.Register(&commands.ChromeReload{CDP: cdpClient})
	registry.Register(&commands.ChromeScreenshot{CDP: cdpClient})
	registry.Register(&commands.ConfigRefresh{CDP: cdpClient, Before: syncer.Trigger})
	registry.Register(&commands.KioskRestart{})
	registry.Register(&commands.SystemReboot{})
	registry.Register(&commands.LogsTail{})
	registry.Register(&commands.UpdateInstallNow{Run: sched.CheckAndInstall})
	logger.Info("command handlers registered", "kinds", registry.Kinds())
	wsClient.SetCommandHandler(commands.NewDispatcher(registry, logger).Handle)
	goRun(wg, func() { wsClient.Run(ctx) })
}

// watchKiosk navigates Chromium back to the board if it lands on its own
// error page (the server restarting under it, for instance).
func watchKiosk(ctx context.Context, logger *slog.Logger, c *cdp.Client) {
	t := time.NewTicker(kioskWatchInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		target, err := c.FirstPageTarget(cctx)
		if err == nil && strings.HasPrefix(target.URL, "chrome-error:") {
			logger.Info("kiosk is on an error page; returning it to the board")
			_ = c.Call(cctx, target, "Page.navigate", map[string]any{"url": localserver.BaseURL}, nil)
		}
		cancel()
	}
}

func logInstalled(logger *slog.Logger, l store.Layout, cfg *config.Config) {
	st := localserver.BuildStatus(l, cfg.DeviceID, version.Version, time.Now())
	attrs := []any{"deviceId", cfg.DeviceID, "arch", runtime.GOARCH, "servicePort", cfg.ServicePort()}
	if st.App != nil {
		attrs = append(attrs, "app", st.App.Version)
	} else {
		attrs = append(attrs, "app", "none")
	}
	if st.Content != nil {
		attrs = append(attrs, "content", st.Content.FirstDay+".."+st.Content.LastDay, "staleDays", *st.StaleDays)
	} else {
		attrs = append(attrs, "content", "none")
	}
	if st.Error != "" {
		attrs = append(attrs, "error", st.Error)
	}
	logger.Info("board state", attrs...)
}

func goRun(wg *sync.WaitGroup, f func()) {
	wg.Add(1)
	go func() {
		defer wg.Done()
		f()
	}()
}
