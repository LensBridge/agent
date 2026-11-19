// Package clock keeps the board's wall clock from going wrong
// (docs/architecture.md, section 10).
//
// Which day's content is shown, and every prayer time on it, depends on the
// clock. A Pi has no battery-backed clock unless someone fitted one, so after
// a power cut on a board without internet it can come up years in the past.
//
// When systemd-timesyncd has synchronised the clock, NTP is the authority:
// the keeper never moves the clock, ignores uploaders' clocks, and lowers a
// remembered floor that is ahead of NTP time (it was raised by something
// wrong). The floor follows NTP time, once two checks an interval apart agree
// with the monotonic clock.
//
// Without NTP the defence is a floor that only moves forward: the newest
// signed createdAt of any verified package, and the last trusted time carried
// forward by the monotonic clock, persisted every ten minutes. The clock is
// never allowed to sit more than a minute behind it. The wall clock itself is
// never written into the floor without NTP: nothing has confirmed it, and a
// wrong one (an uploader's, a manual date, a bad RTC) would otherwise lock the
// board in the future.
//
// The only other way the clock moves is an uploader's time, and only without
// NTP, when a package in that same upload verified and the time is within 90
// days past the floor. It sets the clock but not the floor, so a wrong
// uploader's time is undone by the next good one, or by NTP.
//
// The floor keeps the clock from going backwards; it cannot say the clock is
// right. Info does: a board without NTP, an RTC, or a laptop's time since it
// last booted is showing whatever time it came up with, and says so.
package clock

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/LensBridge/agent/internal/clock/timesync"
	"github.com/LensBridge/agent/internal/importer"
	"github.com/LensBridge/agent/internal/state"
	"github.com/LensBridge/agent/internal/store"
)

const (
	// Interval is how often the wall clock is persisted and checked.
	Interval = 10 * time.Minute
	// BehindTolerance is how far behind the floor the clock may be before it
	// is corrected. Persisting happens every ten minutes, so a floor a few
	// seconds ahead of a correct clock is normal and not worth a jump. It is
	// also how far a floor may be ahead of NTP time before it is lowered, and
	// how far the wall clock may disagree with the monotonic clock between
	// two checks before NTP time is believed again.
	BehindTolerance = 60 * time.Second
	// ClientTolerance is the drift an uploader's time must exceed to be
	// applied; below it, laptop clocks disagree with each other anyway.
	ClientTolerance = 5 * time.Second
	// ClientWindow is how far past the floor an uploader's time may be.
	ClientWindow = 90 * 24 * time.Hour

	// DefaultRTCDevice is the kernel's first hardware clock.
	DefaultRTCDevice = "/dev/rtc0"

	// BootGrace is how long after boot an unverified clock is given before
	// the board says it may be wrong: an online board's NTP sync takes a
	// moment, and a warning that flashes up at every boot is noise.
	BootGrace = 3 * time.Minute
)

// Where the clock's time comes from.
const (
	SourceNTP      = "ntp"      // systemd-timesyncd has synchronised it
	SourceRTC      = "rtc"      // a battery-backed hardware clock
	SourceUploader = "uploader" // a laptop or phone's time, since boot
	// SourceStarting: just booted, not verified yet, not yet worth a
	// warning (BootGrace).
	SourceStarting = "starting"
	// SourceUnverified: nothing has confirmed the time since boot. After a
	// power cut on an offline board without an RTC it can be days out.
	SourceUnverified = "unverified"
)

// Info says whether the board's clock can be believed.
type Info struct {
	Source string `json:"source"`
	// Trusted is false only when there is reason to warn: the board
	// should say its clock may be wrong.
	Trusted bool `json:"trusted"`
}

// ClockReport is what the upload server returns about the uploader's clock.
// DriftSeconds is the board's clock minus the uploader's, measured before any
// correction: positive means the board was ahead.
type ClockReport struct {
	DriftSeconds int64  `json:"driftSeconds"`
	Adjusted     bool   `json:"adjusted"`
	Note         string `json:"note"`
}

// Keeper owns the board's clock.
type Keeper struct {
	layout store.Layout
	logger *slog.Logger

	// Now, SetClock and WriteRTC are replaceable for tests.
	Now      func() time.Time
	SetClock func(time.Time) error
	WriteRTC func() error
	// RTCDevice is checked for existence by RTCPresent.
	RTCDevice string
	// NTPSynced, Uptime and BootID are replaceable for tests.
	NTPSynced func() bool
	Uptime    func() time.Duration
	BootID    func() string
	// Monotonic is a clock that only counts real elapsed time: setting the
	// wall clock does not move it. Replaceable for tests.
	Monotonic func() time.Duration

	mu sync.Mutex
	// anchor is the last time the keeper trusted, and when by Monotonic.
	// Without NTP the floor advances from it by elapsed monotonic time, not
	// by whatever the wall clock says.
	anchor *anchor
}

// anchor is a trusted wall-clock time and the monotonic reading taken with it.
type anchor struct {
	wall time.Time
	mono time.Duration
	// ntp: wall was NTP-synchronised time, not the floor.
	ntp bool
}

// at is the anchor carried forward to the monotonic reading mono.
func (a *anchor) at(mono time.Duration) time.Time { return a.wall.Add(mono - a.mono) }

// New returns a Keeper for the board's state in layout.
func New(layout store.Layout, logger *slog.Logger) *Keeper {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Keeper{
		layout:    layout,
		logger:    logger,
		Now:       time.Now,
		SetClock:  setSystemClock,
		WriteRTC:  writeRTC,
		RTCDevice: DefaultRTCDevice,
		NTPSynced: timesync.Synced,
		Uptime:    uptime,
		BootID:    bootID,
		Monotonic: monotonic,
	}
}

// Info reports where the clock's time comes from and whether to trust it.
func (k *Keeper) Info() Info {
	switch {
	case k.NTPSynced():
		return Info{Source: SourceNTP, Trusted: true}
	case k.RTCPresent():
		return Info{Source: SourceRTC, Trusted: true}
	}
	if st, err := k.layout.State().Load(); err == nil && st.ClockSetBoot != "" && st.ClockSetBoot == k.BootID() {
		return Info{Source: SourceUploader, Trusted: true}
	}
	if k.Uptime() < BootGrace {
		return Info{Source: SourceStarting, Trusted: true}
	}
	return Info{Source: SourceUnverified, Trusted: false}
}

// verifiedThisBoot records that an uploader's time confirmed the clock since
// this boot.
func (k *Keeper) verifiedThisBoot() {
	id := k.BootID()
	if id == "" {
		return
	}
	if _, err := k.layout.State().Update(func(s *state.State) error { s.ClockSetBoot = id; return nil }); err != nil {
		k.logger.Warn("clock: could not record the verified time", "err", err)
	}
}

// processStart carries Go's monotonic reading; time.Since(processStart) is
// unaffected by changes to the wall clock.
var processStart = time.Now()

func monotonic() time.Duration { return time.Since(processStart) }

func uptime() time.Duration {
	raw, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return 0
	}
	f := strings.Fields(string(raw))
	if len(f) == 0 {
		return 0
	}
	secs, err := strconv.ParseFloat(f[0], 64)
	if err != nil {
		return 0
	}
	return time.Duration(secs * float64(time.Second))
}

func bootID() string {
	raw, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(raw))
}

// RTCPresent reports whether the board has a hardware clock.
func (k *Keeper) RTCPresent() bool {
	_, err := os.Stat(k.RTCDevice)
	return err == nil
}

// Run checks at once and then every Interval until ctx ends.
func (k *Keeper) Run(ctx context.Context) {
	t := time.NewTicker(Interval)
	defer t.Stop()
	for {
		k.Check()
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Check runs at start and every Interval.
//
// With NTP it never moves the clock; it lowers a floor that is ahead of NTP
// time and persists NTP time into the floor (checkNTP).
//
// Without NTP it moves a clock that is more than BehindTolerance behind the
// floor up to the floor, then persists the last trusted time carried forward
// by the monotonic clock: never the wall clock, which nothing has confirmed.
func (k *Keeper) Check() {
	k.mu.Lock()
	defer k.mu.Unlock()
	st, err := k.layout.State().Load()
	if err != nil {
		k.logger.Warn("clock: cannot read the clock floor", "err", err)
		return
	}
	now, mono := k.Now(), k.Monotonic()
	if k.NTPSynced() {
		k.checkNTP(st, now, mono)
		return
	}
	floor := time.Unix(st.ClockFloor, 0)
	if st.ClockFloor > 0 && now.Before(floor.Add(-BehindTolerance)) {
		k.logger.Warn("clock is behind the newest time this board has proof of, and NTP has not synchronised it; moving it forward",
			"now", now.UTC().Format(time.RFC3339), "floor", floor.UTC().Format(time.RFC3339),
			"behind", floor.Sub(now).Round(time.Second).String())
		if err := k.set(floor); err != nil {
			k.logger.Error("clock: could not set the system clock", "err", err)
			return
		}
	}
	if st.ClockFloor <= 0 {
		// Nothing trusted to carry forward yet: no signed package, no NTP.
		return
	}
	if k.anchor == nil || k.anchor.at(mono).Unix() < st.ClockFloor {
		// First check, or a signed createdAt raised the floor since.
		k.anchor = &anchor{wall: floor, mono: mono}
	}
	if err := k.layout.State().RaiseClockFloor(k.anchor.at(mono).Unix()); err != nil {
		k.logger.Warn("clock: could not persist the clock floor", "err", err)
	}
}

// checkNTP is Check for an NTP-synchronised clock. NTP time is believed once
// two checks agree: since the previous check, the wall clock moved as far as
// the monotonic clock did. That keeps a clock stepped by hand between
// timesyncd's polls out of the floor. The first check after start, or after
// a step, only takes a new anchor.
func (k *Keeper) checkNTP(st state.State, now time.Time, mono time.Duration) {
	prev := k.anchor
	k.anchor = &anchor{wall: now, mono: mono, ntp: true}
	if prev == nil || !prev.ntp {
		return
	}
	if d := now.Sub(prev.at(mono)); d > BehindTolerance || d < -BehindTolerance {
		k.logger.Info("clock: the clock was stepped since the last check; waiting for it to settle before trusting it",
			"step", d.Round(time.Second).String())
		return
	}
	limit := now.Add(BehindTolerance).Unix()
	if st.ClockFloor > limit || st.SignedClockFloor > limit {
		before, lowered, err := k.layout.State().CapClockFloor(now.Unix())
		switch {
		case err != nil:
			k.logger.Warn("clock: could not lower the clock floor", "err", err)
		case lowered:
			k.logger.Warn("clock: the clock floor was ahead of NTP-synchronised time, so it was wrong; lowered it to NTP time",
				"ntp", now.UTC().Format(time.RFC3339),
				"floor", time.Unix(before.ClockFloor, 0).UTC().Format(time.RFC3339),
				"signedFloor", time.Unix(before.SignedClockFloor, 0).UTC().Format(time.RFC3339),
				"floorAhead", time.Unix(before.ClockFloor, 0).Sub(now).Round(time.Second).String())
		}
	}
	if err := k.layout.State().RaiseClockFloor(now.Unix()); err != nil {
		k.logger.Warn("clock: could not persist the clock floor", "err", err)
	}
}

// set sets the system clock and, when there is one, the hardware clock, so
// the correction survives the next power cut.
func (k *Keeper) set(t time.Time) error {
	if err := k.SetClock(t); err != nil {
		return err
	}
	if k.RTCPresent() {
		if err := k.WriteRTC(); err != nil {
			k.logger.Warn("clock: set the system clock but could not write the hardware clock", "err", err)
		}
	}
	return nil
}

// ApplyClientTime considers an uploader's clock (X-MB-Client-Time, Unix
// seconds) after the upload's batch b ran. It applies it only when the clock
// is not NTP-synchronised, a package in b verified, the difference is more
// than ClientTolerance, and the time is within [floor, floor + ClientWindow].
//
// It sets the clock (and the RTC) but never the floor: the uploader is not
// authenticated, and its clock may be wrong. A wrong time it set is undone by
// the next uploader with a better clock, or by NTP.
func (k *Keeper) ApplyClientTime(clientUnix int64, b importer.Batch) ClockReport {
	now := k.Now()
	drift := now.Unix() - clientUnix
	rep := ClockReport{DriftSeconds: drift}
	if abs(drift) <= int64(ClientTolerance/time.Second) {
		rep.Note = "the board's clock agrees with this device"
		if b.Verified {
			k.verifiedThisBoot()
		}
		return rep
	}
	if k.NTPSynced() {
		rep.Note = fmt.Sprintf("the board's clock is %s, but it is set from the internet, so it was not changed; this device's clock may be wrong", describeDrift(drift))
		return rep
	}
	if !b.Verified {
		rep.Note = fmt.Sprintf("the board's clock is %s; it was not changed because no package in this upload verified", describeDrift(drift))
		return rep
	}
	st, err := k.layout.State().Load()
	if err != nil {
		rep.Note = "the board's clock was not changed: cannot read its clock floor: " + err.Error()
		return rep
	}
	floor := st.ClockFloor
	switch {
	case clientUnix < floor:
		rep.Note = fmt.Sprintf("the board's clock is %s, but this device's time is earlier than a signed package the board has seen; this device's clock looks wrong, so the board's was not changed", describeDrift(drift))
		return rep
	case clientUnix > floor+int64(ClientWindow/time.Second):
		rep.Note = fmt.Sprintf("the board's clock is %s, but this device's time is more than 90 days past anything the board can confirm; the board's clock was not changed", describeDrift(drift))
		return rep
	}
	target := time.Unix(clientUnix, 0)
	if err := k.set(target); err != nil {
		rep.Note = fmt.Sprintf("the board's clock is %s; setting it failed: %v", describeDrift(drift), err)
		k.logger.Error("clock: could not apply uploader time", "err", err)
		return rep
	}
	rep.Adjusted = true
	k.verifiedThisBoot()
	rep.Note = fmt.Sprintf("the board's clock was %s and has been set to this device's time", describeDrift(drift))
	k.logger.Info("clock set from uploader", "driftSeconds", drift, "to", target.UTC().Format(time.RFC3339))
	return rep
}

// describeDrift says which way the board is off, against the uploader.
func describeDrift(drift int64) string {
	d := (time.Duration(abs(drift)) * time.Second).String()
	if drift > 0 {
		return d + " ahead"
	}
	return d + " behind"
}

func abs(n int64) int64 {
	if n < 0 {
		return -n
	}
	return n
}

// writeRTC copies the system clock into the hardware clock.
func writeRTC() error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "hwclock", "--systohc", "--utc", "--noadjfile").CombinedOutput()
	if err != nil {
		return fmt.Errorf("hwclock --systohc --utc --noadjfile: %v: %s", err, out)
	}
	return nil
}
