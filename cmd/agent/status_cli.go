package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/LensBridge/agent/internal/localserver"
	"github.com/LensBridge/agent/internal/store"
	"github.com/LensBridge/agent/internal/trust"
	"github.com/LensBridge/agent/internal/version"
)

// cliStatus is /api/local/status plus what only the CLI reports.
type cliStatus struct {
	localserver.Status
	Clock  clockInfo `json:"clock"`
	RTC    bool      `json:"rtc"`
	Trust  trustInfo `json:"trust"`
	Daemon bool      `json:"daemonRunning"`
}

type clockInfo struct {
	Now      string `json:"now"`
	Unix     int64  `json:"unix"`
	Timezone string `json:"timezone"`
}

type trustInfo struct {
	ContentKeys int `json:"contentKeys"`
	ReleaseKeys int `json:"releaseKeys"`
}

func runStatus(args []string) {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "print the status object as JSON")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "usage: musallahboard-agent status [--json]")
		os.Exit(2)
	}
	st := buildCLIStatus()
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(st)
		return
	}
	printStatus(st)
}

// buildCLIStatus asks the running daemon first: only it knows the sync state
// and whether the update screen is up, and asking needs no privileges. If it
// is not running, the installed state is read from disk (which needs root).
func buildCLIStatus() cliStatus {
	var st cliStatus
	if s, ok := daemonStatus(); ok {
		st.Status, st.Daemon = s, true
	} else {
		cfg := loadConfigCLI()
		st.Status = localserver.BuildStatus(store.Default(), cfg.DeviceID, version.Version, time.Now())
	}
	now := time.Now()
	st.Clock = clockInfo{Now: now.Format(time.RFC3339), Unix: now.Unix(), Timezone: systemZone()}
	_, err := os.Stat(rtcDevice)
	st.RTC = err == nil
	if ts, err := trust.Load(trust.DefaultPath); err == nil {
		st.Trust.ContentKeys = len(ts.Content)
		st.Trust.ReleaseKeys = len(ts.Release)
	}
	if b, err := trust.Builtin(); err == nil {
		st.Trust.ReleaseKeys += len(b)
	}
	return st
}

func daemonStatus() (localserver.Status, bool) {
	var s localserver.Status
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, localserver.BaseURL+"api/local/status", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return s, false
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil || resp.StatusCode != http.StatusOK || json.Unmarshal(raw, &s) != nil {
		return s, false
	}
	return s, true
}

// systemZone names the system timezone. Go reports time.Local as "Local",
// which tells a person nothing.
func systemZone() string {
	if tz := strings.TrimPrefix(os.Getenv("TZ"), ":"); tz != "" {
		return tz
	}
	if target, err := os.Readlink("/etc/localtime"); err == nil {
		if _, name, ok := strings.Cut(target, "zoneinfo/"); ok {
			return name
		}
	}
	if b, err := os.ReadFile("/etc/timezone"); err == nil && strings.TrimSpace(string(b)) != "" {
		return strings.TrimSpace(string(b))
	}
	name, _ := time.Now().Zone()
	return name
}

func printStatus(st cliStatus) {
	row := func(k, format string, args ...any) {
		fmt.Printf("  %-13s %s\n", k+":", fmt.Sprintf(format, args...))
	}
	row("Device", "%s", st.DeviceID)
	if st.Daemon {
		row("Agent", "%s (running)", st.AgentVersion)
	} else {
		row("Agent", "%s (NOT running: systemctl status musallahboard-agent)", version.Version)
	}
	if st.App != nil {
		row("Board app", "%s", st.App.Version)
	} else {
		row("Board app", "none installed: the screen asks for an update")
	}

	switch {
	case st.Error != "":
		row("Content", "installed but unreadable: %s", st.Error)
	case st.Content == nil:
		row("Content", "none installed")
	default:
		c := st.Content
		src := ""
		if c.Source != "" {
			src = ", from " + c.Source
		}
		row("Content", "%s to %s (created %s%s)", c.FirstDay, c.LastDay, c.CreatedAt, src)
		switch {
		case *st.StaleDays > 0:
			row("Today", "%s: content ran out %d day(s) ago; repeating %s", st.Today, *st.StaleDays, *st.ServingDay)
		case *st.DaysRemaining == 0:
			row("Today", "%s: the last day of this content", st.Today)
		default:
			row("Today", "%s, %d more day(s) of content", st.Today, *st.DaysRemaining)
		}
	}
	if m, ok := st.Sync.(map[string]any); ok {
		switch {
		case m["enabled"] == false:
			row("Sync", "off")
		case m["lastError"] != nil:
			row("Sync", "failing: %v", m["lastError"])
		case m["lastSuccessAt"] != nil:
			row("Sync", "last succeeded %v", m["lastSuccessAt"])
		default:
			row("Sync", "not yet attempted")
		}
	}
	if u := st.Updates; u != nil {
		switch {
		case u.Installing:
			row("Updates", "installing now")
		case len(u.Available) == 0:
			row("Updates", "none waiting (installs at %s)", u.InstallTime)
		default:
			var names []string
			for _, a := range u.Available {
				names = append(names, a.Description)
			}
			at := ""
			if u.InstallAt != nil {
				at = *u.InstallAt
			}
			row("Updates", "%s waiting, installs %s", strings.Join(names, " and "), at)
			row("", "Install now: sudo musallahboard-agent update now")
		}
	}
	if o := st.LastAgentUpdate; o != nil {
		row("Last agent update", "%s, %s: %s", o.At, o.Status, o.Message)
	}
	row("Trust", "%d content key(s), %d release key(s)", st.Trust.ContentKeys, st.Trust.ReleaseKeys)
	if st.Trust.ContentKeys == 0 {
		row("", "No content key: run `sudo musallahboard-agent trust fetch` while online.")
	}
	t, _ := time.Parse(time.RFC3339, st.Clock.Now)
	row("Clock", "%s (%s)", t.Format("Mon 2 Jan 2006 15:04:05 MST"), st.Clock.Timezone)
	if st.RTC {
		row("RTC", "present (%s)", rtcDevice)
	} else {
		row("RTC", "not found: the clock only survives power cuts through the last update's time")
	}
}
