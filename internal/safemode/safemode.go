// Package safemode counts agent starts that did not lead to a stable run.
package safemode

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/LensBridge/agent/internal/fsutil"
)

const (
	crashCounterFile  = "crash-counter"
	safeModeThreshold = 3
)

// Manager tracks consecutive failed agent runs via a file at
// $stateDir/crash-counter. Each startup increments it; OnSuccessfulRun
// resets it after the agent has been up long enough to be considered
// stable (typically 5 minutes — caller schedules that).
//
// If the counter exceeds safeModeThreshold, the agent is "in safe mode".
// That is a report, nothing more: the flag goes out with every backend
// heartbeat (telemetry safeMode) so the admin portal shows that the board is
// restarting over and over. The agent does not behave differently in safe
// mode, it does not refuse commands, and nothing stops the restarts:
// musallahboard-agent.service restarts the agent for ever
// (StartLimitIntervalSec=0), with a growing delay. A new agent that crashes
// in its first minutes after a self-update is rolled back by the root
// updater's probation (package selfupdate), independently of this counter.
type Manager struct {
	path string
}

func New(stateDir string) *Manager {
	return &Manager{path: filepath.Join(stateDir, crashCounterFile)}
}

// OnStartup increments the crash counter and returns true if the agent
// should boot in safe mode.
func (m *Manager) OnStartup() bool {
	n := m.read()
	n++
	m.write(n)
	return n > safeModeThreshold
}

// OnSuccessfulRun resets the crash counter — call this after the agent
// has been running long enough that we consider it stable.
func (m *Manager) OnSuccessfulRun() {
	m.write(0)
}

func (m *Manager) read() int {
	b, err := os.ReadFile(m.path)
	if err != nil {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		return 0
	}
	return n
}

func (m *Manager) write(n int) {
	if err := os.MkdirAll(filepath.Dir(m.path), 0o755); err != nil {
		return
	}
	_ = fsutil.WriteAtomic(m.path, []byte(fmt.Sprintf("%d\n", n)), 0o644)
}
