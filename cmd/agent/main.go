package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/coreos/go-systemd/v22/daemon"

	"github.com/LensBridge/agent/internal/cdp"
	"github.com/LensBridge/agent/internal/commands"
	"github.com/LensBridge/agent/internal/config"
	"github.com/LensBridge/agent/internal/enroll"
	"github.com/LensBridge/agent/internal/keystore"
	"github.com/LensBridge/agent/internal/kioskurl"
	"github.com/LensBridge/agent/internal/netinfo"
	"github.com/LensBridge/agent/internal/safemode"
	"github.com/LensBridge/agent/internal/splash"
	"github.com/LensBridge/agent/internal/version"
	"github.com/LensBridge/agent/internal/wsclient"
)

const (
	defaultConfigPath = "/etc/musallahboard/agent.toml"
	defaultStateDir   = "/var/lib/musallahboard"

	// stableRunDuration is how long the agent must run cleanly before its
	// crash counter is reset. Shorter than systemd's StartLimitIntervalSec
	// so that a flapping agent never gets credit for "stable."
	stableRunDuration = 5 * time.Minute

	// enrollPollInterval is how often an unenrolled agent re-reads its network
	// state, repaints the kiosk splash, and checks whether a config appeared.
	// Short enough that a board plugged into ethernet shows its address while
	// the installer is still standing in front of it, and that enrollment
	// takes effect without anyone restarting the service by hand.
	enrollPollInterval = 5 * time.Second
)

func main() {
	if len(os.Args) < 2 {
		runDaemon()
		return
	}

	switch os.Args[1] {
	case "run":
		runDaemon()
	case "enroll":
		runEnroll(os.Args[2:])
	case "bundle":
		runBundle(os.Args[2:])
	case "status":
		runStatus(os.Args[2:])
	case "mode":
		runMode(os.Args[2:])
	case "app":
		runApp(os.Args[2:])
	case "gate":
		runGate(os.Args[2:])
	case "version", "-v", "--version":
		fmt.Printf("musallahboard-agent %s\n", version.Version)
	case "-h", "--help", "help":
		printHelp()
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n\n", os.Args[1])
		printHelp()
		os.Exit(2)
	}
}

func printHelp() {
	fmt.Print(`musallahboard-agent — device agent for MusallahBoard kiosk Pis

Usage:
  musallahboard-agent run                          Run the daemon (default)
  musallahboard-agent enroll --token=X --backend=Y Register this device
  musallahboard-agent status [--json]              Show mode, content bundle and clock
  musallahboard-agent version                      Print version

Offline mode (see docs/offline.md; these need sudo):
  musallahboard-agent bundle install <bundle.zip>  Check and install a content bundle
  musallahboard-agent mode online|offline          Switch mode (restarts the agent)
  musallahboard-agent app install <dir|tar.gz>     Install a board app build for offline use
  musallahboard-agent gate <request>               Restricted entry point for the push account:
                                                   status [--json] | clock | clock-set <seconds> |
                                                   bundle-install | app-install (file on stdin)

Enroll flags:
  --token     One-time enrollment token from the admin portal
  --backend   Backend base URL (e.g. https://backend.utmmsa.ca)
  --config    Path to write agent config (default: ` + defaultConfigPath + `)
`)
}

func runEnroll(args []string) {
	fs := flag.NewFlagSet("enroll", flag.ExitOnError)
	token := fs.String("token", "", "one-time enrollment token")
	backend := fs.String("backend", "", "backend base URL")
	configPath := fs.String("config", defaultConfigPath, "path to write config")
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}
	if *token == "" || *backend == "" {
		fmt.Fprintln(os.Stderr, "error: --token and --backend are required")
		os.Exit(2)
	}

	logger := newLogger()
	if err := enroll.Run(context.Background(), logger, enroll.Params{
		Token:        *token,
		BackendURL:   *backend,
		ConfigPath:   *configPath,
		AgentVersion: version.Version,
	}); err != nil {
		logger.Error("enrollment failed", "err", err)
		os.Exit(1)
	}
}

func runDaemon() {
	logger := newLogger()
	logger.Info("agent starting", "version", version.Version)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	var wg sync.WaitGroup

	// Started before the config gate: awaitEnrollment below can hold the
	// process for days, and systemd's WatchdogSec does not pause for it.
	wg.Add(1)
	go func() {
		defer wg.Done()
		watchdogLoop(ctx)
	}()

	cfg, err := config.Load(defaultConfigPath)
	if err != nil {
		logger.Info("not enrolled yet — entering pre-enrollment mode", "reason", err)
		cfg = awaitEnrollment(ctx, logger)
		if cfg == nil { // context cancelled while waiting
			wg.Wait()
			logger.Info("shutdown complete")
			return
		}
		logger.Info("enrollment detected", "deviceId", cfg.DeviceID)
	}

	sm := safemode.New(defaultStateDir)
	safeMode := sm.OnStartup()
	if safeMode {
		logger.Warn("agent starting in safe mode (repeated crashes detected)")
	}

	if cfg.Mode == config.ModeOffline {
		// No backend WebSocket, telemetry or remote commands: offline mode
		// makes no network calls at all. See docs/offline.md, Contract 2.
		if err := startOffline(ctx, logger, cfg, &wg); err != nil {
			logger.Error("could not start offline mode", "err", err)
			os.Exit(1)
		}
	} else {
		startOnline(ctx, logger, cfg, safeMode, &wg)
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		select {
		case <-time.After(stableRunDuration):
			sm.OnSuccessfulRun()
			logger.Info("crash counter reset (agent stable)")
		case <-ctx.Done():
		}
	}()

	<-ctx.Done()
	logger.Info("shutdown signal received")
	_, _ = daemon.SdNotify(false, daemon.SdNotifyStopping)
	wg.Wait()
	logger.Info("shutdown complete")
}

// startOnline is the original run mode: load the device key, point the kiosk
// at the provisioned board URL, and hold the backend WebSocket open with the
// command dispatcher behind it. It returns once everything is started; the
// goroutines it adds to wg stop when ctx is cancelled.
func startOnline(ctx context.Context, logger *slog.Logger, cfg *config.Config, safeMode bool, wg *sync.WaitGroup) {
	priv, err := keystore.Load(cfg.KeyPath)
	if err != nil {
		logger.Error("failed to load device key", "err", err, "keyPath", cfg.KeyPath)
		os.Exit(1)
	}
	logger.Info("agent identity loaded",
		"deviceId", cfg.DeviceID,
		"websocketUrl", cfg.WebSocketURL,
		"safeMode", safeMode,
	)

	// Re-compose the kiosk URL on every startup. Covers the case where enroll
	// ran before the operator provisioned the base board URL, and picks up a
	// changed base URL after an agent restart. The kiosk unit waits on this
	// file, so a successful write here is what unblocks the board on first boot.
	if err := kioskurl.Write(kioskurl.DefaultBoardURLPath, kioskurl.DefaultOutPath, cfg.DeviceID); err != nil {
		logger.Warn("could not compose kiosk url (kiosk will wait)", "err", err)
	} else {
		logger.Info("kiosk url written", "path", kioskurl.DefaultOutPath)
	}

	wsClient := wsclient.New(cfg, priv, logger, version.Version, safeMode)

	cdpClient := cdp.New("")
	// Heartbeats ask the browser what it's showing rather than trusting the
	// kiosk unit's state, which Restart=always keeps "active" through a crash
	// loop or a stuck splash.
	wsClient.SetPageProber(cdpClient)
	registry := commands.NewRegistry()
	registry.Register(&commands.ChromeReload{CDP: cdpClient})
	registry.Register(&commands.ChromeScreenshot{CDP: cdpClient})
	registry.Register(&commands.ConfigRefresh{CDP: cdpClient, DeviceID: cfg.DeviceID})
	registry.Register(&commands.KioskRestart{})
	registry.Register(&commands.SystemReboot{})
	registry.Register(&commands.LogsTail{})
	logger.Info("command handlers registered", "kinds", registry.Kinds())

	dispatcher := commands.NewDispatcher(registry, logger)
	wsClient.SetCommandHandler(dispatcher.Handle)

	// Re-sent, and harmless, when awaitEnrollment already reported ready.
	_, _ = daemon.SdNotify(false, daemon.SdNotifyReady)
	_, _ = daemon.SdNotify(false, "STATUS=enrolled")

	wg.Add(1)
	go func() {
		defer wg.Done()
		wsClient.Run(ctx)
	}()
}

// awaitEnrollment blocks until a usable config appears at defaultConfigPath,
// painting this device's IP address onto the kiosk splash while it waits.
// Returns nil if ctx is cancelled first.
//
// Before this existed the daemon exited(1) on a missing config, so a freshly
// imaged board burned through systemd's StartLimitBurst within a minute and
// then sat dead behind a static "waiting for enrollment" card. Finding the box
// to SSH into meant plugging in a keyboard or trawling the router's DHCP
// leases. The screen is the one output an appliance always has, so an
// unenrolled agent now stays up and uses it: no network client, no command
// dispatcher, no key — just a poll loop and a CDP push.
//
// Waiting is a legitimate running state rather than a slow start, so we report
// READY to systemd up front; otherwise Type=notify would hold the unit in
// "activating" and kill it at TimeoutStartSec.
func awaitEnrollment(ctx context.Context, logger *slog.Logger) *config.Config {
	_, _ = daemon.SdNotify(false, daemon.SdNotifyReady)
	_, _ = daemon.SdNotify(false, "STATUS=waiting for enrollment")

	cdpClient := cdp.New("")

	var last netinfo.Info
	var reported bool

	t := time.NewTicker(enrollPollInterval)
	defer t.Stop()
	for {
		info := netinfo.Collect(ctx)
		if !reported || !info.Equal(last) {
			logger.Info("device network address",
				"ipv4", info.IPv4,
				"ssid", info.SSID,
				"hostname", info.Hostname,
			)
			last, reported = info, true
		}

		// Chromium may not be up yet, or may be mid-restart, and once enrolled
		// the board replaces the splash entirely — a push that finds no hook is
		// the normal case, not an error worth a log line every tick.
		switch err := splash.PushNetInfo(ctx, cdpClient, info); {
		case err == nil, errors.Is(err, splash.ErrNoSplash):
		default:
			logger.Debug("could not update enrollment splash", "err", err)
		}

		if cfg, err := config.Load(defaultConfigPath); err == nil {
			return cfg
		}

		select {
		case <-t.C:
		case <-ctx.Done():
			return nil
		}
	}
}

// watchdogLoop pings systemd's WATCHDOG=1 at half the configured interval.
// systemd kills (and restarts) the agent if it stops pinging — useful when
// a goroutine deadlocks but the process is technically still alive.
func watchdogLoop(ctx context.Context) {
	interval, err := daemon.SdWatchdogEnabled(false)
	if err != nil || interval == 0 {
		return
	}
	t := time.NewTicker(interval / 2)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			_, _ = daemon.SdNotify(false, daemon.SdNotifyWatchdog)
		case <-ctx.Done():
			return
		}
	}
}

func newLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))
}
