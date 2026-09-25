// Package state persists the board's high-water marks (docs/architecture.md,
// section 6): the newest content sequence and app version ever installed, the
// agent versions that failed to start, and the clock floor.
//
// These live in their own file, not only in the installed directories, so
// that deleting a bundle can never reopen a rollback. The daemon and the root
// self-updater both write it; every write is a locked read-modify-write and an
// atomic replace.
package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"

	"github.com/LensBridge/agent/internal/fsutil"
)

// State is state.json.
type State struct {
	ContentSequence       int64    `json:"contentSequence"`
	AppVersion            string   `json:"appVersion,omitempty"`
	AgentVersionsRejected []string `json:"agentVersionsRejected,omitempty"`
	// ClockFloor is Unix seconds; the board's clock is never allowed behind it.
	ClockFloor int64 `json:"clockFloor"`
	// SignedClockFloor is the newest createdAt of any verified package: the
	// part of the floor that is proven rather than remembered.
	SignedClockFloor int64 `json:"signedClockFloor,omitempty"`
	// AgentUpdateSeen is the "at" of the last agent self-update outcome the
	// board has shown, so each is shown once.
	AgentUpdateSeen string `json:"agentUpdateSeen,omitempty"`
}

// MaxWallAheadOfSigned bounds how far a remembered wall clock may raise the
// floor past the newest signed time. Without it, a clock once set far in the
// future (a bad manual date, a broken time source) would raise the floor for
// good, and nothing could bring the board back.
const MaxWallAheadOfSigned = 400 * 24 * 60 * 60

// File is state.json at a path.
type File struct{ Path string }

// Load reads the state. A missing file is the zero state.
func (f File) Load() (State, error) {
	var s State
	raw, err := os.ReadFile(f.Path)
	if errors.Is(err, fs.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		return s, fmt.Errorf("%s is corrupt: %w", f.Path, err)
	}
	return s, nil
}

// Update applies fn to the current state under an exclusive lock and writes
// the result atomically. fn's error aborts without writing.
func (f File) Update(fn func(*State) error) (State, error) {
	dir := filepath.Dir(f.Path)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return State{}, err
	}
	unlock, err := lockBlocking(filepath.Join(dir, ".state.lock"))
	if err != nil {
		return State{}, err
	}
	defer unlock()
	s, err := f.Load()
	if err != nil {
		return s, err
	}
	if err := fn(&s); err != nil {
		return s, err
	}
	raw, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return s, err
	}
	mode := os.FileMode(0o640)
	if fi, err := os.Stat(f.Path); err == nil {
		mode = fi.Mode().Perm()
	}
	return s, fsutil.WriteAtomic(f.Path, append(raw, '\n'), mode)
}

// RaiseClockFloor moves the floor up to a remembered wall-clock time unix,
// never down, and never more than MaxWallAheadOfSigned past the newest signed
// time (when there is one).
func (f File) RaiseClockFloor(unix int64) error {
	_, err := f.Update(func(s *State) error {
		if s.SignedClockFloor > 0 && unix > s.SignedClockFloor+MaxWallAheadOfSigned {
			unix = s.SignedClockFloor + MaxWallAheadOfSigned
		}
		if unix > s.ClockFloor {
			s.ClockFloor = unix
		}
		return nil
	})
	return err
}

// RaiseSignedClockFloor records a verified package's createdAt, which also
// raises the floor.
func (f File) RaiseSignedClockFloor(unix int64) error {
	_, err := f.Update(func(s *State) error {
		if unix > s.SignedClockFloor {
			s.SignedClockFloor = unix
		}
		if unix > s.ClockFloor {
			s.ClockFloor = unix
		}
		return nil
	})
	return err
}

// AgentRejected reports whether version failed a self-update before.
func (s State) AgentRejected(version string) bool {
	return slices.Contains(s.AgentVersionsRejected, version)
}
