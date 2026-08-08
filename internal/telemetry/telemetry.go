package telemetry

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

	"github.com/LensBridge/agent/internal/cdp"
	"github.com/LensBridge/agent/internal/netinfo"
)

// Snapshot is one heartbeat payload's worth of telemetry. Optional fields
// (any with omitempty) may be missing if the underlying source isn't
// available — collecting telemetry must never fail; degraded data is fine.
type Snapshot struct {
	Timestamp         int64    `json:"timestamp"`
	UptimeSec         int64    `json:"uptimeSec"`
	CPUTempC          float64  `json:"cpuTempC,omitempty"`
	ThrottleFlags     string   `json:"throttleFlags,omitempty"`
	MemUsedMB         int64    `json:"memUsedMb"`
	MemTotalMB        int64    `json:"memTotalMb"`
	DiskUsedPct       float64  `json:"diskUsedPct,omitempty"`
	KioskAlive        bool     `json:"kioskAlive"`
	DisplayedFrameKey string   `json:"displayedFrameKey,omitempty"`
	IPAddrs           []string `json:"ipAddrs,omitempty"`
	SSID              string   `json:"ssid,omitempty"`
	AgentVersion      string   `json:"agentVersion"`
	SafeMode          bool     `json:"safeMode,omitempty"`
}

// PageProber reports what the kiosk browser is currently showing. Satisfied by
// *cdp.Client; pass nil to skip the browser probe entirely.
type PageProber interface {
	PageStatus(ctx context.Context) (cdp.PageStatus, error)
}

// pageProbeTimeout bounds the CDP round trip. Chromium is on loopback, so this
// is generous; the point is that a wedged browser must never stall a heartbeat.
const pageProbeTimeout = 3 * time.Second

// Collect gathers a fresh snapshot. Errors in individual collectors are
// swallowed and the corresponding field left zero/empty.
func Collect(ctx context.Context, agentVersion string, safeMode bool, prober PageProber) Snapshot {
	s := Snapshot{
		Timestamp:    time.Now().Unix(),
		AgentVersion: agentVersion,
		SafeMode:     safeMode,
	}
	s.UptimeSec, _ = readUptime()
	s.CPUTempC, _ = readCPUTemp()
	s.ThrottleFlags, _ = readThrottleFlags(ctx)
	s.MemUsedMB, s.MemTotalMB, _ = readMemInfo()
	s.DiskUsedPct, _ = readDiskUsedPct()
	s.KioskAlive, s.DisplayedFrameKey = checkKiosk(ctx, prober)
	// Unfiltered on purpose: the heartbeat reports every address the box holds,
	// link-local included, so the backend can see a board that failed DHCP.
	// The enrollment splash uses netinfo.Collect, which drops those.
	s.IPAddrs = netinfo.IPv4Addrs()
	s.SSID, _ = netinfo.SSID(ctx)
	return s
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
// enrollment splash, or showing a network error page. That made kioskAlive
// useless as an alert signal. Ask the browser instead, and fall back to the
// unit state only when the debugger is unreachable (no CDP port, browser still
// starting, or a build without --remote-debugging-port).
func checkKiosk(ctx context.Context, prober PageProber) (alive bool, frameID string) {
	if prober != nil {
		pctx, cancel := context.WithTimeout(ctx, pageProbeTimeout)
		defer cancel()

		status, err := prober.PageStatus(pctx)
		switch {
		case err == nil:
			// The board answered. It's alive by definition.
			return true, status.SlideKey
		case errors.Is(err, cdp.ErrEvalUndefined):
			// A page exists but isn't the board — the splash, or a frontend
			// too old to expose getStatus(). The browser is up regardless.
			return true, ""
		}
		// Anything else: Chromium unreachable. Fall through.
	}
	return systemdKioskActive(ctx), ""
}

func systemdKioskActive(ctx context.Context) bool {
	cctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	out, _ := exec.CommandContext(cctx, "systemctl", "is-active", "musallahboard-kiosk.service").Output()
	return strings.TrimSpace(string(out)) == "active"
}
