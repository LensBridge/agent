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
	"sync/atomic"
	"time"

	"github.com/coreos/go-systemd/v22/daemon"

	"github.com/LensBridge/agent/internal/agentupdate"
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
	"github.com/LensBridge/agent/internal/notice"
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
func startBoard(ctx context.Context, logger *slog.Logger, cfg *config.Config, wg *sync.WaitGroup) error {
	layout := store.Default()
	if err := os.MkdirAll(layout.Inbox(), 0o770); err != nil {
		logger.Warn("could not create inbox", "err", err)
	}

	hub := &events.Hub{}
	cdpClient := cdp.New("")
	screen := updatescreen.New(cdpClient, strings.TrimSuffix(localserver.BaseURL, "/"), logger)
	var reporter *agentupdate.Reporter
	imp := importer.New(importer.Deps{
		Layout:       layout,
		DeviceID:     cfg.DeviceID,
		AgentVersion: version.Version,
		Ring:         func() (*trust.Ring, error) { return trust.LoadRing(trust.DefaultPath) },
		Screen:       screen,
		AgentStaged:  func(n notice.Notice) { reporter.Staged(n) },
		Events:       hub,
		Logger:       logger,
	})
	reporter = agentupdate.New(agentupdate.Deps{
		Layout: layout, Version: version.Version, Screen: screen,
		Banner: hub.Notice, Resume: imp.AgentNotReplaced, Logger: logger,
	})
	keeper := clock.New(layout, logger)

	priv, keyErr := keystore.Load(cfg.KeyPath)
	if keyErr != nil {
		logger.Error("device key unreadable: serving installed content only (no sync, no remote commands)",
			"err", keyErr, "keyPath", cfg.KeyPath)
	}
	// Nothing that talks to the backend is built from an insecure config:
	// content sync, its refresh channel and the command WebSocket all take
	// their addresses from it (docs/architecture.md, section 11).
	transportErr := backendTransport(logger, cfg)
	offline := func() string {
		if transportErr != nil {
			return "not talking to LensBridge: " + transportErr.Error()
		}
		return "device key unreadable"
	}
	hour, minute := cfg.UpdateTime()
	var syncer *boardsync.Syncer
	sched := updates.New(updates.Deps{
		Layout: layout, Importer: imp, Hour: hour, Minute: minute, AutoUpdate: cfg.AutoUpdate(), Logger: logger,
		Changed: func(info updates.Info) { hub.UpdatesChanged(info) },
		Check: func(ctx context.Context) error {
			if syncer == nil {
				return errors.New("this board cannot reach the release channels (" + offline() + ")")
			}
			return syncer.CheckChannels(ctx)
		},
	})
	if keyErr == nil && transportErr == nil {
		syncer = boardsync.New(boardsync.Deps{
			Cfg: cfg, Key: priv, Layout: layout, Importer: imp, Updates: sched,
			AgentVersion: version.Version, Logger: logger,
		})
	}

	// The backend connection starts after the local server; its state is
	// read through this once it exists.
	var backend atomic.Pointer[wsclient.Client]
	local := localserver.New(localserver.Deps{
		Layout: layout, DeviceID: cfg.DeviceID, AgentVersion: version.Version, Hub: hub,
		Sync: func() any {
			if syncer == nil {
				// Enabled as configured, so the board app and `status` show
				// the reason as a sync error rather than "sync off".
				return map[string]any{"enabled": transportErr != nil && cfg.ContentSync(), "lastError": offline()}
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
		Clock:        keeper.Info,
		Backend: func() any {
			if ws := backend.Load(); ws != nil {
				s := ws.State()
				return &s
			}
			if transportErr != nil {
				return map[string]any{"connected": false, "lastError": offline()}
			}
			return nil
		},
		ServicePort: cfg.ServicePort(),
		USBImport:   cfg.USBImport(),
		Logger:      logger,
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
	if transportErr != nil {
		_, _ = daemon.SdNotify(false, "STATUS=serving installed content only; refusing the backend: "+transportErr.Error())
	} else {
		_, _ = daemon.SdNotify(false, "STATUS=serving the board on "+localserver.ListenAddr)
	}

	logInstalled(logger, layout, cfg)
	goRun(wg, func() { watchKiosk(ctx, logger, cdpClient, screen.Active) })
	goRun(wg, func() {
		// First say how an agent update that restarted us went: the rest of
		// its batch may be waiting in the inbox, and belongs on the same
		// screen.
		reporter.Startup(ctx)
		imp.RunInbox(ctx)
	})
	goRun(wg, func() { keeper.Run(ctx) })
	goRun(wg, func() { sched.Run(ctx) })

	if cfg.ServicePort() {
		startUploadServer(ctx, logger, cfg, layout, imp, keeper, screen, wg)
	}

	if syncer != nil {
		goRun(wg, func() { syncer.Run(ctx) })
		ws := startCommandChannel(ctx, logger, cfg, priv, cdpClient, syncer, sched,
			func() any { return local.Status().Board() }, wg)
		backend.Store(ws)
	}
	return nil
}

// backendTransport checks that the config may be used to talk to the
// backend (config.CheckTransport) and says so loudly at every start when it
// may not, or when it may only because of the insecure_dev opt-in. A board
// enrolled over plain http before https was required lands here: it keeps
// showing what it has, and the log, `systemctl status`, `musallahboard-agent
// status` and the board app's sync error all carry the reason and the fix.
func backendTransport(logger *slog.Logger, cfg *config.Config) error {
	if err := cfg.CheckTransport(); err != nil {
		logger.Error("REFUSING TO TALK TO THE BACKEND: this board's config is not safe to use. "+
			"Serving installed content only: no content sync, no release channel checks, no remote commands. "+
			"Re-enroll it over https (sudo musallahboard-agent enroll --force --token=... --backend=https://...), "+
			"or, for a development board only, add insecure_dev = true to "+defaultConfigPath+" and restart the agent.",
			"err", err, "backendUrl", cfg.BackendURL, "websocketUrl", cfg.WebSocketURL)
		return err
	}
	if cfg.InsecureDev() {
		logger.Warn("INSECURE DEVELOPMENT BOARD: insecure_dev = true, so this board accepts a plaintext backend. "+
			"Anyone on its network can impersonate the backend: send it content, reboot it, read its screen and logs. "+
			"Never deploy a board like this.",
			"backendUrl", cfg.BackendURL, "websocketUrl", cfg.WebSocketURL)
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

// startCommandChannel holds the backend WebSocket (telemetry and remote
// commands) open. config.refresh also asks the content syncer for a sync now;
// update.install_now checks the release channels and installs at once.
func startCommandChannel(ctx context.Context, logger *slog.Logger, cfg *config.Config, priv []byte,
	cdpClient *cdp.Client, syncer *boardsync.Syncer, sched *updates.Scheduler,
	boardReport func() any, wg *sync.WaitGroup) *wsclient.Client {
	wsClient := wsclient.New(cfg, priv, logger, version.Version)
	wsClient.SetPageProber(cdpClient)
	wsClient.SetBoardReport(boardReport)
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
	return wsClient
}

// Kiosk self-healing (watchKiosk). The page is probed every
// kioskWatchInterval. A crashed renderer is certain after one probe; a page
// that does not answer may be busy (a large poster decoding on a Pi), so it
// gets kioskHungThreshold probes. Then the watcher reloads the page; if the
// page is still dead after that, it restarts the kiosk unit. Restarts back off
// from kioskRestartMinGap to kioskRestartMaxGap, and the backoff only starts
// over after the page has been healthy for kioskRestartResetAfter, so a page
// that never comes back costs at most one reload and one restart an hour.
const (
	kioskProbeTimeout      = 5 * time.Second
	kioskCrashThreshold    = 1
	kioskHungThreshold     = 3
	kioskRestartMinGap     = 5 * time.Minute
	kioskRestartMaxGap     = time.Hour
	kioskRestartResetAfter = time.Hour
)

// watchKiosk keeps the kiosk on the board: it navigates Chromium back if it
// lands on its own error page (the server restarting under it, for instance)
// and heals a crashed or hung page (kioskHealer).
func watchKiosk(ctx context.Context, logger *slog.Logger, c *cdp.Client, updateActive func() bool) {
	h := newKioskHealer(logger, c, updateActive)
	t := time.NewTicker(kioskWatchInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		h.tick(ctx)
	}
}

// kioskHealer is the watcher's state between ticks. Its browser and systemd
// operations are fields so the escalation can be tested without either.
type kioskHealer struct {
	log          *slog.Logger
	now          func() time.Time
	updateActive func() bool
	target       func(context.Context) (cdp.Target, error)
	navigate     func(context.Context, cdp.Target, string) error
	health       func(context.Context) error
	reload       func(context.Context) error
	restart      func(context.Context) error

	fails        int       // consecutive dead-page probes
	reloaded     bool      // reloaded since the last restart (or since healthy)
	restarts     int       // restarts since the backoff last started over
	nextRestart  time.Time // no restart before this
	healthySince time.Time // zero while the page is dead
	waitLogged   bool
}

func newKioskHealer(logger *slog.Logger, c *cdp.Client, updateActive func() bool) *kioskHealer {
	if updateActive == nil {
		updateActive = func() bool { return false }
	}
	return &kioskHealer{
		log:          logger,
		now:          time.Now,
		updateActive: updateActive,
		target:       c.FirstPageTarget,
		navigate: func(ctx context.Context, t cdp.Target, url string) error {
			return c.Call(ctx, t, "Page.navigate", map[string]any{"url": url}, nil)
		},
		health: c.PageHealth,
		reload: c.PageReload,
		restart: func(ctx context.Context) error {
			// The same privileged path as the kiosk.restart remote command
			// (sudo -n systemctl restart, allowed by the sudoers drop-in).
			_, err := (&commands.KioskRestart{}).Execute(ctx, nil, func(string, string, *int) {})
			return err
		},
	}
}

func (h *kioskHealer) tick(ctx context.Context) {
	// The update screen is driven over CDP by the importer and may be
	// waiting on an agent restart; nothing here may navigate under it.
	if h.updateActive() {
		h.fails = 0
		return
	}

	cctx, cancel := context.WithTimeout(ctx, kioskProbeTimeout)
	target, err := h.target(cctx)
	cancel()
	if err == nil && strings.HasPrefix(target.URL, "chrome-error:") {
		h.log.Info("kiosk is on an error page; returning it to the board")
		cctx, cancel := context.WithTimeout(ctx, kioskProbeTimeout)
		_ = h.navigate(cctx, target, localserver.BaseURL)
		cancel()
		return
	}
	if err != nil {
		// No browser, or no page yet: starting, or restarting (perhaps
		// because we asked). systemd's Restart=always owns that case.
		return
	}

	cctx, cancel = context.WithTimeout(ctx, kioskProbeTimeout)
	err = h.health(cctx)
	cancel()
	if ctx.Err() != nil {
		return
	}

	threshold := 0
	switch {
	case errors.Is(err, cdp.ErrPageCrashed):
		threshold = kioskCrashThreshold
	case errors.Is(err, cdp.ErrPageUnresponsive):
		threshold = kioskHungThreshold
	case errors.Is(err, cdp.ErrDebuggerUnreachable), errors.Is(err, cdp.ErrNoPageTarget):
		// The browser went away between the two calls: not the page's fault,
		// and not proof it recovered either.
		return
	default:
		// Answered, or between documents, or the hook threw: the page is
		// alive. A navigation in flight is not a failure.
		h.healthy()
		return
	}

	h.healthySince = time.Time{}
	h.fails++
	if h.fails < threshold {
		h.log.Warn("kiosk page is not answering", "err", err, "failures", h.fails, "url", target.URL)
		return
	}
	h.heal(ctx, err, target.URL)
}

func (h *kioskHealer) healthy() {
	now := h.now()
	if h.fails > 0 || h.reloaded {
		h.log.Info("kiosk page is answering again")
	}
	h.fails, h.reloaded, h.waitLogged = 0, false, false
	if h.healthySince.IsZero() {
		h.healthySince = now
	}
	if h.restarts > 0 && now.Sub(h.healthySince) >= kioskRestartResetAfter {
		h.restarts, h.nextRestart = 0, time.Time{}
	}
}

// heal takes the next step for a dead page: a reload, at most once until the
// page answers again or the kiosk restarts; then a restart, at most once per
// backoff gap. A page that recovers each time it is reloaded is reloaded each
// time it dies, which is what it needs; a page that never recovers gets one
// reload and one restart per gap.
func (h *kioskHealer) heal(ctx context.Context, cause error, url string) {
	if !h.reloaded {
		h.fails, h.reloaded = 0, true
		h.log.Warn("kiosk page is dead; reloading it", "err", cause, "url", url)
		cctx, cancel := context.WithTimeout(ctx, kioskProbeTimeout)
		if err := h.reload(cctx); err != nil {
			h.log.Warn("could not reload the kiosk page", "err", err)
		}
		cancel()
		return
	}

	now := h.now()
	if now.Before(h.nextRestart) {
		if !h.waitLogged {
			h.log.Error("kiosk page is still dead; waiting before restarting the kiosk again so a broken page cannot restart it in a loop",
				"err", cause, "url", url, "nextAttempt", h.nextRestart.Format(time.RFC3339))
			h.waitLogged = true
		}
		return
	}

	h.fails, h.reloaded, h.waitLogged = 0, false, false
	gap := min(kioskRestartMinGap<<min(h.restarts, 8), kioskRestartMaxGap)
	h.restarts++
	h.nextRestart = now.Add(gap)
	h.log.Error("kiosk page is still dead after a reload; restarting the kiosk",
		"err", cause, "url", url, "restarts", h.restarts, "nextAllowed", h.nextRestart.Format(time.RFC3339))
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	if err := h.restart(cctx); err != nil {
		h.log.Error("could not restart the kiosk", "err", err)
	}
	cancel()
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
