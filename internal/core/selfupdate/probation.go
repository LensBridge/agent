package selfupdate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Probation timing. Every window but ComeUpTimeout starts when the new agent
// first reports its version.
//
// Boards installed before the update transaction existed run the updater
// under a unit with TimeoutStartSec=5min; a typical update (seconds to verify
// and start, then ProbationPeriod) fits inside it. If one does not, systemd
// stops the updater, the record stays, and the probation is started again
// (see Txn.Attempts).
const (
	// ComeUpTimeout is how long the restarted agent has to report its
	// version on the loopback status endpoint.
	ComeUpTimeout = 120 * time.Second
	// ResumeComeUpTimeout replaces it when a probation is resumed at boot:
	// the agent waits for the network before it starts.
	ResumeComeUpTimeout = 5 * time.Minute
	// BackendTimeout is how long a board that was connected to LensBridge
	// before the update has to connect again.
	BackendTimeout = 3 * time.Minute
	// ProbationPeriod is how long the new agent must then run without being
	// restarted.
	ProbationPeriod = 4 * time.Minute
	// FinalCheckGrace is how long, after ProbationPeriod, the updater keeps
	// trying for the clean final reading the verdict needs.
	FinalCheckGrace = 30 * time.Second
	// PollInterval is how often the agent is looked at during probation.
	PollInterval = 3 * time.Second
	// MaxProbationAttempts bounds how often a probation cut short is
	// started again before the update is rolled back.
	MaxProbationAttempts = 3
	// MaxHandRestarts bounds how often the new agent is judged again because
	// someone (not systemd) restarted or stopped it during its probation.
	MaxHandRestarts = 3
)

// AgentStatus is the part of /api/local/status (docs/architecture.md,
// section 7) the updater judges by. Every field has been there since local
// API 2, so the previous agent's answer reads the same as the new one's.
type AgentStatus struct {
	AgentVersion string `json:"agentVersion"`
	Backend      *struct {
		Connected bool `json:"connected"`
	} `json:"backend"`
	Sync *struct {
		LastSuccessAt *string `json:"lastSuccessAt"`
		LastError     *string `json:"lastError"`
	} `json:"sync"`
}

// BackendUp reports whether the agent is connected to LensBridge: its
// WebSocket is authenticated, or a content sync worked and none has failed
// since. Both live in the agent's memory, so for a freshly started agent
// they say what that process achieved.
func (s AgentStatus) BackendUp() bool {
	if s.Backend != nil && s.Backend.Connected {
		return true
	}
	return s.Sync != nil && s.Sync.LastSuccessAt != nil && s.Sync.LastError == nil
}

// ServiceState is what systemd says about the agent's unit.
type ServiceState struct {
	ActiveState string
	MainPID     int
	NRestarts   int
}

// serviceState asks systemd about the agent unit. systemd, not the agent,
// is the authority on whether the agent was restarted: a crash between two
// looks shows as a new MainPID or a higher NRestarts.
func (u *Updater) serviceState(ctx context.Context) (ServiceState, error) {
	out, err := u.Run(ctx, "systemctl", "show", AgentUnit, "--property=ActiveState,MainPID,NRestarts")
	if err != nil {
		return ServiceState{}, fmt.Errorf("systemctl show: %v %s", err, firstLine(out))
	}
	var s ServiceState
	seen := 0
	for _, line := range strings.Split(out, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		switch k {
		case "ActiveState":
			s.ActiveState, seen = v, seen+1
		case "MainPID":
			s.MainPID, _ = strconv.Atoi(v)
			seen++
		case "NRestarts":
			s.NRestarts, _ = strconv.Atoi(v)
			seen++
		}
	}
	if seen != 3 {
		return ServiceState{}, fmt.Errorf("systemctl show said %q", firstLine(out))
	}
	return s, nil
}

// ErrInterrupted means the updater was told to stop mid-probation. The
// record stays, so the probation is resumed by the next run.
var ErrInterrupted = errors.New("the updater was stopped before the new agent could be judged")

// verdict is a failed probation: why, and whether the version should be
// rejected outright (false: only a strike, see strikes).
type verdict struct {
	reason string
	reject bool
}

// probation watches the restarted agent and returns nil if it passes:
//
//   - it reports rec.To on the loopback status endpoint within the come-up
//     timeout;
//   - from then on systemd never restarts it (same MainPID, same NRestarts,
//     still active) for ProbationPeriod;
//   - a crash (systemd's NRestarts goes up, or the unit fails) rejects it.
//     A restart or stop that is not systemd's doing (a new MainPID, or the
//     unit inactive, with NRestarts unchanged: someone ran systemctl, or
//     rebooted it into a resumed probation) is not the new agent's fault: it
//     is judged again from the start, up to MaxHandRestarts times, and if it
//     then fails to come up the version only gets a strike;
//   - if rec.BackendBefore, it connects to LensBridge within BackendTimeout.
//
// A board that was offline before the update is not asked to be online
// after it.
func (u *Updater) probation(ctx context.Context, rec *Txn, resumed bool) (*verdict, error) {
	comeUp := u.ComeUpTimeout
	if resumed {
		comeUp = u.ResumeComeUpTimeout
	}
	start := u.Now()
	var (
		healthy   bool
		healthyAt time.Time
		base      ServiceState
		backendOK = !rec.BackendBefore
		last      = "no answer"
		// restartBase is systemd's NRestarts when the agent first came up.
		// It is not reset when the agent is judged again, so a crash that a
		// poll happened to see as a plain restart is still caught.
		restartBase  = -1
		handRestarts int
	)
	need := ""
	if rec.BackendBefore {
		need = ", and connect to LensBridge within " + u.BackendTimeout.String()
	}
	u.Logf("judging agent %s (attempt %d): it must report its version within %s, then run %s without a restart%s",
		rec.To, rec.Attempts, comeUp, u.ProbationPeriod, need)
	for {
		if ctx.Err() != nil {
			return nil, ErrInterrupted
		}
		st, serr := u.Status(ctx)
		svc, verr := u.serviceState(ctx)
		if ctx.Err() != nil {
			return nil, ErrInterrupted
		}
		now := u.Now()
		switch {
		case serr != nil:
			last = serr.Error()
		case st.AgentVersion != rec.To:
			last = "it reports version " + st.AgentVersion
		default:
			last = ""
		}
		good := serr == nil && st.AgentVersion == rec.To && verr == nil && svc.ActiveState == "active" && svc.MainPID > 0

		crashed := verr == nil && restartBase >= 0 && (svc.NRestarts > restartBase || svc.ActiveState == "failed")
		if crashed {
			return &verdict{fmt.Sprintf("agent %s was restarted by systemd during its probation: it crashed (systemd: %s, pid %d, %d restarts)",
				rec.To, svc.ActiveState, svc.MainPID, svc.NRestarts), true}, nil
		}
		if !healthy {
			if good {
				healthy, healthyAt, base = true, now, svc
				if restartBase < 0 {
					restartBase = svc.NRestarts
				}
				u.Logf("agent %s is up (pid %d)", rec.To, svc.MainPID)
			} else if now.Sub(start) >= comeUp {
				if last == "" && verr != nil {
					last = verr.Error()
				}
				// After a restart by hand, not coming back is as likely the
				// hand's doing (a stop that was never undone) as the agent's.
				return &verdict{fmt.Sprintf("agent %s did not come up within %s: %s", rec.To, comeUp, last), handRestarts == 0}, nil
			}
		} else {
			if verr == nil && (svc.MainPID != base.MainPID ||
				(svc.ActiveState != "active" && svc.ActiveState != "reloading")) {
				// Not a crash (checked above): someone restarted or stopped it.
				handRestarts++
				if handRestarts > MaxHandRestarts {
					return &verdict{fmt.Sprintf("agent %s could not be judged: it was restarted or stopped by hand %d times during its probation",
						rec.To, handRestarts), false}, nil
				}
				u.Logf("agent %s was restarted or stopped %s after it came up, not by systemd (systemd: %s, pid %d, %d restarts); judging it again from the start",
					rec.To, now.Sub(healthyAt).Round(time.Second), svc.ActiveState, svc.MainPID, svc.NRestarts)
				healthy, start, backendOK = false, now, !rec.BackendBefore
				if err := u.Sleep(ctx, u.PollInterval); err != nil {
					return nil, ErrInterrupted
				}
				continue
			}
			if serr == nil && st.AgentVersion != rec.To {
				return &verdict{fmt.Sprintf("agent %s was replaced by %s during its probation", rec.To, st.AgentVersion), true}, nil
			}
		}
		if healthy && !backendOK {
			if serr == nil && st.BackendUp() {
				backendOK = true
				u.Logf("agent %s is connected to LensBridge after %s", rec.To, now.Sub(healthyAt).Round(time.Second))
			} else if now.Sub(healthyAt) >= u.BackendTimeout {
				return &verdict{fmt.Sprintf("agent %s did not connect to LensBridge within %s, although the board was connected before the update",
					rec.To, u.BackendTimeout), false}, nil
			}
		}
		if healthy && backendOK && now.Sub(healthyAt) >= u.ProbationPeriod {
			if good && svc.MainPID == base.MainPID && svc.NRestarts == base.NRestarts {
				return nil, nil
			}
			if now.Sub(healthyAt) >= u.ProbationPeriod+u.FinalCheckGrace {
				if last == "" && verr != nil {
					last = verr.Error()
				}
				return &verdict{fmt.Sprintf("agent %s stopped answering at the end of its probation: %s", rec.To, last), true}, nil
			}
		}
		if err := u.Sleep(ctx, u.PollInterval); err != nil {
			return nil, ErrInterrupted
		}
	}
}

// FetchStatus reads the agent's /api/local/status.
func FetchStatus(ctx context.Context, statusURL string) (AgentStatus, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, statusURL, nil)
	if err != nil {
		return AgentStatus{}, err
	}
	// Loopback only: never through a proxy from the environment.
	client := &http.Client{Transport: &http.Transport{Proxy: nil, DisableKeepAlives: true}}
	resp, err := client.Do(req)
	if err != nil {
		return AgentStatus{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return AgentStatus{}, fmt.Errorf("status returned %d", resp.StatusCode)
	}
	var st AgentStatus
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&st); err != nil {
		return AgentStatus{}, err
	}
	return st, nil
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
