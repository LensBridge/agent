package splash

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/LensBridge/agent/internal/cdp"
	"github.com/LensBridge/agent/internal/netinfo"
)

// fakeEval records the expression it was handed and replays a canned outcome.
type fakeEval struct {
	gotExpr string
	applied bool
	err     error
}

func (f *fakeEval) EvaluateValue(_ context.Context, expression string, out any) error {
	f.gotExpr = expression
	if f.err != nil {
		return f.err
	}
	if p, ok := out.(*bool); ok {
		*p = f.applied
	}
	return nil
}

func TestPushNetInfoApplied(t *testing.T) {
	ev := &fakeEval{applied: true}
	info := netinfo.Info{IPv4: []string{"192.168.1.42"}, SSID: "MSA", Hostname: "lobby"}

	if err := PushNetInfo(context.Background(), ev, info); err != nil {
		t.Fatalf("PushNetInfo() = %v, want nil", err)
	}
	if !strings.Contains(ev.gotExpr, "192.168.1.42") {
		t.Errorf("expression does not carry the address:\n%s", ev.gotExpr)
	}
	if !strings.Contains(ev.gotExpr, "setNetInfo") {
		t.Errorf("expression does not call the page hook:\n%s", ev.gotExpr)
	}
}

// A page that answers but has no hook is the board, or Chromium on about:blank.
// The poll loop must be able to tell that apart from a real failure.
func TestPushNetInfoNoHook(t *testing.T) {
	ev := &fakeEval{applied: false}
	if err := PushNetInfo(context.Background(), ev, netinfo.Info{}); !errors.Is(err, ErrNoSplash) {
		t.Fatalf("PushNetInfo() = %v, want ErrNoSplash", err)
	}
}

func TestPushNetInfoUndefinedIsNoSplash(t *testing.T) {
	ev := &fakeEval{err: cdp.ErrEvalUndefined}
	if err := PushNetInfo(context.Background(), ev, netinfo.Info{}); !errors.Is(err, ErrNoSplash) {
		t.Fatalf("PushNetInfo() = %v, want ErrNoSplash", err)
	}
}

// Chromium unreachable must surface as itself, not be swallowed as "no splash".
func TestPushNetInfoTransportErrorPropagates(t *testing.T) {
	want := fmt.Errorf("chrome not reachable")
	ev := &fakeEval{err: want}
	err := PushNetInfo(context.Background(), ev, netinfo.Info{})
	if !errors.Is(err, want) {
		t.Fatalf("PushNetInfo() = %v, want %v", err, want)
	}
	if errors.Is(err, ErrNoSplash) {
		t.Error("transport failure was misreported as ErrNoSplash")
	}
}

// The SSID is chosen by whoever runs the access point. It lands in a string
// literal inside JS evaluated in the kiosk browser, so it has to be encoded,
// never concatenated.
func TestNetInfoScriptEscapesHostileFields(t *testing.T) {
	info := netinfo.Info{
		IPv4:     []string{"192.168.1.42"},
		SSID:     `evil");alert("pwned`,
		Hostname: "</script><img src=x onerror=alert(1)>",
	}

	script, err := netInfoScript(info)
	if err != nil {
		t.Fatalf("netInfoScript() error = %v", err)
	}

	// The payload must be one JSON literal that round-trips to the original.
	start := strings.Index(script, "mb.setNetInfo(")
	if start < 0 {
		t.Fatalf("no setNetInfo call in script:\n%s", script)
	}
	arg := script[start+len("mb.setNetInfo(") : strings.LastIndex(script, ");")]

	var got netinfo.Info
	if err := json.Unmarshal([]byte(arg), &got); err != nil {
		t.Fatalf("argument is not valid JSON (%v): %s", err, arg)
	}
	if !got.Equal(info) {
		t.Errorf("round trip lost data: got %+v, want %+v", got, info)
	}
	// The quote that would have closed the JS string must be backslash-escaped,
	// and the tag that would have closed the <script> block must be \u-encoded.
	if !strings.Contains(arg, `\");alert(\"`) {
		t.Errorf("quotes in the SSID were not escaped: %s", arg)
	}
	if strings.Contains(arg, "</script>") {
		t.Errorf("payload contains a raw closing script tag: %s", arg)
	}
}

func TestNetInfoScriptOfflineDeviceStillPushes(t *testing.T) {
	script, err := netInfoScript(netinfo.Info{Hostname: "lobby"})
	if err != nil {
		t.Fatalf("netInfoScript() error = %v", err)
	}
	// An offline board must still reach the page: the splash renders "Not
	// connected", which is what tells an operator the agent is alive at all.
	if !strings.Contains(script, `"ipv4":null`) {
		t.Errorf("expected an explicit empty address list:\n%s", script)
	}
}
