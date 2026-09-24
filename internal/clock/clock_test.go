package clock

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/LensBridge/agent/internal/importer"
	"github.com/LensBridge/agent/internal/state"
	"github.com/LensBridge/agent/internal/store"
)

type fake struct {
	now  time.Time
	sets []time.Time
	rtc  int
	fail error
}

func newKeeper(t *testing.T, floor int64, now time.Time, withRTC bool) (*Keeper, *fake) {
	t.Helper()
	l := store.Layout{Root: t.TempDir()}
	if floor > 0 {
		if _, err := l.State().Update(func(s *state.State) error { s.ClockFloor = floor; return nil }); err != nil {
			t.Fatal(err)
		}
	}
	f := &fake{now: now}
	k := New(l, nil)
	k.Now = func() time.Time { return f.now }
	k.SetClock = func(t time.Time) error {
		if f.fail != nil {
			return f.fail
		}
		f.sets = append(f.sets, t)
		f.now = t
		return nil
	}
	k.WriteRTC = func() error { f.rtc++; return nil }
	k.RTCDevice = filepath.Join(t.TempDir(), "rtc0")
	if withRTC {
		os.WriteFile(k.RTCDevice, nil, 0o644)
	}
	return k, f
}

func floorOf(t *testing.T, k *Keeper) int64 {
	st, err := k.layout.State().Load()
	if err != nil {
		t.Fatal(err)
	}
	return st.ClockFloor
}

var base = time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)

func TestCheckPersistsWallClock(t *testing.T) {
	k, f := newKeeper(t, 0, base, false)
	k.Check()
	if len(f.sets) != 0 {
		t.Fatalf("clock set with no floor: %v", f.sets)
	}
	if got := floorOf(t, k); got != base.Unix() {
		t.Fatalf("floor = %d, want %d", got, base.Unix())
	}
}

func TestCheckSetsClockBehindFloor(t *testing.T) {
	floor := base.Unix()
	k, f := newKeeper(t, floor, base.Add(-2*time.Hour), true)
	k.Check()
	if len(f.sets) != 1 || f.sets[0].Unix() != floor {
		t.Fatalf("sets = %v, want one set to the floor", f.sets)
	}
	if f.rtc != 1 {
		t.Fatalf("RTC written %d times, want 1", f.rtc)
	}
}

func TestCheckToleratesSmallLag(t *testing.T) {
	k, f := newKeeper(t, base.Unix(), base.Add(-30*time.Second), true)
	k.Check()
	if len(f.sets) != 0 {
		t.Fatalf("clock set for a 30 s lag: %v", f.sets)
	}
	if got := floorOf(t, k); got != base.Unix() {
		t.Fatalf("floor moved down to %d", got)
	}
}

func TestCheckNoRTC(t *testing.T) {
	k, f := newKeeper(t, base.Unix(), base.Add(-time.Hour), false)
	k.Check()
	if len(f.sets) != 1 || f.rtc != 0 {
		t.Fatalf("sets=%v rtc=%d", f.sets, f.rtc)
	}
}

func TestApplyClientTime(t *testing.T) {
	floor := base.Unix()
	verified := importer.Batch{Verified: true}
	cases := []struct {
		name     string
		now      time.Time
		client   int64
		batch    importer.Batch
		adjusted bool
		drift    int64
	}{
		{"within tolerance", base, floor + 4, verified, false, -4},
		{"not verified", base, floor + 3600, importer.Batch{}, false, -3600},
		{"behind floor", base.Add(time.Hour), floor - 10, verified, false, 3610},
		{"beyond 90 days", base, floor + int64(91*24*3600), verified, false, -int64(91 * 24 * 3600)},
		{"board behind", base.Add(-time.Hour), floor + 60, verified, true, -3660},
		{"board ahead, client at floor", base.Add(time.Hour), floor, verified, true, 3600},
		{"exactly 90 days", base, floor + int64(90*24*3600), verified, true, -int64(90 * 24 * 3600)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			k, f := newKeeper(t, floor, tc.now, false)
			rep := k.ApplyClientTime(tc.client, tc.batch)
			if rep.Adjusted != tc.adjusted || rep.DriftSeconds != tc.drift {
				t.Fatalf("report = %+v, want adjusted=%v drift=%d", rep, tc.adjusted, tc.drift)
			}
			if rep.Note == "" {
				t.Error("empty note")
			}
			if tc.adjusted && (len(f.sets) != 1 || f.sets[0].Unix() != tc.client) {
				t.Fatalf("sets = %v", f.sets)
			}
			if !tc.adjusted && len(f.sets) != 0 {
				t.Fatalf("clock set: %v", f.sets)
			}
		})
	}
}

func TestApplyClientTimeSetFails(t *testing.T) {
	k, f := newKeeper(t, base.Unix(), base.Add(-time.Hour), false)
	f.fail = errors.New("EPERM")
	rep := k.ApplyClientTime(base.Unix()+10, importer.Batch{Verified: true})
	if rep.Adjusted {
		t.Fatal("reported adjusted after a failed set")
	}
}
