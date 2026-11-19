package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/coreos/go-systemd/v22/daemon"

	"github.com/LensBridge/agent/internal/core/config"
	"github.com/LensBridge/agent/internal/core/kiosk"
	"github.com/LensBridge/agent/internal/core/localserver"
	"github.com/LensBridge/agent/internal/core/netinfo"
	"github.com/LensBridge/agent/internal/core/splash"
	"github.com/LensBridge/agent/internal/online/enroll"
	"github.com/LensBridge/agent/internal/version"
)

const (
	defaultConfigPath = "/etc/musallahboard/agent.toml"

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
	case "status":
		runStatus(os.Args[2:])
	case "import":
		runImport(os.Args[2:])
	case "update":
		runUpdate(os.Args[2:])
	case "service-port":
		runServicePort(os.Args[2:])
	case "trust":
		runTrust(os.Args[2:])
	case "selfupdate":
		runSelfUpdate(os.Args[2:])
	case "usb-import":
		runUSBImport(os.Args[2:])
	case "splash":
		runSplash(os.Args[2:])
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
	fmt.Print(`musallahboard-agent: device agent for MusallahBoard kiosk Pis (docs/architecture.md)

Usage:
  musallahboard-agent run                          Run the daemon (default)
  musallahboard-agent enroll --token=X --backend=Y Register this device
  musallahboard-agent status [--json]              Show versions, content, sync, trust and clock
  musallahboard-agent version                      Print version

Administration (need sudo):
  musallahboard-agent import <file.mbu>... | -     Install signed update packages
  musallahboard-agent update now                   Check for a new app or agent and install it now
  musallahboard-agent service-port on|off          eth0 service port + upload server at http://10.77.0.1/
  musallahboard-agent trust show|add|remove|fetch  Keys this board accepts packages from

Run by systemd (root):
  musallahboard-agent selfupdate apply             Install a staged agent update, with rollback
  musallahboard-agent usb-import <device>          Copy update packages from a USB stick
  musallahboard-agent splash install [path]        Install the enrollment splash page

Enroll flags:
  --token     One-time enrollment token from the admin portal
  --backend   Backend base URL (e.g. https://backend.utmmsa.ca)
  --config    Path to write agent config (default: ` + defaultConfigPath + `)
  --force     Enroll a board that is already enrolled (it gets a new identity)
  --insecure-dev
              Development boards only: allow an http:// backend and ws:// socket
              (saved in the config as insecure_dev = true; never on a deployed board)
`)
}

func runEnroll(args []string) {
	fs := flag.NewFlagSet("enroll", flag.ExitOnError)
	token := fs.String("token", "", "one-time enrollment token")
	backend := fs.String("backend", "", "backend base URL")
	configPath := fs.String("config", defaultConfigPath, "path to write config")
	force := fs.Bool("force", false, "enroll again, replacing this board's identity")
	insecureDev := fs.Bool("insecure-dev", false, "development only: allow a plaintext http:// backend")
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}
	if *token == "" || *backend == "" {
		fmt.Fprintln(os.Stderr, "usage: sudo musallahboard-agent enroll --token=<token> --backend=<url>")
		fmt.Fprintln(os.Stderr, "Copy the whole command from the admin portal: Devices, Enroll a board.")
		os.Exit(2)
	}
	if err := config.CheckBackendURL(*backend, *insecureDev); err != nil {
		fail("%v", err)
	}
	requireRoot("enroll")

	// Enrolling again gives the board a new identity; the old device stays
	// in the portal, orphaned, until someone revokes it. Only on purpose.
	if old, err := config.Load(*configPath); err == nil && !*force {
		fmt.Fprintf(os.Stderr, "This board is already enrolled (device %s).\n", old.DeviceID)
		fmt.Fprintln(os.Stderr, "Enrolling again gives it a new identity; revoke the old device in the portal afterwards.")
		fmt.Fprintln(os.Stderr, "To go ahead, run the same command with --force.")
		os.Exit(1)
	}

	fmt.Println("Enrolling this board with", strings.TrimRight(*backend, "/"), "...")
	if err := enroll.Run(context.Background(), slog.New(plainHandler{os.Stdout}), enroll.Params{
		Token:        *token,
		BackendURL:   *backend,
		ConfigPath:   *configPath,
		AgentVersion: version.Version,
		InsecureDev:  *insecureDev,
	}); err != nil {
		fmt.Fprintf(os.Stderr, "Enrollment failed: %v\n", err)
		fmt.Fprintln(os.Stderr, "Tokens can be used once and expire; if this one was used or has expired, issue a new one in the portal.")
		os.Exit(1)
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		fail("enrolled, but the config it wrote cannot be read: %v", err)
	}
	fmt.Printf("Enrolled as device %s.\n", cfg.DeviceID)
	fmt.Println("The board switches from the enrollment screen to MusallahBoard within a few seconds, then")
	fmt.Println("downloads its board app and content. Check on it with: musallahboard-agent status")
}

func runDaemon() {
	logger := newLogger()
	logger.Info("agent starting", "version", version.Version)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	var wg sync.WaitGroup

	// Started before the config gate: awaitEnrollment below can hold the
	// process for days, and systemd's WatchdogSec does not pause for it.
	// It pings only while hb is fresh (watchdogLoop).
	hb := newHeartbeat()
	wg.Add(1)
	go func() {
		defer wg.Done()
		watchdogLoop(ctx, logger, hb)
	}()

	cfg, err := config.Load(defaultConfigPath)
	if err != nil {
		logger.Info("not enrolled yet — entering pre-enrollment mode", "reason", err)
		cfg = awaitEnrollment(ctx, logger, hb)
		if cfg == nil { // context cancelled while waiting
			wg.Wait()
			logger.Info("shutdown complete")
			return
		}
		logger.Info("enrollment detected", "deviceId", cfg.DeviceID)
	}

	hb.beat()
	if err := startBoard(ctx, logger, cfg, &wg); err != nil {
		logger.Error("could not start", "err", err)
		os.Exit(1)
	}
	// From now on the heartbeat is the local server answering.
	wg.Add(1)
	go func() {
		defer wg.Done()
		probeLoop(ctx, hb, "http://"+localserver.ListenAddr+"/api/local/status", livenessProbeInterval)
	}()

	<-ctx.Done()
	logger.Info("shutdown signal received")
	_, _ = daemon.SdNotify(false, daemon.SdNotifyStopping)
	wg.Wait()
	logger.Info("shutdown complete")
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
func awaitEnrollment(ctx context.Context, logger *slog.Logger, hb *heartbeat) *config.Config {
	_, _ = daemon.SdNotify(false, daemon.SdNotifyReady)
	_, _ = daemon.SdNotify(false, "STATUS=waiting for enrollment")

	cdpClient := kiosk.NewClient("")

	var last netinfo.Info
	var reported bool

	t := time.NewTicker(enrollPollInterval)
	defer t.Stop()
	for {
		// Every step below is bounded, so a loop that stops coming round
		// is stuck, and the watchdog should restart us.
		hb.beat()
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
		switch err := splash.PushNetInfo(ctx, cdpClient, info, version.Version); {
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

// The systemd watchdog (WatchdogSec= in musallahboard-agent.service).
//
// A ping on a timer of its own would only prove that the Go runtime still
// schedules goroutines. So the agent pings only while a heartbeat is fresh,
// and the heartbeat is bumped by real work: before enrollment by the poll
// loop in awaitEnrollment (every 5 s), and once enrolled by probeLoop, which
// asks the agent's own local server for /api/local/status. Answering that
// takes the HTTP server and the locks of the sync, backend, update and clock
// state, i.e. what the board and the admin portal depend on. None of it
// waits on the network or on the disk beyond a few small files, so an idle or
// offline board stays live; only a wedged server or a deadlock goes stale.
//
// The loops that do wait on the network (backend WebSocket, content sync,
// release channels) are not covered: a stall there is indistinguishable here
// from a slow network, and restarting the agent for it would do more harm
// than good.
const (
	// livenessProbeInterval is how often probeLoop asks the local server.
	livenessProbeInterval = 10 * time.Second
	// livenessProbeTimeout bounds one probe.
	livenessProbeTimeout = 15 * time.Second
)

// heartbeat is the time of the last proof of life. It holds a time.Time
// with its monotonic reading, so stepping the wall clock (which this agent
// does, with CAP_SYS_TIME) never makes it look stale.
type heartbeat struct{ last atomic.Pointer[time.Time] }

func newHeartbeat() *heartbeat {
	h := &heartbeat{}
	h.beat()
	return h
}

func (h *heartbeat) beat() {
	t := time.Now()
	h.last.Store(&t)
}

func (h *heartbeat) age() time.Duration { return time.Since(*h.last.Load()) }

// watchdogLoop pings systemd's WATCHDOG=1 while the heartbeat is fresh.
func watchdogLoop(ctx context.Context, logger *slog.Logger, hb *heartbeat) {
	interval, err := daemon.SdWatchdogEnabled(false)
	if err != nil || interval == 0 {
		return
	}
	runWatchdog(ctx, logger, hb, interval, func() { _, _ = daemon.SdNotify(false, daemon.SdNotifyWatchdog) })
}

// runWatchdog checks every interval/4 and pings when the heartbeat is at
// most interval old. A daemon that stops beating is therefore killed between
// interval and about 2.25 x interval after its last beat (2 to 2.5 minutes
// with WatchdogSec=60s): long enough that a busy moment is not mistaken for
// a hang.
func runWatchdog(ctx context.Context, logger *slog.Logger, hb *heartbeat, interval time.Duration, ping func()) {
	t := time.NewTicker(interval / 4)
	defer t.Stop()
	withheld := false
	for {
		select {
		case <-t.C:
		case <-ctx.Done():
			return
		}
		age := hb.age()
		if age <= interval {
			if withheld {
				logger.Info("agent is live again; watchdog pings resumed")
				withheld = false
			}
			ping()
			continue
		}
		if !withheld {
			logger.Error("agent is not live: no heartbeat; withholding the watchdog ping so systemd restarts it",
				"lastHeartbeat", age.Round(time.Second).String())
			withheld = true
		}
	}
}

// probeLoop bumps hb whenever the local server answers url with 200.
func probeLoop(ctx context.Context, hb *heartbeat, url string, every time.Duration) {
	client := &http.Client{
		Timeout:   livenessProbeTimeout,
		Transport: &http.Transport{Proxy: nil, DisableKeepAlives: true},
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		if probe(ctx, client, url) {
			hb.beat()
		}
		select {
		case <-t.C:
		case <-ctx.Done():
			return
		}
	}
}

func probe(ctx context.Context, client *http.Client, url string) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false
	}
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

func newLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))
}
