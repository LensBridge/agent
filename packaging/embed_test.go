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

// The cursor theme: a valid Xcursor file whose one pixel is transparent,
// installed under every cursor name, in the "default" theme cage loads.
func TestCursorThemeIsTransparent(t *testing.T) {
	b, err := Read(cursorFile)
	if err != nil {
		t.Fatal(err)
	}
	le := func(off int) uint32 {
		return uint32(b[off]) | uint32(b[off+1])<<8 | uint32(b[off+2])<<16 | uint32(b[off+3])<<24
	}
	if string(b[:4]) != "Xcur" || le(4) != 16 || le(12) != 1 {
		t.Fatalf("not an Xcursor file with one entry: % x", b[:16])
	}
	pos := int(le(24))
	if le(16) != 0xfffd0002 || pos != 28 || le(pos) != 36 || le(pos+4) != 0xfffd0002 {
		t.Fatalf("bad table of contents or image chunk: % x", b)
	}
	if w, h := le(pos+16), le(pos+20); w != 1 || h != 1 {
		t.Fatalf("image %dx%d", w, h)
	}
	if px := le(pos + 36); px != 0 || len(b) != pos+40 {
		t.Fatalf("pixel %#x, length %d", px, len(b))
	}

	want := map[string]bool{}
	for _, n := range []string{"default", "left_ptr", "pointer", "text"} {
		want[CursorDir+"/default/cursors/"+n] = true
	}
	want[CursorDir+"/default/index.theme"] = true
	for _, f := range System {
		if want[f.Path] {
			if _, err := Read(f.Name); err != nil {
				t.Errorf("%s: %v", f.Path, err)
			}
			delete(want, f.Path)
		}
	}
	if len(want) > 0 {
		t.Errorf("not installed: %v", want)
	}
}
