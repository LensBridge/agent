package commands

import (
	"strings"
	"testing"
)

// The device id is interpolated into an expression evaluated in the page. It
// comes from our own config, but a quote in it must never be able to close the
// JS literal and change what runs in the browser.
func TestRefreshScriptEscapesDeviceID(t *testing.T) {
	// json.Marshal emits a double-quoted literal, so single quotes and the
	// `);` that would otherwise close the call stay inert data.
	script := refreshScript(`'); alert('pwned`)
	if !strings.Contains(script, `const id = "'); alert('pwned";`) {
		t.Errorf("single quotes not contained in a JSON literal; got:\n%s", script)
	}

	// A double quote must be backslash-escaped rather than terminating it.
	script = refreshScript(`a" + b + "c`)
	if !strings.Contains(script, `const id = "a\" + b + \"c";`) {
		t.Errorf("double quotes not escaped; got:\n%s", script)
	}
}

// An unenrolled agent shouldn't push an empty identity into the page. The
// script guards on `if (id)`, so an empty id must still produce valid JS that
// refreshes without calling setDeviceId.
func TestRefreshScriptEmptyDeviceID(t *testing.T) {
	script := refreshScript("")

	if !strings.Contains(script, `const id = "";`) {
		t.Errorf("expected an empty JS string literal; got:\n%s", script)
	}
	if !strings.Contains(script, "if (id) {") {
		t.Error("script must guard setDeviceId behind a non-empty id")
	}
}

func TestRefreshScriptCallsBothHooks(t *testing.T) {
	script := refreshScript("3f2504e0-4f89-11d3-9a0c-0305e82c3301")

	for _, want := range []string{
		"window.MusallahBoard",
		"mb.setDeviceId(id, { reload: false })",
		"await mb.refresh()",
		"hook: false", // the "page has no hook" signal
	} {
		if !strings.Contains(script, want) {
			t.Errorf("script missing %q; got:\n%s", want, script)
		}
	}
}

func TestFirstNonEmpty(t *testing.T) {
	if got := firstNonEmpty("", "", "third"); got != "third" {
		t.Errorf("got %q, want %q", got, "third")
	}
	if got := firstNonEmpty("first", "second"); got != "first" {
		t.Errorf("got %q, want %q", got, "first")
	}
	if got := firstNonEmpty("", ""); got != "" {
		t.Errorf("got %q, want empty", got)
	}
}
