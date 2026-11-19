package wsclient

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/LensBridge/agent/internal/core/kiosk"
)

type stubProber struct {
	status kiosk.PageStatus
	err    error
	calls  int
}

func (s *stubProber) PageStatus(context.Context) (kiosk.PageStatus, error) {
	s.calls++
	return s.status, s.err
}

// The board answering is the only positive proof the kiosk is really up, and
// it is also where displayedFrameKey comes from.
func TestCheckKioskBoardAnswers(t *testing.T) {
	p := &stubProber{status: kiosk.PageStatus{Paired: true, SlideKey: "poster-3"}}

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
	p := &stubProber{err: kiosk.ErrEvalUndefined}

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

// A crashed or hung renderer leaves the kiosk unit active, so the unit state
// must not be consulted: the board is blank and has to be reported as such.
func TestCheckKioskDeadPageIsNotAlive(t *testing.T) {
	for _, err := range []error{
		fmt.Errorf("%w (http://127.0.0.1:8080/)", kiosk.ErrPageCrashed),
		fmt.Errorf("%w: read: context deadline exceeded", kiosk.ErrPageUnresponsive),
	} {
		p := &stubProber{err: err}
		alive, frame := checkKiosk(context.Background(), p)
		if alive {
			t.Errorf("%v: kiosk reported alive", err)
		}
		if frame != "" {
			t.Errorf("%v: frame = %q, want empty", err, frame)
		}
	}
}

// A page between documents, or one whose hook threw, still answered.
func TestCheckKioskTransientPageStatesAreAlive(t *testing.T) {
	for _, err := range []error{
		fmt.Errorf("%w: Execution context was destroyed.", kiosk.ErrPageNavigating),
		fmt.Errorf("%w: TypeError: x is undefined", kiosk.ErrPageThrew),
	} {
		p := &stubProber{err: err}
		if alive, _ := checkKiosk(context.Background(), p); !alive {
			t.Errorf("%v: kiosk reported dead", err)
		}
	}
}

// No browser to ask is not proof of death: the unit's state decides, and
// the probe errors that mean "no browser" must take that path.
func TestCheckKioskUnreachableUsesUnitState(t *testing.T) {
	want := systemdKioskActive(context.Background())
	for _, err := range []error{
		fmt.Errorf("%w at http://localhost:9222: connection refused", kiosk.ErrDebuggerUnreachable),
		fmt.Errorf("%w (0 total)", kiosk.ErrNoPageTarget),
	} {
		p := &stubProber{err: err}
		if alive, _ := checkKiosk(context.Background(), p); alive != want {
			t.Errorf("%v: alive = %v, want the unit state %v", err, alive, want)
		}
	}
}
