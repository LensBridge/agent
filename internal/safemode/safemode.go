package safemode

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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
// If the counter exceeds safeModeThreshold, the agent should boot in
// heartbeat-only mode: still report in (so admins see the device is
// degraded) but refuse to execute commands. This complements systemd's
// StartLimitBurst, which stops the death-loop entirely.
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
	tmp := m.path + ".tmp"
	if err := os.WriteFile(tmp, []byte(fmt.Sprintf("%d\n", n)), 0o644); err != nil {
		return
	}
	_ = os.Rename(tmp, m.path)
}
