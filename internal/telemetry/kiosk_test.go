package telemetry

import (
	"context"
	"errors"
	"testing"

	"github.com/utmmsa/musallahboard-agent/internal/cdp"
)

type stubProber struct {
	status cdp.PageStatus
	err    error
	calls  int
}

func (s *stubProber) PageStatus(context.Context) (cdp.PageStatus, error) {
	s.calls++
	return s.status, s.err
}

// The board answering is the only positive proof the kiosk is really up, and
// it is also where displayedFrameKey comes from.
func TestCheckKioskBoardAnswers(t *testing.T) {
	p := &stubProber{status: cdp.PageStatus{Paired: true, SlideKey: "poster-3"}}

	alive, frame := checkKiosk(context.Background(), p)

	if !alive {
		t.Error("kiosk should be alive when the page answers")
	}
	if frame != "poster-3" {
		t.Errorf("frame = %q, want %q", frame, "poster-3")
	}
}

// The enrollment splash (and any frontend older than getStatus) has no hook.
// Chromium is still up, so the kiosk is alive — just not showing a frame.
func TestCheckKioskSplashIsStillAlive(t *testing.T) {
	p := &stubProber{err: cdp.ErrEvalUndefined}

	alive, frame := checkKiosk(context.Background(), p)

	if !alive {
		t.Error("a page without the hook still means the browser is running")
	}
	if frame != "" {
		t.Errorf("frame = %q, want empty", frame)
	}
}

// When Chromium is unreachable we must fall through to the systemd check
// rather than reporting a hard false — the probe failing is not proof of death
// (no debugger port, browser still starting).
func TestCheckKioskFallsBackWhenBrowserUnreachable(t *testing.T) {
	p := &stubProber{err: errors.New("connection refused")}

	_, frame := checkKiosk(context.Background(), p)

	if p.calls != 1 {
		t.Errorf("prober calls = %d, want 1", p.calls)
	}
	// Whatever systemd reports on this host, we must not invent a frame id.
	if frame != "" {
		t.Errorf("frame = %q, want empty when the probe failed", frame)
	}
}

// A nil prober is the documented "no browser channel" case and must not panic.
func TestCheckKioskNilProber(t *testing.T) {
	_, frame := checkKiosk(context.Background(), nil)

	if frame != "" {
		t.Errorf("frame = %q, want empty", frame)
	}
}
