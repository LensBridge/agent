package selfupdate

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/LensBridge/agent/internal/mbu"
	"github.com/LensBridge/agent/internal/state"
	"github.com/LensBridge/agent/internal/store"
	"github.com/LensBridge/agent/internal/trust"
)

var (
	releaseKey = ed25519.NewKeyFromSeed(bytes.Repeat([]byte{2}, 32))
	otherKey   = ed25519.NewKeyFromSeed(bytes.Repeat([]byte{9}, 32))
)

const oldBinary = "old agent binary"

// fakeAgent is musallahboard-agent.service as systemd and the loopback
// status endpoint show it. It runs whatever binary is installed when it is
// (re)started.
type fakeAgent struct {
	version   string
	pid       int
	restarts  int
	active    bool
	startedAt time.Time
	// crashAfter makes the new version crash (and systemd restart it) once
	// it has run this long; 0 never.
	crashAfter time.Duration
	// backendOld and backendNew say whether each version connects to
	// LensBridge.
	backendOld, backendNew bool
	// downNew makes the new version never answer on the status endpoint.
	downNew bool
}

type harness struct {
	t        *testing.T
	u        *Updater
	cmds     []string
	versionO string
	now      time.Time
	agent    fakeAgent
	// restartsSeen records, for each `systemctl restart`, whether
	// last-update.json existed at that moment.
	outcomeAtRestart []bool
	// tick runs after every fake sleep.
	tick func(h *harness)
}

func versionOf(binary string) string {
	if binary == oldBinary {
		return "0.3.0"
	}
	return strings.TrimPrefix(binary, "new agent ")
}

func (h *harness) start() {
	h.agent.pid += 100
	h.agent.version = versionOf(read(h.t, h.u.Binary))
	h.agent.active = true
	h.agent.startedAt = h.now
}

func (h *harness) isNew() bool { return h.agent.version != "0.3.0" }

// advance moves the fake clock, crashing the agent when it is due to.
func (h *harness) advance(d time.Duration) {
	h.now = h.now.Add(d)
	if h.isNew() && h.agent.crashAfter > 0 && h.now.Sub(h.agent.startedAt) >= h.agent.crashAfter {
		h.agent.restarts++
		h.start()
	}
	if h.tick != nil {
		h.tick(h)
	}
}

func newHarness(t *testing.T, withContentKey bool) *harness {
	t.Helper()
	dir := t.TempDir()
	h := &harness{t: t, now: time.Date(2026, 9, 24, 23, 0, 0, 0, time.UTC)}
	l := store.Layout{Root: filepath.Join(dir, "var")}
	os.MkdirAll(l.AgentStagedDir(), 0o750)
	bin := filepath.Join(dir, "usr", "bin", "musallahboard-agent")
	os.MkdirAll(filepath.Dir(bin), 0o755)
	os.WriteFile(bin, []byte(oldBinary), 0o755)

	ts := &trust.Store{}
	ts.Add(trust.RoleRelease, releaseKey.Public().(ed25519.PublicKey), "ci")
	if withContentKey {
		ts.Add(trust.RoleContent, otherKey.Public().(ed25519.PublicKey), "lensbridge")
	}
	trustPath := filepath.Join(dir, "trust.json")
	if err := ts.Save(trustPath); err != nil {
		t.Fatal(err)
	}

	h.u = &Updater{
		Layout:         l,
		TrustPath:      trustPath,
		Binary:         bin,
		NewBinary:      filepath.Join(filepath.Dir(bin), ".musallahboard-agent.new"),
		Prev:           filepath.Join(dir, "usr", "lib", "musallahboard", "agent.prev"),
		TempRoot:       dir,
		Arch:           "arm64",
		CurrentVersion: "0.3.0",
		Logf:           t.Logf,
	}
	h.wire(h.u)
	h.u.defaults()
	h.start()
	return h
}

// wire connects an Updater to the harness's fakes.
func (h *harness) wire(u *Updater) {
	u.Run = func(ctx context.Context, name string, args ...string) (string, error) {
		cmd := strings.TrimSpace(filepath.Base(name) + " " + strings.Join(args, " "))
		h.cmds = append(h.cmds, cmd)
		switch {
		case len(args) == 1 && args[0] == "version":
			return h.versionO, nil
		case cmd == "systemctl restart "+AgentUnit:
			_, err := os.Stat(u.Layout.AgentLastUpdate())
			h.outcomeAtRestart = append(h.outcomeAtRestart, err == nil)
			h.start()
		case strings.HasPrefix(cmd, "systemctl show "+AgentUnit):
			state := "active"
			if !h.agent.active {
				state = "inactive"
			}
			return fmt.Sprintf("MainPID=%d\nNRestarts=%d\nActiveState=%s", h.agent.pid, h.agent.restarts, state), nil
		}
		return "", nil
	}
	u.Status = func(ctx context.Context) (AgentStatus, error) {
		if h.isNew() && h.agent.downNew {
			return AgentStatus{}, errors.New("connection refused")
		}
		var st AgentStatus
		st.AgentVersion = h.agent.version
		up := h.agent.backendOld
		if h.isNew() {
			up = h.agent.backendNew
		}
		st.Backend = &struct {
			Connected bool `json:"connected"`
		}{up}
		return st, nil
	}
	u.Now = func() time.Time { return h.now }
	u.Sleep = func(ctx context.Context, d time.Duration) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		h.advance(d)
		return nil
	}
}

// stage writes a signed agent package and the ready marker.
func (h *harness) stage(t *testing.T, version, arch string, key ed25519.PrivateKey) {
	t.Helper()
	m := mbu.Manifest{Type: mbu.TypeAgent, Version: version, Agent: &mbu.AgentInfo{Arch: arch, Binary: "musallahboard-agent"}}
	var buf bytes.Buffer
	if err := mbu.Build(&buf, m, []mbu.Source{{Path: "musallahboard-agent", Data: []byte("new agent " + version)}},
		[]ed25519.PrivateKey{key}); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(h.u.Layout.StagedPackage(), buf.Bytes(), 0o640)
	os.WriteFile(h.u.Layout.StagedReady(), []byte("now\n"), 0o640)
	h.versionO = "musallahboard-agent " + version
}

func read(t *testing.T, p string) string {
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read %s: %v", p, err)
	}
	return string(b)
}

func exists(p string) bool { _, err := os.Lstat(p); return err == nil }

func lastUpdate(t *testing.T, u *Updater) Outcome {
	var o Outcome
	if err := json.Unmarshal([]byte(read(t, u.Layout.AgentLastUpdate())), &o); err != nil {
		t.Fatal(err)
	}
	return o
}

func rejected(t *testing.T, u *Updater, v string) bool {
	st, err := u.Layout.State().Load()
	if err != nil {
		t.Fatal(err)
	}
	return st.AgentRejected(v)
}

func (h *harness) count(cmd string) int {
	n := 0
	for _, c := range h.cmds {
		if c == cmd {
			n++
		}
	}
	return n
}

// settled checks that no transaction or trigger is left behind.
func (h *harness) settled(t *testing.T) {
	t.Helper()
	for _, p := range []string{h.u.Txn, h.u.Layout.StagedReady(), h.u.Layout.StagedPackage(), h.u.NewBinary} {
		if exists(p) {
			t.Errorf("%s left behind", p)
		}
	}
	if m, _ := filepath.Glob(filepath.Join(h.u.TempRoot, "musallahboard-selfupdate-*")); len(m) != 0 {
		t.Errorf("work dirs left: %v", m)
	}
}

func TestApplyOK(t *testing.T) {
	h := newHarness(t, false)
	h.agent.backendOld, h.agent.backendNew = true, true
	h.stage(t, "0.4.0", "arm64", releaseKey)
	start := h.now
	out, err := h.u.Apply(context.Background())
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if out.Status != StatusOK || out.From != "0.3.0" || out.To != "0.4.0" {
		t.Fatalf("outcome = %+v", out)
	}
	if got := read(t, h.u.Binary); got != "new agent 0.4.0" {
		t.Errorf("binary = %q", got)
	}
	if fi, _ := os.Stat(h.u.Binary); fi.Mode().Perm() != 0o755 {
		t.Errorf("binary mode = %v", fi.Mode())
	}
	if got := read(t, h.u.Prev); got != oldBinary {
		t.Errorf("prev = %q", got)
	}
	h.settled(t)
	if took := h.now.Sub(start); took < ProbationPeriod {
		t.Errorf("committed after %s, before the probation was over", took)
	}
	if h.count("systemctl restart "+AgentUnit) != 1 || h.count("musallahboard-agent splash install") != 1 {
		t.Errorf("commands = %q", h.cmds)
	}
	if h.cmds[0] != ".musallahboard-agent.new version" {
		t.Errorf("first command = %q", h.cmds[0])
	}
	if o := lastUpdate(t, h.u); o.Status != StatusOK || o.At == "" {
		t.Errorf("last-update = %+v", o)
	}
}

func TestApplyNoComeUpRollsBack(t *testing.T) {
	h := newHarness(t, true)
	h.agent.downNew = true
	h.stage(t, "0.4.0", "arm64", releaseKey)
	out, err := h.u.Apply(context.Background())
	if err == nil || out.Status != StatusRolledBack || !strings.Contains(out.Message, "did not come up") {
		t.Fatalf("outcome = %+v, err = %v", out, err)
	}
	if got := read(t, h.u.Binary); got != oldBinary {
		t.Errorf("binary after rollback = %q", got)
	}
	if !rejected(t, h.u, "0.4.0") {
		t.Error("0.4.0 not recorded as rejected")
	}
	// The previous agent reads the outcome when it starts, so it must be
	// written before the second restart.
	if fmt.Sprint(h.outcomeAtRestart) != "[false true]" {
		t.Errorf("outcome present at restarts: %v (%q)", h.outcomeAtRestart, h.cmds)
	}
	if h.agent.version != "0.3.0" {
		t.Errorf("agent running %s after rollback", h.agent.version)
	}
	if o := lastUpdate(t, h.u); o.Status != StatusRolledBack || !strings.Contains(o.Message, "rolled back") {
		t.Errorf("last-update = %+v", o)
	}
	h.settled(t)
}

func TestApplyCrashLoopRollsBack(t *testing.T) {
	h := newHarness(t, true)
	// Up long enough to pass the old two-minute check, then crashes.
	h.agent.crashAfter = 150 * time.Second
	h.stage(t, "0.4.0", "arm64", releaseKey)
	out, err := h.u.Apply(context.Background())
	if err == nil || out.Status != StatusRolledBack || !strings.Contains(out.Message, "was restarted") {
		t.Fatalf("outcome = %+v, err = %v", out, err)
	}
	if read(t, h.u.Binary) != oldBinary || !rejected(t, h.u, "0.4.0") {
		t.Error("not rolled back and rejected")
	}
	h.settled(t)
}

func TestApplyStoppedAgentRollsBack(t *testing.T) {
	h := newHarness(t, true)
	h.stage(t, "0.4.0", "arm64", releaseKey)
	h.tick = func(h *harness) {
		if h.isNew() && h.now.Sub(h.agent.startedAt) > time.Minute {
			h.agent.active = false
		}
	}
	out, err := h.u.Apply(context.Background())
	if err == nil || out.Status != StatusRolledBack {
		t.Fatalf("outcome = %+v, err = %v", out, err)
	}
}

func TestApplyBackendLostRollsBackWithStrike(t *testing.T) {
	h := newHarness(t, true)
	h.agent.backendOld, h.agent.backendNew = true, false
	h.stage(t, "0.4.0", "arm64", releaseKey)
	out, err := h.u.Apply(context.Background())
	if err == nil || out.Status != StatusRolledBack || !strings.Contains(out.Message, "did not connect to LensBridge") ||
		!strings.Contains(out.Message, "tried again") {
		t.Fatalf("outcome = %+v, err = %v", out, err)
	}
	if read(t, h.u.Binary) != oldBinary {
		t.Error("not rolled back")
	}
	// Once might be the backend's fault: not rejected yet.
	if rejected(t, h.u, "0.4.0") {
		t.Error("rejected after one connection failure")
	}
	h.settled(t)

	// The second time it is.
	h.now = h.now.Add(24 * time.Hour)
	h.stage(t, "0.4.0", "arm64", releaseKey)
	out, err = h.u.Apply(context.Background())
	if err == nil || out.Status != StatusRolledBack || !strings.Contains(out.Message, "not retried") {
		t.Fatalf("second outcome = %+v, err = %v", out, err)
	}
	if !rejected(t, h.u, "0.4.0") {
		t.Error("not rejected after two connection failures")
	}
}

func TestApplyOfflineBoardIsNotRolledBack(t *testing.T) {
	h := newHarness(t, true)
	h.agent.backendOld, h.agent.backendNew = false, false
	h.stage(t, "0.4.0", "arm64", releaseKey)
	out, err := h.u.Apply(context.Background())
	if err != nil || out.Status != StatusOK {
		t.Fatalf("outcome = %+v, err = %v", out, err)
	}
	if read(t, h.u.Binary) != "new agent 0.4.0" {
		t.Error("offline board rolled back")
	}
}

func TestApplyNoStatusBeforeIsNotRequiredOnline(t *testing.T) {
	h := newHarness(t, true)
	h.agent.backendNew = false
	h.stage(t, "0.4.0", "arm64", releaseKey)
	status := h.u.Status
	calls := 0
	h.u.Status = func(ctx context.Context) (AgentStatus, error) {
		calls++
		if calls == 1 {
			return AgentStatus{}, errors.New("connection refused")
		}
		return status(ctx)
	}
	if out, err := h.u.Apply(context.Background()); err != nil || out.Status != StatusOK {
		t.Fatalf("outcome = %+v, err = %v", out, err)
	}
}

func TestApplyRollbackFails(t *testing.T) {
	h := newHarness(t, true)
	h.agent.downNew = true
	h.stage(t, "0.4.0", "arm64", releaseKey)
	h.u.crash = func(step string) bool {
		if step == "probation" {
			os.WriteFile(h.u.Prev, []byte("damaged"), 0o755)
		}
		return false
	}
	out, err := h.u.Apply(context.Background())
	if err == nil || out.Status != StatusRollbackFailed || !strings.Contains(out.Message, "FAILED") {
		t.Fatalf("outcome = %+v, err = %v", out, err)
	}
	if read(t, h.u.Binary) != "new agent 0.4.0" {
		t.Error("binary replaced by a damaged agent.prev")
	}
	// Finished, not left for a loop of retries.
	h.settled(t)
	if o := lastUpdate(t, h.u); o.Status != StatusRollbackFailed {
		t.Errorf("last-update = %+v", o)
	}
}

func TestApplyRefuses(t *testing.T) {
	cases := []struct {
		name    string
		setup   func(t *testing.T, h *harness)
		wantMsg string
	}{
		{"older", func(t *testing.T, h *harness) { h.stage(t, "0.2.0", "arm64", releaseKey) }, "not newer"},
		{"equal", func(t *testing.T, h *harness) { h.stage(t, "0.3.0", "arm64", releaseKey) }, "not newer"},
		{"wrong arch", func(t *testing.T, h *harness) { h.stage(t, "0.4.0", "amd64", releaseKey) }, "built for amd64"},
		{"untrusted key", func(t *testing.T, h *harness) { h.stage(t, "0.4.0", "arm64", otherKey) }, "did not verify"},
		{"rejected before", func(t *testing.T, h *harness) {
			h.stage(t, "0.4.0", "arm64", releaseKey)
			h.u.Layout.State().Update(func(s *state.State) error { s.AgentVersionsRejected = []string{"0.4.0"}; return nil })
		}, "failed to start"},
		{"binary does not run", func(t *testing.T, h *harness) {
			h.stage(t, "0.4.0", "arm64", releaseKey)
			h.versionO = "exec format error"
		}, "does not run"},
		{"symlinked package", func(t *testing.T, h *harness) {
			h.stage(t, "0.4.0", "arm64", releaseKey)
			real := h.u.Layout.StagedPackage() + ".real"
			os.Rename(h.u.Layout.StagedPackage(), real)
			os.Symlink(real, h.u.Layout.StagedPackage())
		}, "cannot read the staged package"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, true)
			tc.setup(t, h)
			out, err := h.u.Apply(context.Background())
			if out.At == "" {
				t.Error("outcome returned without its time")
			}
			if err == nil || out.Status != StatusRefused || !strings.Contains(out.Message, tc.wantMsg) {
				t.Fatalf("outcome = %+v, err = %v; want refused with %q", out, err, tc.wantMsg)
			}
			if got := read(t, h.u.Binary); got != oldBinary {
				t.Errorf("binary changed on refusal: %q", got)
			}
			for _, c := range h.cmds {
				if strings.HasPrefix(c, "systemctl") {
					t.Errorf("service touched on refusal: %q", h.cmds)
				}
			}
			if exists(h.u.NewBinary) || exists(h.u.Txn) || exists(h.u.Layout.StagedReady()) {
				t.Error("new binary, record or ready left behind")
			}
			if o := lastUpdate(t, h.u); o.Status != StatusRefused {
				t.Errorf("last-update = %+v", o)
			}
		})
	}
}

func TestApplyNothingStaged(t *testing.T) {
	h := newHarness(t, true)
	if _, err := h.u.Apply(context.Background()); !errors.Is(err, ErrNothingStaged) {
		t.Fatalf("err = %v", err)
	}
	if exists(h.u.Layout.AgentLastUpdate()) {
		t.Fatal("last-update written with nothing staged")
	}
}

// A package staged by the new agent while its predecessor was on probation
// keeps its trigger, so the path unit runs the updater again for it.
func TestApplyKeepsReadyForANewStaging(t *testing.T) {
	h := newHarness(t, true)
	h.stage(t, "0.4.0", "arm64", releaseKey)
	staged := false
	h.tick = func(h *harness) {
		if !staged && h.isNew() {
			staged = true
			os.WriteFile(h.u.Layout.StagedPackage(), []byte("next"), 0o640)
			os.WriteFile(h.u.Layout.StagedReady(), []byte("now\n"), 0o640)
		}
	}
	if _, err := h.u.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !exists(h.u.Layout.StagedReady()) {
		t.Error("ready removed although a package is staged")
	}
}

// cutAt runs Apply on h until the transaction reaches step, as a power cut
// would leave it: nothing after that point runs, and nothing is cleaned up.
func cutAt(t *testing.T, h *harness, step string) {
	t.Helper()
	h.u.crash = func(s string) bool { return s == step }
	defer func() {
		h.u.crash, h.u.poweredOff = nil, false
		r := recover()
		if pc, ok := r.(PowerCut); !ok || pc.Step != step {
			t.Fatalf("Apply did not reach %q (recovered %v)", step, r)
		}
	}()
	h.u.Apply(context.Background())
}

// reboot is the board starting again: systemd starts whatever agent is
// installed, and the path unit starts the updater, which is that same binary
// (so its CurrentVersion is the installed one), if the record or the `ready`
// marker is there.
func (h *harness) reboot(t *testing.T) (*Updater, bool) {
	t.Helper()
	h.now = h.now.Add(time.Minute)
	h.agent.restarts = 0
	h.start()
	u := &Updater{Layout: h.u.Layout, TrustPath: h.u.TrustPath, Binary: h.u.Binary, NewBinary: h.u.NewBinary,
		Prev: h.u.Prev, TempRoot: h.u.TempRoot, Arch: h.u.Arch, CurrentVersion: h.agent.version, Logf: t.Logf}
	h.wire(u)
	u.defaults()
	triggered := exists(u.Txn) || exists(u.Layout.StagedReady())
	return u, triggered
}

func TestPowerCutAtEveryStep(t *testing.T) {
	type want struct {
		binary, status string
		rejected       bool
	}
	installed := want{"new agent 0.4.0", StatusOK, false}
	untouched := want{oldBinary, StatusRefused, false}
	rolledBack := want{oldBinary, StatusRolledBack, true}
	cases := []struct {
		step string
		bad  bool // the new agent never comes up
		want want
	}{
		{"recorded", false, untouched},
		{"marked", false, untouched},
		{"swapped", false, installed},
		{"probation", false, installed},
		{"committing", false, installed},
		{"committed", false, installed},
		{"swapped", true, rolledBack},
		{"probation", true, rolledBack},
		{"rolling-back", true, rolledBack},
		{"restored", true, rolledBack},
		{"rolled-back", true, rolledBack},
		{"closed", true, rolledBack},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("%s/bad=%v", tc.step, tc.bad), func(t *testing.T) {
			h := newHarness(t, true)
			h.agent.downNew = tc.bad
			h.stage(t, "0.4.0", "arm64", releaseKey)
			cutAt(t, h, tc.step)

			u, triggered := h.reboot(t)
			if tc.step != "closed" && !triggered {
				t.Fatal("nothing would start the updater at boot")
			}
			if triggered {
				out, err := u.Apply(context.Background())
				t.Logf("after reboot: %+v, %v", out, err)
				if (errors.Is(err, ErrNothingStaged) && tc.step != "closed") || errors.Is(err, ErrInterrupted) {
					t.Fatalf("not resolved: %v", err)
				}
			}
			if got := read(t, u.Binary); got != tc.want.binary {
				t.Errorf("binary = %q, want %q", got, tc.want.binary)
			}
			if o := lastUpdate(t, u); o.Status != tc.want.status || o.To != "0.4.0" || o.From != "0.3.0" {
				t.Errorf("last-update = %+v, want status %s", o, tc.want.status)
			}
			if got := rejected(t, u, "0.4.0"); got != tc.want.rejected {
				t.Errorf("rejected = %v, want %v", got, tc.want.rejected)
			}
			h.u = u
			h.settled(t)
			if h.agent.version != versionOf(tc.want.binary) {
				t.Errorf("agent runs %s", h.agent.version)
			}
			// Resolved for good: another run finds nothing to do.
			if _, err := u.Apply(context.Background()); !errors.Is(err, ErrNothingStaged) {
				t.Errorf("second run: %v", err)
			}
		})
	}
}

func TestOutcomeOfAResumedCommitIsTheSame(t *testing.T) {
	h := newHarness(t, true)
	h.stage(t, "0.4.0", "arm64", releaseKey)
	cutAt(t, h, "committed")
	first := lastUpdate(t, h.u)
	u, _ := h.reboot(t)
	if _, err := u.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	// agentupdate shows an outcome once, by its timestamp.
	if again := lastUpdate(t, u); again != first {
		t.Errorf("outcome rewritten: %+v, was %+v", again, first)
	}
}

func TestProbationCutShortRepeatedlyRollsBack(t *testing.T) {
	h := newHarness(t, true)
	h.stage(t, "0.4.0", "arm64", releaseKey)
	cutAt(t, h, "swapped")
	for i := 0; i < MaxProbationAttempts; i++ {
		u, _ := h.reboot(t)
		h.u = u
		cutAt(t, h, "probation")
	}
	u, _ := h.reboot(t)
	out, err := u.Apply(context.Background())
	if err == nil || out.Status != StatusRolledBack || !strings.Contains(out.Message, "cut short") {
		t.Fatalf("outcome = %+v, err = %v", out, err)
	}
	if read(t, u.Binary) != oldBinary || !rejected(t, u, "0.4.0") {
		t.Error("not rolled back and rejected")
	}
}

// SIGTERM (shutdown, or the unit's start timeout) mid-probation leaves the
// update to the next run, which judges it again.
func TestApplyInterruptedIsResumed(t *testing.T) {
	h := newHarness(t, true)
	h.stage(t, "0.4.0", "arm64", releaseKey)
	ctx, cancel := context.WithCancel(context.Background())
	h.tick = func(h *harness) {
		if h.isNew() && h.now.Sub(h.agent.startedAt) > time.Minute {
			cancel()
		}
	}
	_, err := h.u.Apply(ctx)
	if !errors.Is(err, ErrInterrupted) {
		t.Fatalf("err = %v", err)
	}
	if !exists(h.u.Txn) || !exists(h.u.Layout.StagedReady()) {
		t.Fatal("record or trigger missing after an interruption")
	}
	if exists(h.u.Layout.AgentLastUpdate()) {
		t.Error("an outcome was written for an unjudged update")
	}
	h.tick = nil
	// The unit's timeout, not a reboot: the agent kept running.
	u := *h.u
	u.CurrentVersion = "0.4.0"
	out, err := u.Apply(context.Background())
	if err != nil || out.Status != StatusOK {
		t.Fatalf("resumed: %+v, %v", out, err)
	}
	if h.count("systemctl restart "+AgentUnit) != 1 {
		t.Errorf("a resumed probation restarted the agent: %q", h.cmds)
	}
	h.settled(t)
}

func TestUnreadableRecordRestoresPrevious(t *testing.T) {
	h := newHarness(t, true)
	h.stage(t, "0.4.0", "arm64", releaseKey)
	cutAt(t, h, "probation")
	os.WriteFile(h.u.Txn, []byte("{garbage"), 0o644)
	u, _ := h.reboot(t)
	out, err := u.Apply(context.Background())
	if err == nil || out.Status != StatusRolledBack {
		t.Fatalf("outcome = %+v, err = %v", out, err)
	}
	if read(t, u.Binary) != oldBinary || exists(u.Txn) {
		t.Error("previous agent not restored, or record left")
	}
}

func TestBinaryReplacedByHandIsLeftAlone(t *testing.T) {
	h := newHarness(t, true)
	h.stage(t, "0.4.0", "arm64", releaseKey)
	cutAt(t, h, "probation")
	os.WriteFile(h.u.Binary, []byte("new agent 0.9.0"), 0o755)
	u, _ := h.reboot(t)
	out, err := u.Apply(context.Background())
	if err == nil || out.Status != StatusRefused || !strings.Contains(out.Message, "left as it is") {
		t.Fatalf("outcome = %+v, err = %v", out, err)
	}
	if read(t, u.Binary) != "new agent 0.9.0" || exists(u.Txn) {
		t.Error("hand-installed agent touched, or record left")
	}
}

func TestStrikesAreIdempotent(t *testing.T) {
	p := filepath.Join(t.TempDir(), "s.json")
	for _, id := range []string{"a", "a", "b"} {
		addStrike(p, "1.0.0", id)
	}
	if n, _ := addStrike(p, "1.0.0", "b"); n != 2 {
		t.Errorf("strikes = %d, want 2", n)
	}
	if s := loadStrikes(p); !slices.Equal(s["1.0.0"], []string{"a", "b"}) {
		t.Errorf("strikes = %v", s)
	}
}

func TestServiceState(t *testing.T) {
	u := &Updater{Run: func(ctx context.Context, name string, args ...string) (string, error) {
		return "ActiveState=active\nMainPID=812\nNRestarts=2\n", nil
	}}
	s, err := u.serviceState(context.Background())
	if err != nil || s != (ServiceState{"active", 812, 2}) {
		t.Fatalf("%+v %v", s, err)
	}
	u.Run = func(ctx context.Context, name string, args ...string) (string, error) { return "MainPID=1", nil }
	if _, err := u.serviceState(context.Background()); err == nil {
		t.Fatal("partial answer accepted")
	}
}

func TestFetchStatus(t *testing.T) {
	body := `{"localApi":2,"agentVersion":"0.4.0","backend":{"connected":false},"sync":{"enabled":true,"lastSuccessAt":"2026-09-24T23:01:00Z","lastError":null}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body) }))
	defer srv.Close()
	st, err := FetchStatus(context.Background(), srv.URL)
	if err != nil || st.AgentVersion != "0.4.0" || !st.BackendUp() {
		t.Fatalf("%+v %v", st, err)
	}
	for _, b := range []string{
		`{"agentVersion":"0.4.0"}`,
		`{"agentVersion":"0.4.0","backend":{"connected":false},"sync":{"lastSuccessAt":"x","lastError":"timeout"}}`,
		`{"agentVersion":"0.4.0","sync":{"enabled":false,"lastError":"device key unreadable"}}`,
	} {
		body = b
		if st, err := FetchStatus(context.Background(), srv.URL); err != nil || st.BackendUp() {
			t.Errorf("%s: up=%v err=%v", b, st.BackendUp(), err)
		}
	}
	body = `{"agentVersion":"0.4.0","backend":{"connected":true}}`
	if st, _ := FetchStatus(context.Background(), srv.URL); !st.BackendUp() {
		t.Error("connected backend not up")
	}
}
