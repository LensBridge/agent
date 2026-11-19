package wsclient

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/LensBridge/agent/internal/core/kiosk"
	"github.com/LensBridge/agent/internal/core/netinfo"
)

// PageProber reports what the kiosk browser is currently showing. Satisfied by
// *kiosk.Client; pass nil to skip the browser probe entirely.
type PageProber interface {
	PageStatus(ctx context.Context) (kiosk.PageStatus, error)
}

// pageProbeTimeout bounds the CDP round trip. Chromium is on loopback, so this
// is generous; the point is that a wedged browser must never stall a heartbeat.
const pageProbeTimeout = 3 * time.Second

// collectTelemetry gathers one heartbeat's telemetry. It never fails: a
// reading that cannot be taken is left out (nil, so the backend stores NULL
// rather than a misleading zero).
func collectTelemetry(ctx context.Context, agentVersion string, prober PageProber) Telemetry {
	t := Telemetry{AgentVersion: agentVersion}
	if v, err := readUptime(); err == nil && v > 0 {
		t.UptimeSec = &v
	}
	if v, err := readCPUTemp(); err == nil && v > 0 {
		t.CPUTempC = &v
	}
	t.ThrottleFlags, _ = readThrottleFlags(ctx)
	if used, total, err := readMemInfo(); err == nil && total > 0 {
		u, tot := int(used), int(total)
		t.MemUsedMb, t.MemTotalMb = &u, &tot
	}
	if v, err := readDiskUsedPct(); err == nil && v > 0 {
		pct := int(v + 0.5) // nearest percent
		t.DiskUsedPct = &pct
	}
	// kioskAlive is always sent: "could not tell" and "the board is down"
	// must not look the same to the fleet dashboard.
	alive, frame := checkKiosk(ctx, prober)
	t.KioskAlive, t.DisplayedFrameKey = &alive, frame
	// Unfiltered on purpose: the heartbeat reports every address the box
	// holds, link-local included, so the backend can see a board that
	// failed DHCP.
	t.IPv4 = netinfo.IPv4Addrs()
	t.WifiSSID, _ = netinfo.SSID(ctx)
	return t
}

func readUptime() (int64, error) {
	b, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return 0, err
	}
	parts := strings.Fields(string(b))
	if len(parts) < 1 {
		return 0, fmt.Errorf("malformed /proc/uptime")
	}
	f, err := strconv.ParseFloat(parts[0], 64)
	if err != nil {
		return 0, err
	}
	return int64(f), nil
}

func readCPUTemp() (float64, error) {
	b, err := os.ReadFile("/sys/class/thermal/thermal_zone0/temp")
	if err != nil {
		return 0, err
	}
	raw, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		return 0, err
	}
	return float64(raw) / 1000.0, nil
}

// readThrottleFlags returns the raw vcgencmd value, e.g. "0x50000".
// Bit 0: undervoltage detected. Bit 16: undervoltage has occurred.
// Bit 1: ARM frequency capped. Bit 2: currently throttled. (etc.)
// Backend decodes the bitmap into human-readable alerts.
func readThrottleFlags(ctx context.Context) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(cctx, "vcgencmd", "get_throttled").Output()
	if err != nil {
		return "", err
	}
	s := strings.TrimSpace(string(out))
	if i := strings.Index(s, "="); i >= 0 {
		return s[i+1:], nil
	}
	return s, nil
}

func readMemInfo() (used, total int64, err error) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()

	fields := map[string]int64{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		parts := strings.Fields(sc.Text())
		if len(parts) < 2 {
			continue
		}
		key := strings.TrimSuffix(parts[0], ":")
		v, err := strconv.ParseInt(parts[1], 10, 64)
		if err != nil {
			continue
		}
		fields[key] = v // kB
	}
	if fields["MemTotal"] == 0 {
		return 0, 0, fmt.Errorf("no MemTotal in /proc/meminfo")
	}
	total = fields["MemTotal"] / 1024
	avail := fields["MemAvailable"]
	if avail == 0 {
		avail = fields["MemFree"] + fields["Buffers"] + fields["Cached"]
	}
	used = (fields["MemTotal"] - avail) / 1024
	return used, total, nil
}

func readDiskUsedPct() (float64, error) {
	var stat statfsResult
	if err := statfs("/", &stat); err != nil {
		return 0, err
	}
	if stat.Blocks == 0 {
		return 0, nil
	}
	used := stat.Blocks - stat.Bavail
	return float64(used) / float64(stat.Blocks) * 100.0, nil
}

// checkKiosk reports whether the board is actually on screen, plus which frame
// it is showing.
//
// The systemd unit is Restart=always, so `is-active` says "active" almost
// unconditionally — including while Chromium is crash-looping, parked on the
// setup screen, or showing a crashed tab ("Aw, Snap!"). Ask the browser
// instead, and fall back to the unit state only when there is no browser to
// ask (no CDP port, browser still starting, or a build without
// --remote-debugging-port).
//
// A browser that is reachable but whose page target crashed or does not
// answer evaluate in time is not alive, whatever systemd says: that is the
// blank board the admin console must see.
func checkKiosk(ctx context.Context, prober PageProber) (alive bool, frameID string) {
	if prober != nil {
		pctx, cancel := context.WithTimeout(ctx, pageProbeTimeout)
		defer cancel()

		status, err := prober.PageStatus(pctx)
		switch {
		case err == nil:
			// The board answered. It's alive by definition.
			return true, status.SlideKey
		case errors.Is(err, kiosk.ErrEvalUndefined),
			errors.Is(err, kiosk.ErrPageThrew),
			errors.Is(err, kiosk.ErrPageNavigating):
			// A page answered (or is between documents) but isn't the board:
			// the setup screen, the update screen, a frontend too old to expose
			// getStatus(), or a reload in flight. The browser is up.
			return true, ""
		case errors.Is(err, kiosk.ErrPageCrashed), errors.Is(err, kiosk.ErrPageUnresponsive):
			// The browser is there and its page is dead.
			return false, ""
		}
		// Anything else: Chromium unreachable, or no page yet. Fall through.
	}
	return systemdKioskActive(ctx), ""
}

func systemdKioskActive(ctx context.Context) bool {
	cctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	out, _ := exec.CommandContext(cctx, "systemctl", "is-active", "musallahboard-kiosk.service").Output()
	return strings.TrimSpace(string(out)) == "active"
}
