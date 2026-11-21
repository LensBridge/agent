package updateanim

import (
	"strings"
	"testing"
)

// Every board screen takes the update screen's look from Style, so it must
// hold the parts they rely on.
func TestStyleIsTheUpdateScreens(t *testing.T) {
	for _, want := range []string{"@font-face", "@property --bg-h", "@keyframes turn", ".wash", ".veil", ".headline", ".sweep", ".alert"} {
		if !strings.Contains(Style, want) {
			t.Errorf("Style has no %q", want)
		}
	}
	if strings.Contains(Style, "<style>") || strings.Contains(Style, "</style>") {
		t.Error("Style includes its own tags")
	}
}

func TestPage(t *testing.T) {
	p := Page("T", ".x{}", `<main class="screen">hi</main>`)
	for _, want := range []string{"<title>T</title>", "@property --bg-h", ".focal", ".x{}", `<div class="wash"`, `<main class="screen">hi</main></body></html>`} {
		if !strings.Contains(p, want) {
			t.Errorf("page has no %q", want)
		}
	}
}
