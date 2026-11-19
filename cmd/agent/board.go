package main

import (
	"context"
	"log/slog"
	"net"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-systemd/v22/daemon"

	"github.com/LensBridge/agent/internal/core/clock"
	"github.com/LensBridge/agent/internal/core/config"
	"github.com/LensBridge/agent/internal/core/events"
	"github.com/LensBridge/agent/internal/core/importer"
	"github.com/LensBridge/agent/internal/core/kiosk"
	"github.com/LensBridge/agent/internal/core/kioskurl"
	"github.com/LensBridge/agent/internal/core/localserver"
	"github.com/LensBridge/agent/internal/core/notice"
	"github.com/LensBridge/agent/internal/core/selfupdate"
	"github.com/LensBridge/agent/internal/core/store"
	"github.com/LensBridge/agent/internal/core/trust"
	"github.com/LensBridge/agent/internal/core/updates"
	"github.com/LensBridge/agent/internal/core/uploadserver"
	"github.com/LensBridge/agent/internal/online"
	"github.com/LensBridge/agent/internal/version"
)

// startBoard starts everything a board runs (docs/architecture.md, section
// 3), provisioned or not. The local server comes first and is the only part
// whose failure is fatal: without it the board has nothing to show. The
// online layer runs beside the core when agent.toml asks for it, and degrades
// to "not running" on its own, because a board that cannot reach LensBridge
// must still show what it has.
func startBoard(ctx context.Context, logger *slog.Logger, cfg *config.Live, wg *sync.WaitGroup) error {
	layout := store.Default()
	if err := os.MkdirAll(layout.Inbox(), 0o770); err != nil {
		logger.Warn("could not create inbox", "err", err)
	}
	c := cfg.Get()

	hub := &events.Hub{}
	cdpClient := kiosk.NewClient("")
	screen := kiosk.NewScreen(cdpClient, strings.TrimSuffix(localserver.BaseURL, "/"), logger)
	var reporter *selfupdate.Reporter
	imp := importer.New(importer.Deps{
		Layout:       layout,
		Config:       cfg,
		AgentVersion: version.Version,
		Ring:         func() (*trust.Ring, error) { return trust.LoadRing(trust.DefaultPath) },
		Screen:       screen,
		AgentStaged:  func(n notice.Notice) { reporter.Staged(n) },
		Events:       hub,
		Logger:       logger,
	})
	reporter = selfupdate.NewReporter(selfupdate.ReporterDeps{
		Layout: layout, Version: version.Version, Screen: screen,
		Banner: hub.Notice, Resume: imp.AgentNotReplaced, Logger: logger,
	})
	keeper := clock.New(layout, logger)

	hour, minute := c.UpdateTime()
	// The release channels are public files: any board with a network
	// follows them, provisioned and enrolled or not.
	sched := updates.New(updates.Deps{
		Layout: layout, Importer: imp, Hour: hour, Minute: minute, AutoUpdate: c.AutoUpdate(), Logger: logger,
		AppChannel: c.AppChannelURL(), AgentChannel: c.AgentChannelURL(runtime.GOARCH),
		AgentVersion: version.Version,
		Changed:      func(info updates.Info) { hub.UpdatesChanged(info) },
	})

	var local *localserver.Server
	layer := online.New(online.Deps{
		Config: cfg, Layout: layout, Importer: imp, Updates: sched, Kiosk: cdpClient,
		BoardReport:  func() any { return local.Status().Board() },
		AgentVersion: version.Version, Logger: logger,
	})
	local = localserver.New(localserver.Deps{
		Layout: layout, Config: cfg, AgentVersion: version.Version, Hub: hub,
		Sync:         layer.Sync,
		Weather:      layer.Weather,
		Backend:      layer.Backend,
		UpdateActive: screen.Active,
		Updates:      sched.Info,
		Clock:        keeper.Info,
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

	logInstalled(logger, layout, c)
	goRun(wg, func() { kiosk.Watch(ctx, logger, cdpClient, localserver.BaseURL, screen.Active) })
	goRun(wg, func() {
		// First say how an agent update that restarted us went: the rest of
		// its batch may be waiting in the inbox, and belongs on the same
		// screen.
		reporter.Startup(ctx)
		imp.RunInbox(ctx)
	})
	goRun(wg, func() { keeper.Run(ctx) })
	goRun(wg, func() { sched.Run(ctx) })
	goRun(wg, func() { layer.Run(ctx) })

	if c.ServicePort() {
		startUploadServer(ctx, logger, cfg, layout, imp, keeper, screen, wg)
	}
	return nil
}

func startUploadServer(ctx context.Context, logger *slog.Logger, cfg *config.Live, layout store.Layout,
	imp *importer.Importer, keeper *clock.Keeper, screen *kiosk.Screen, wg *sync.WaitGroup) {
	ln, err := uploadserver.Listen()
	if err != nil {
		logger.Error("could not start the upload server; USB sticks still work", "addr", uploadserver.ListenAddr, "err", err)
		return
	}
	srv := uploadserver.New(uploadserver.Deps{
		Layout:       layout,
		DeviceID:     func() string { return cfg.Get().DeviceID },
		AgentVersion: version.Version, Importer: imp,
		ApplyClientTime: func(unix int64, b importer.Batch) uploadserver.ClockReport {
			r := keeper.ApplyClientTime(unix, b)
			return uploadserver.ClockReport{DriftSeconds: r.DriftSeconds, Adjusted: r.Adjusted, Note: r.Note}
		},
		RTCPresent:   keeper.RTCPresent,
		ClockInfo:    keeper.Info,
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
