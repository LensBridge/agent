package packaging

import (
	"strings"
	"testing"
)

// The installed starting page is file:// and needs nothing from anyone: it
// must carry the update screen's style, font included, with the marker gone.
func TestStartingPageCarriesTheScreenStyle(t *testing.T) {
	b, err := Read("starting.html")
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if strings.Contains(s, screenCSS) {
		t.Fatal("marker left in starting.html")
	}
	for _, want := range []string{"@font-face", "@property --bg-h", ".focal", "MusallahBoard is starting", "/_mb/alive.gif"} {
		if !strings.Contains(s, want) {
			t.Errorf("starting.html has no %q", want)
		}
	}
	if strings.Contains(s, "https://") {
		t.Error("starting.html must not load anything from the network")
	}
}
