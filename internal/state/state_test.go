package state

import (
	"path/filepath"
	"testing"
)

func TestWallClockCannotRatchetFarPastSignedTime(t *testing.T) {
	f := File{Path: filepath.Join(t.TempDir(), "state.json")}
	signed := int64(1_790_000_000)
	if err := f.RaiseSignedClockFloor(signed); err != nil {
		t.Fatal(err)
	}
	// A clock set ten years ahead once.
	if err := f.RaiseClockFloor(signed + 10*365*86400); err != nil {
		t.Fatal(err)
	}
	s, _ := f.Load()
	if s.ClockFloor != signed+MaxWallAheadOfSigned {
		t.Fatalf("floor = %d, want capped at %d", s.ClockFloor, signed+MaxWallAheadOfSigned)
	}
	// Never down.
	f.RaiseClockFloor(signed - 100)
	if s2, _ := f.Load(); s2.ClockFloor != s.ClockFloor {
		t.Fatal("floor went down")
	}
}
