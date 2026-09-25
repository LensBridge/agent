// Package clock keeps the board's wall clock from going wrong
// (docs/architecture.md, section 10).
//
// Which day's content is shown, and every prayer time on it, depends on the
// clock. A Pi has no battery-backed clock unless someone fitted one, so after
// a power cut on a board without internet it can come up years in the past.
// The defence is a floor that only moves forward: the newest signed createdAt
// of any verified package, and the wall clock itself, persisted every ten
// minutes. The clock is never allowed to sit more than a minute behind it.
//
// The only other way the clock moves is an uploader's time, and only when a
// package in that same upload verified and the time is within 90 days of the
// floor, so an unauthenticated client cannot set the board to any date it
// likes.
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
	"time"

	"github.com/LensBridge/agent/internal/importer"
	"github.com/LensBridge/agent/internal/state"
	"github.com/LensBridge/agent/internal/store"
)

const (
	// Interval is how often the wall clock is persisted and checked.
	Interval = 10 * time.Minute
	// BehindTolerance is how far behind the floor the clock may be before it
	// is corrected. Persisting happens every ten minutes, so a floor a few
	// seconds ahead of a correct clock is normal and not worth a jump.
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
}

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
		NTPSynced: timesyncdSynced,
		Uptime:    uptime,
		BootID:    bootID,
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

// timesyncdSynced reports whether systemd-timesyncd has synchronised the
// clock since boot; it creates this file when it does (systemd 239 and
// later).
func timesyncdSynced() bool {
	_, err := os.Stat("/run/systemd/timesync/synchronized")
	return err == nil
}

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

// Check corrects a clock that is behind the floor, then persists the wall
// clock into the floor. The order matters: persisting first would be a no-op
// for a clock behind the floor anyway, but checking first means the floor is
// then raised to the corrected time.
func (k *Keeper) Check() {
	st, err := k.layout.State().Load()
	if err != nil {
		k.logger.Warn("clock: cannot read the clock floor", "err", err)
		return
	}
	now := k.Now()
	floor := time.Unix(st.ClockFloor, 0)
	if st.ClockFloor > 0 && now.Before(floor.Add(-BehindTolerance)) {
		k.logger.Warn("clock is behind the newest time this board has proof of; moving it forward",
			"now", now.UTC().Format(time.RFC3339), "floor", floor.UTC().Format(time.RFC3339),
			"behind", floor.Sub(now).Round(time.Second).String())
		if err := k.set(floor); err != nil {
			k.logger.Error("clock: could not set the system clock", "err", err)
			return
		}
		now = floor
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
// seconds) after the upload's batch b ran. It applies it only when a package
// in b verified, the difference is more than ClientTolerance, and the time is
// within [floor, floor + ClientWindow].
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
	if err := k.layout.State().RaiseClockFloor(clientUnix); err != nil {
		k.logger.Warn("clock: could not persist the clock floor", "err", err)
	}
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
