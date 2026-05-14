package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/coreos/go-systemd/v22/daemon"

	"github.com/utmmsa/musallahboard-agent/internal/cdp"
	"github.com/utmmsa/musallahboard-agent/internal/commands"
	"github.com/utmmsa/musallahboard-agent/internal/config"
	"github.com/utmmsa/musallahboard-agent/internal/enroll"
	"github.com/utmmsa/musallahboard-agent/internal/keystore"
	"github.com/utmmsa/musallahboard-agent/internal/safemode"
	"github.com/utmmsa/musallahboard-agent/internal/version"
	"github.com/utmmsa/musallahboard-agent/internal/wsclient"
)

const (
	defaultConfigPath = "/etc/musallahboard/agent.toml"
	defaultStateDir   = "/var/lib/musallahboard"

	// stableRunDuration is how long the agent must run cleanly before its
	// crash counter is reset. Shorter than systemd's StartLimitIntervalSec
	// so that a flapping agent never gets credit for "stable."
	stableRunDuration = 5 * time.Minute
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
  musallahboard-agent version                      Print version

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

	cfg, err := config.Load(defaultConfigPath)
	if err != nil {
		logger.Error("failed to load config (run `musallahboard-agent enroll` first)", "err", err)
		os.Exit(1)
	}

	sm := safemode.New(defaultStateDir)
	safeMode := sm.OnStartup()
	if safeMode {
		logger.Warn("agent starting in safe mode (repeated crashes detected)")
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

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

	wsClient := wsclient.New(cfg, priv, logger, version.Version, safeMode)

	cdpClient := cdp.New("")
	registry := commands.NewRegistry()
	registry.Register(&commands.ChromeReload{CDP: cdpClient})
	registry.Register(&commands.ChromeScreenshot{CDP: cdpClient})
	registry.Register(&commands.ConfigRefresh{CDP: cdpClient})
	registry.Register(&commands.KioskRestart{})
	registry.Register(&commands.SystemReboot{})
	registry.Register(&commands.LogsTail{})
	logger.Info("command handlers registered", "kinds", registry.Kinds())

	dispatcher := commands.NewDispatcher(registry, logger)
	wsClient.SetCommandHandler(dispatcher.Handle)

	_, _ = daemon.SdNotify(false, daemon.SdNotifyReady)

	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		wsClient.Run(ctx)
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		watchdogLoop(ctx)
	}()

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
