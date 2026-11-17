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
	mono time.Duration
	ntp  bool
	sets []time.Time
	rtc  int
	fail error
}

// tick lets d of real time pass: the wall and the monotonic clock both move.
func (f *fake) tick(d time.Duration) {
	f.now = f.now.Add(d)
	f.mono += d
}

func newKeeper(t *testing.T, floor int64, now time.Time, withRTC bool) (*Keeper, *fake) {
	t.Helper()
	l := store.Layout{Root: t.TempDir()}
	if floor > 0 {
		if _, err := l.State().Update(func(s *state.State) error { s.ClockFloor = floor; return nil }); err != nil {
			t.Fatal(err)
		}
	}
	return keeperOn(t, l, now, withRTC)
}

// keeperOn is a fresh agent process on an existing board: same state.json,
// new monotonic clock.
func keeperOn(t *testing.T, l store.Layout, now time.Time, withRTC bool) (*Keeper, *fake) {
	t.Helper()
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
	k.NTPSynced = func() bool { return f.ntp }
	k.Monotonic = func() time.Duration { return f.mono }
	k.Uptime = func() time.Duration { return time.Hour }
	k.BootID = func() string { return "boot-1" }
	k.RTCDevice = filepath.Join(t.TempDir(), "rtc0")
	if withRTC {
		os.WriteFile(k.RTCDevice, nil, 0o644)
	}
	return k, f
}

func floorOf(t *testing.T, k *Keeper) int64 {
	return stateOf(t, k).ClockFloor
}

func stateOf(t *testing.T, k *Keeper) state.State {
	t.Helper()
	st, err := k.layout.State().Load()
	if err != nil {
		t.Fatal(err)
	}
	return st
}

// setState writes the floors as an earlier agent left them.
func setState(t *testing.T, k *Keeper, floor, signed int64) {
	t.Helper()
	if _, err := k.layout.State().Update(func(s *state.State) error {
		s.ClockFloor, s.SignedClockFloor = floor, signed
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

var base = time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)

func TestCheckWithoutFloorOrNTPPersistsNothing(t *testing.T) {
	k, f := newKeeper(t, 0, base, false)
	k.Check()
	f.tick(Interval)
	k.Check()
	if len(f.sets) != 0 {
		t.Fatalf("clock set with no floor: %v", f.sets)
	}
	if got := floorOf(t, k); got != 0 {
		t.Fatalf("an unconfirmed wall clock became the floor: %d", got)
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
		ntp      bool
		client   int64
		batch    importer.Batch
		adjusted bool
		drift    int64
	}{
		{"within tolerance", base, false, floor + 4, verified, false, -4},
		{"not verified", base, false, floor + 3600, importer.Batch{}, false, -3600},
		{"behind floor", base.Add(time.Hour), false, floor - 10, verified, false, 3610},
		{"beyond 90 days", base, false, floor + int64(91*24*3600), verified, false, -int64(91 * 24 * 3600)},
		{"board behind", base.Add(-time.Hour), false, floor + 60, verified, true, -3660},
		{"board ahead, client at floor", base.Add(time.Hour), false, floor, verified, true, 3600},
		{"exactly 90 days", base, false, floor + int64(90*24*3600), verified, true, -int64(90 * 24 * 3600)},
		// NTP is the authority: an unauthenticated uploader never moves it.
		{"ntp, laptop 60 days ahead", base, true, floor + int64(60*24*3600), verified, false, -int64(60 * 24 * 3600)},
		{"ntp, laptop an hour behind", base, true, floor - 3600, verified, false, 3600},
		{"ntp, board an hour behind the laptop", base.Add(-time.Hour), true, floor, verified, false, -3600},
		{"ntp, within tolerance", base, true, floor + 3, verified, false, -3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			k, f := newKeeper(t, floor, tc.now, false)
			f.ntp = tc.ntp
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
			// An uploader's time never becomes the floor.
			if got := floorOf(t, k); got != floor {
				t.Fatalf("floor = %d, want %d unchanged", got, floor)
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

func TestInfo(t *testing.T) {
	k, _ := newKeeper(t, base.Unix(), base, false)
	if i := k.Info(); i.Source != SourceUnverified || i.Trusted {
		t.Fatalf("no NTP, RTC or uploader: %+v", i)
	}

	k.Uptime = func() time.Duration { return time.Minute }
	if i := k.Info(); i.Source != SourceStarting || !i.Trusted {
		t.Fatalf("just booted: %+v", i)
	}
	k.Uptime = func() time.Duration { return time.Hour }

	// A laptop's time counts, until the next boot.
	k.ApplyClientTime(base.Unix()+2, importer.Batch{Verified: true})
	if i := k.Info(); i.Source != SourceUploader || !i.Trusted {
		t.Fatalf("after a verified upload: %+v", i)
	}
	k.BootID = func() string { return "boot-2" }
	if i := k.Info(); i.Trusted {
		t.Fatalf("an uploader's time survived a reboot: %+v", i)
	}

	// An unverified upload proves nothing.
	k.ApplyClientTime(base.Unix()+2, importer.Batch{})
	if i := k.Info(); i.Trusted {
		t.Fatalf("after an unverified upload: %+v", i)
	}

	os.WriteFile(k.RTCDevice, nil, 0o644)
	if i := k.Info(); i.Source != SourceRTC || !i.Trusted {
		t.Fatalf("with an RTC: %+v", i)
	}
	k.NTPSynced = func() bool { return true }
	if i := k.Info(); i.Source != SourceNTP {
		t.Fatalf("with NTP: %+v", i)
	}
}

// checks runs n Checks an Interval apart, starting now.
func checks(k *Keeper, f *fake, n int) {
	for i := 0; i < n; i++ {
		if i > 0 {
			f.tick(Interval)
		}
		k.Check()
	}
}

// Without NTP nothing has confirmed the wall clock, so however it got ahead
// (a manual date, a bad RTC, an uploader) it must not become the floor. The
// floor advances by the time that really passed since the last trusted time.
func TestUnconfirmedClockAheadDoesNotRaiseFloor(t *testing.T) {
	cases := []struct {
		name    string
		withRTC bool
		ahead   func(k *Keeper, f *fake)
	}{
		{"manual date", false, func(k *Keeper, f *fake) { f.now = f.now.Add(365 * 24 * time.Hour) }},
		{"wrong RTC", true, func(k *Keeper, f *fake) { f.now = f.now.Add(200 * 24 * time.Hour) }},
		{"uploader", false, func(k *Keeper, f *fake) {
			if rep := k.ApplyClientTime(f.now.Add(60*24*time.Hour).Unix(), importer.Batch{Verified: true}); !rep.Adjusted {
				panic("uploader time not applied: " + rep.Note)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			k, f := newKeeper(t, base.Unix(), base, tc.withRTC)
			k.Check()
			tc.ahead(k, f)
			sets := len(f.sets)
			f.tick(Interval)
			checks(k, f, 3)
			if got, want := floorOf(t, k), base.Add(3*Interval).Unix(); got != want {
				t.Fatalf("floor = %s, want %s (trusted time carried forward, not the wall clock)",
					time.Unix(got, 0).UTC(), time.Unix(want, 0).UTC())
			}
			if len(f.sets) != sets {
				t.Fatalf("clock set again: %v", f.sets[sets:])
			}
		})
	}
}

// The floor's purpose without NTP: a board that loses power comes back no
// earlier than the time it last knew, carried forward while it ran.
func TestFloorSurvivesPowerCutWithoutNTP(t *testing.T) {
	k, f := newKeeper(t, base.Unix(), base, false)
	checks(k, f, 7) // an hour
	if got, want := floorOf(t, k), base.Add(time.Hour).Unix(); got != want {
		t.Fatalf("floor = %d, want %d", got, want)
	}
	// Power cut. No RTC, so the clock comes up wherever.
	k2, f2 := keeperOn(t, k.layout, time.Unix(0, 0), false)
	k2.Check()
	if len(f2.sets) != 1 || f2.sets[0].Unix() != base.Add(time.Hour).Unix() {
		t.Fatalf("sets = %v, want the floor", f2.sets)
	}
	checks(k2, f2, 2)
	if got, want := floorOf(t, k2), base.Add(time.Hour+Interval).Unix(); got != want {
		t.Fatalf("floor after restart = %d, want %d", got, want)
	}
}

// A verified package's createdAt raises the floor while the keeper runs; the
// keeper carries the floor forward from there, not from its older anchor.
func TestSignedTimeReanchors(t *testing.T) {
	k, f := newKeeper(t, base.Unix(), base, false)
	k.Check()
	if err := k.layout.State().RaiseSignedClockFloor(base.Add(5 * time.Hour).Unix()); err != nil {
		t.Fatal(err)
	}
	f.tick(Interval)
	k.Check() // the clock is ~5 h behind the signed time: moved forward
	if len(f.sets) != 1 || f.sets[0].Unix() != base.Add(5*time.Hour).Unix() {
		t.Fatalf("sets = %v", f.sets)
	}
	f.tick(Interval)
	k.Check()
	if got, want := floorOf(t, k), base.Add(5*time.Hour+Interval).Unix(); got != want {
		t.Fatalf("floor = %d, want %d", got, want)
	}
}

// With NTP the keeper never moves the clock, whatever the floor says, and the
// floor follows NTP time.
func TestNTPIsTheAuthority(t *testing.T) {
	cases := []struct {
		name              string
		floor, signed     time.Time
		wantFloorAtNTP    bool
		wantSignedLowered bool
	}{
		{"floor behind", base.Add(-time.Hour), base.Add(-2 * time.Hour), false, false},
		{"floor a few seconds ahead", base.Add(30 * time.Second), base.Add(-time.Hour), false, false},
		{"floor ahead", base.Add(300 * 24 * time.Hour), base.Add(-time.Hour), true, false},
		{"signed floor ahead too", base.Add(400 * 24 * time.Hour), base.Add(4 * 365 * 24 * time.Hour), true, true},
		{"floor behind, signed ahead", base.Add(-time.Hour), base.Add(2 * time.Hour), true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			k, f := newKeeper(t, 0, base, true)
			setState(t, k, tc.floor.Unix(), tc.signed.Unix())
			f.ntp = true
			k.Check()
			if st := stateOf(t, k); st.ClockFloor != tc.floor.Unix() || st.SignedClockFloor != tc.signed.Unix() {
				t.Fatalf("first NTP check changed the floor: %+v", st)
			}
			checks(k, f, 1+6)
			if len(f.sets) != 0 || f.rtc != 0 {
				t.Fatalf("the clock was moved against NTP: sets=%v rtc=%d", f.sets, f.rtc)
			}
			st := stateOf(t, k)
			if want := f.now.Unix(); tc.wantFloorAtNTP && st.ClockFloor != want {
				t.Fatalf("floor = %s, want lowered to NTP time %s", time.Unix(st.ClockFloor, 0).UTC(), f.now.UTC())
			}
			if !tc.wantFloorAtNTP && st.ClockFloor < tc.floor.Unix() {
				t.Fatalf("floor lowered: %d < %d", st.ClockFloor, tc.floor.Unix())
			}
			if st.ClockFloor < f.now.Unix()-1 && st.ClockFloor < tc.floor.Unix() {
				t.Fatalf("floor %d does not follow NTP time %d", st.ClockFloor, f.now.Unix())
			}
			if tc.wantSignedLowered != (st.SignedClockFloor != tc.signed.Unix()) {
				t.Fatalf("signed floor = %d (was %d), lowered want %v", st.SignedClockFloor, tc.signed.Unix(), tc.wantSignedLowered)
			}
			if st.SignedClockFloor > f.now.Unix() {
				t.Fatalf("signed floor still ahead of NTP: %d > %d", st.SignedClockFloor, f.now.Unix())
			}
		})
	}
}

// A clock stepped by hand between two of timesyncd's polls is not NTP time,
// even though timesyncd synchronised it earlier this boot.
func TestNTPStepBetweenChecksIsNotPersisted(t *testing.T) {
	for _, step := range []time.Duration{365 * 24 * time.Hour, -30 * 24 * time.Hour} {
		t.Run(step.String(), func(t *testing.T) {
			k, f := newKeeper(t, base.Unix(), base, false)
			f.ntp = true
			checks(k, f, 2)
			want := floorOf(t, k)
			f.tick(Interval)
			f.now = f.now.Add(step)
			k.Check()
			if got := floorOf(t, k); got != want {
				t.Fatalf("floor = %d, want %d: a stepped clock was trusted at once", got, want)
			}
			if len(f.sets) != 0 {
				t.Fatalf("sets = %v", f.sets)
			}
		})
	}
}

// F1: a laptop with a wrong clock uploads on the service port. Before, the
// board jumped ahead, the floor followed, and the keeper pushed the clock
// forward against timesyncd every ten minutes, for good.
func TestWrongLaptopClockDoesNotPinBoard(t *testing.T) {
	laptopAhead := 60 * 24 * time.Hour
	for _, ntpAtUpload := range []bool{false, true} {
		t.Run(map[bool]string{false: "offline at upload", true: "online at upload"}[ntpAtUpload], func(t *testing.T) {
			k, f := newKeeper(t, base.Unix(), base, false)
			real := base
			f.ntp = ntpAtUpload
			k.Check()

			rep := k.ApplyClientTime(real.Add(laptopAhead).Unix(), importer.Batch{Verified: true})
			if rep.Adjusted == ntpAtUpload {
				t.Fatalf("adjusted = %v with NTP %v: %s", rep.Adjusted, ntpAtUpload, rep.Note)
			}
			if got := floorOf(t, k); got != base.Unix() {
				t.Fatalf("the laptop's time reached the floor: %d", got)
			}

			// The board keeps running offline for a while...
			for i := 0; i < 3; i++ {
				f.tick(Interval)
				real = real.Add(Interval)
				k.Check()
			}
			// ...then the network comes back and timesyncd corrects it.
			f.ntp, f.now = true, real
			sets := len(f.sets)
			for i := 0; i < 24*6; i++ { // a day
				f.tick(Interval)
				real = real.Add(Interval)
				k.Check()
			}
			if len(f.sets) != sets {
				t.Fatalf("the keeper moved an NTP-synchronised clock: %v", f.sets[sets:])
			}
			if !f.now.Equal(real) {
				t.Fatalf("clock = %s, want %s", f.now.UTC(), real.UTC())
			}
			if got := floorOf(t, k); got != real.Unix() {
				t.Fatalf("floor = %s, want NTP time %s", time.Unix(got, 0).UTC(), real.UTC())
			}
		})
	}
}

// A state.json left by an earlier agent with the floor (and possibly the
// signed floor) pushed into the future heals by itself once NTP has
// synchronised, and stays healed across a reboot without network.
func TestPoisonedStateRecoversWithNTP(t *testing.T) {
	cases := []struct {
		name          string
		floor, signed time.Duration // ahead of the real time
	}{
		{"floor ahead (uploader, manual date, RTC)", 300 * 24 * time.Hour, -24 * time.Hour},
		{"floor at the 400 day cap", (400 + 30) * 24 * time.Hour, 30 * 24 * time.Hour},
		{"signed createdAt from a backend clock in 2030", 4 * 365 * 24 * time.Hour, 4 * 365 * 24 * time.Hour},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			k, f := newKeeper(t, 0, base, false)
			setState(t, k, base.Add(tc.floor).Unix(), base.Add(tc.signed).Unix())
			real := base

			// Boot after the upgrade, before timesyncd has synchronised: the
			// floor is all the keeper knows, so it is enforced.
			k.Check()
			if len(f.sets) != 1 {
				t.Fatalf("sets = %v", f.sets)
			}
			// timesyncd synchronises a minute later and steps the clock back.
			f.tick(time.Minute)
			real = real.Add(time.Minute)
			f.ntp, f.now = true, real
			for i := 0; i < 3; i++ {
				f.tick(Interval)
				real = real.Add(Interval)
				k.Check()
			}
			if len(f.sets) != 1 {
				t.Fatalf("the keeper fought NTP: %v", f.sets)
			}
			st := stateOf(t, k)
			if st.ClockFloor != real.Unix() || st.SignedClockFloor > real.Unix() {
				t.Fatalf("not healed: floor %s, signed %s, NTP %s", time.Unix(st.ClockFloor, 0).UTC(),
					time.Unix(st.SignedClockFloor, 0).UTC(), real.UTC())
			}

			// Power cut; the board comes back without network or RTC.
			k2, f2 := keeperOn(t, k.layout, real.Add(-2*time.Hour), false)
			k2.Check()
			if len(f2.sets) != 1 || f2.sets[0].Unix() != real.Unix() {
				t.Fatalf("after reboot: sets = %v, want the healed floor %s", f2.sets, real.UTC())
			}
		})
	}
}

// Without NTP, an uploader's time is not written to the floor, so a later
// uploader with a right clock can undo a wrong one. Nobody can move the clock
// below the floor.
func TestUploaderTimeIsCorrectableButNotRollback(t *testing.T) {
	v := importer.Batch{Verified: true}
	k, f := newKeeper(t, base.Unix(), base, true)
	checks(k, f, 7) // an hour of trusted time
	real := f.now

	if rep := k.ApplyClientTime(real.Add(80*24*time.Hour).Unix(), v); !rep.Adjusted {
		t.Fatalf("wrong laptop: %+v", rep)
	}
	if rep := k.ApplyClientTime(real.Unix(), v); !rep.Adjusted || f.now.Unix() != real.Unix() {
		t.Fatalf("a right laptop could not undo the wrong one: %+v, clock %s", rep, f.now.UTC())
	}
	if rep := k.ApplyClientTime(real.Add(-30*time.Minute).Unix(), v); rep.Adjusted {
		t.Fatalf("clock rolled back below the floor: %+v", rep)
	}
	if got, want := floorOf(t, k), base.Add(time.Hour).Unix(); got != want {
		t.Fatalf("floor = %d, want %d", got, want)
	}
}
