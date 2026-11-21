package state

import (
	"os"
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

func TestCapClockFloor(t *testing.T) {
	const ntp = int64(1_795_000_000)
	cases := []struct {
		name                        string
		floor, signed               int64
		wantFloor, wantSigned       int64
		wantLowered, wantFileExists bool
	}{
		{"nothing recorded", 0, 0, 0, 0, false, false},
		{"both behind", ntp - 100, ntp - 1000, ntp - 100, ntp - 1000, false, true},
		{"floor ahead", ntp + 86400*300, ntp - 1000, ntp, ntp - 1000, true, true},
		{"signed ahead too", ntp + 86400*300, ntp + 86400*1500, ntp, ntp, true, true},
		{"exactly at", ntp, ntp, ntp, ntp, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := File{Path: filepath.Join(t.TempDir(), "state.json")}
			if tc.floor > 0 || tc.signed > 0 {
				if _, err := f.Update(func(s *State) error {
					s.ClockFloor, s.SignedClockFloor, s.ContentSequence = tc.floor, tc.signed, 7
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}
			before, lowered, err := f.CapClockFloor(ntp)
			if err != nil {
				t.Fatal(err)
			}
			if lowered != tc.wantLowered {
				t.Fatalf("lowered = %v, want %v", lowered, tc.wantLowered)
			}
			if before.ClockFloor != tc.floor || before.SignedClockFloor != tc.signed {
				t.Fatalf("before = %+v", before)
			}
			s, _ := f.Load()
			if s.ClockFloor != tc.wantFloor || s.SignedClockFloor != tc.wantSigned {
				t.Fatalf("floor, signed = %d, %d; want %d, %d", s.ClockFloor, s.SignedClockFloor, tc.wantFloor, tc.wantSigned)
			}
			if tc.wantFileExists && s.ContentSequence != 7 {
				t.Fatalf("other fields lost: %+v", s)
			}
			if _, err := os.Stat(f.Path); (err == nil) != tc.wantFileExists {
				t.Fatalf("state.json exists = %v, want %v", err == nil, tc.wantFileExists)
			}
		})
	}
}
