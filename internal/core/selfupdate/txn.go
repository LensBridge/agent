package selfupdate

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"

	"github.com/LensBridge/agent/internal/core/fsutil"
)

// The update transaction (docs/architecture.md, section 12).
//
// Before the updater touches /usr/bin/musallahboard-agent it writes a record
// of what it is about to do to a root-owned file beside agent.prev. The
// record is the transaction's journal: it is rewritten (temp file, fsync,
// rename, fsync of the directory) at every change of phase, and removed only
// once the outcome has been written. Whoever finds a record (the updater that
// wrote it, or the next one to start, after a crash, a timeout or a power
// cut) finishes the transaction from it. Which way it goes is decided by the
// phase and by what is actually installed, compared by SHA-256 with the two
// binaries the record names, so the answer does not depend on how far the
// interrupted run got.
//
// The record lives in /usr/lib/musallahboard, not under /var/lib/musallahboard:
// the daemon owns the latter and must not be able to forge, edit or delete
// the journal of a root operation.

// Phases of the transaction, in order.
const (
	// PhaseSwapping: agent.prev holds the running agent and the new binary
	// is extracted; it may or may not have been renamed into place yet.
	PhaseSwapping = "swapping"
	// PhaseProbation: the new binary is in place and is being judged.
	PhaseProbation = "probation"
	// PhaseCommitting: the new agent passed; the outcome is being written.
	PhaseCommitting = "committing"
	// PhaseRollingBack: the new agent failed; agent.prev is being restored.
	PhaseRollingBack = "rolling-back"
)

// txnFormat is the record's format. A newer agent may resume a record an
// older one wrote (after a power cut, the binary that starts is the new one),
// so fields are only ever added.
const txnFormat = 1

// Txn is the on-disk update record.
type Txn struct {
	Format int    `json:"format"`
	Phase  string `json:"phase"`
	From   string `json:"from"`
	To     string `json:"to"`
	// PrevSHA256 is the agent that ran before the update, as kept in
	// agent.prev; NewSHA256 is the binary being installed.
	PrevSHA256 string `json:"prevSha256"`
	NewSHA256  string `json:"newSha256"`
	// BackendBefore is whether the board was connected to LensBridge (an
	// authenticated WebSocket, or a content sync that worked) just before
	// the update. Only then must the new agent connect to pass.
	BackendBefore bool `json:"backendBefore"`
	// Attempts counts the probations started for this update. A probation
	// cut short (power cut, reboot, the updater being stopped) is started
	// again from scratch, at most MaxProbationAttempts times.
	Attempts  int    `json:"attempts"`
	StartedAt string `json:"startedAt"`
	// Reason and Reject are set when rolling back: why, and whether the
	// version is to be recorded as rejected.
	Reason string `json:"reason,omitempty"`
	Reject bool   `json:"reject,omitempty"`
	// OutcomeAt is the outcome's timestamp, fixed once committing or
	// rolling back starts so that a resumed run writes the same outcome.
	OutcomeAt string `json:"outcomeAt,omitempty"`
}

func loadTxn(path string) (*Txn, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var t Txn
	if err := json.Unmarshal(raw, &t); err != nil {
		return nil, fmt.Errorf("%s is unreadable: %w", path, err)
	}
	return &t, nil
}

func saveTxn(path string, t *Txn) error {
	t.Format = txnFormat
	raw, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return fsutil.WriteAtomic(path, append(raw, '\n'), 0o644)
}

// removeDurable removes path and makes the removal durable.
func removeDurable(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return fsutil.SyncDir(filepath.Dir(path))
}

// fileSHA256 is the hex SHA-256 of the file at path.
func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// strikes records, per agent version, the updates that were rolled back only
// because the new agent did not reconnect to LensBridge. One such failure may
// be the backend's or the network's fault, so it does not reject the version;
// a second one does (StrikesToReject). Keyed by the transaction's StartedAt so
// that resuming an interrupted rollback does not count it twice.
type strikes map[string][]string

// StrikesToReject is how many connection-only failures reject a version.
const StrikesToReject = 2

func loadStrikes(path string) strikes {
	s := strikes{}
	if raw, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(raw, &s)
	}
	return s
}

// addStrike records a connection-only failure of version and returns how
// many there have been.
func addStrike(path, version, id string) (int, error) {
	s := loadStrikes(path)
	if !slices.Contains(s[version], id) {
		s[version] = append(s[version], id)
	}
	raw, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return len(s[version]), err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return len(s[version]), err
	}
	return len(s[version]), fsutil.WriteAtomic(path, append(raw, '\n'), 0o644)
}
