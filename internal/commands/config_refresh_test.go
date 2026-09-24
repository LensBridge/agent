package commands

import (
	"strings"
	"testing"
)

func TestRefreshScriptCallsOnlyRefresh(t *testing.T) {
	if !strings.Contains(refreshScript, "mb.refresh()") || strings.Contains(refreshScript, "setDeviceId") {
		t.Fatalf("refresh script: %s", refreshScript)
	}
}

func TestFirstNonEmpty(t *testing.T) {
	if got := firstNonEmpty("", "b", "c"); got != "b" {
		t.Fatalf("got %q", got)
	}
	if got := firstNonEmpty("", ""); got != "" {
		t.Fatalf("got %q", got)
	}
}
