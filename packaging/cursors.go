package packaging

import (
	"bytes"
	"encoding/binary"
	"path"
)

// A board shows no mouse cursor. A Pi's HDMI CEC remote-control inputs
// report relative motion, so libinput takes them for a mouse and cage draws a
// cursor over the board, mouse or not; telling libinput otherwise is not
// reliable (udev labels and quirks leave them pointers), and ignoring them
// outright leaves cage with no input device, which it will not start with.
//
// So every cursor is made invisible instead: a cursor theme in which each
// cursor is one fully transparent pixel. cage asks wlroots for the theme
// named "default" on XCURSOR_PATH, and Chromium reads XCURSOR_THEME, so the
// theme lives at CursorDir/default and start-kiosk.sh points both there.
// A real mouse is invisible too, as a kiosk wants.

// CursorDir is the XCURSOR_PATH directory holding the "default" theme.
const CursorDir = "/usr/share/musallahboard/cursors"

const (
	cursorFile = "cursors/transparent" // the one image every cursor name gets
	cursorTOC  = "cursors/index.theme"
)

// CursorNames are the cursor names compositors and browsers ask a theme for
// (CSS cursor names and their X11 aliases).
var CursorNames = []string{
	"default", "left_ptr", "arrow", "top_left_arrow", "pointer", "hand1", "hand2",
	"text", "xterm", "ibeam", "vertical-text", "crosshair", "cross",
	"wait", "watch", "progress", "left_ptr_watch", "half-busy",
	"help", "question_arrow", "whats_this",
	"move", "fleur", "all-scroll", "grab", "grabbing", "openhand", "closedhand",
	"dnd-move", "dnd-copy", "dnd-link", "dnd-none",
	"not-allowed", "no-drop", "forbidden", "crossed_circle",
	"copy", "alias", "cell", "context-menu", "zoom-in", "zoom-out",
	"col-resize", "row-resize", "ns-resize", "ew-resize", "nesw-resize", "nwse-resize",
	"sb_h_double_arrow", "sb_v_double_arrow",
	"n-resize", "s-resize", "e-resize", "w-resize", "ne-resize", "nw-resize", "se-resize", "sw-resize",
	"top_side", "bottom_side", "left_side", "right_side",
	"top_left_corner", "top_right_corner", "bottom_left_corner", "bottom_right_corner",
	"X_cursor", "pirate",
}

// cursorFiles are the theme's files, for System.
func cursorFiles() []File {
	dir := path.Join(CursorDir, "default")
	files := []File{{cursorTOC, path.Join(dir, "index.theme"), 0o644}}
	for _, n := range CursorNames {
		files = append(files, File{cursorFile, path.Join(dir, "cursors", n), 0o644})
	}
	return files
}

func init() { System = append(System, cursorFiles()...) }

// generated returns the files packaging makes rather than embeds.
func generated(name string) ([]byte, bool) {
	switch name {
	case cursorFile:
		return TransparentCursor(), true
	case cursorTOC:
		return []byte("[Icon Theme]\nName=MusallahBoard (no cursor)\nComment=Every cursor is one transparent pixel: a board shows none\n"), true
	}
	return nil, false
}

// Xcursor file format (libXcursor): a header, a table of contents, chunks.
const (
	xcursorMagic      = "Xcur"
	xcursorFileHeader = 16
	xcursorVersion    = 0x10000
	xcursorImageType  = 0xfffd0002
	xcursorImageHdr   = 36
)

// TransparentCursor is an Xcursor file holding one 1x1 image whose only
// pixel is fully transparent (ARGB 0).
func TransparentCursor() []byte {
	var b bytes.Buffer
	w := func(v ...uint32) {
		for _, x := range v {
			_ = binary.Write(&b, binary.LittleEndian, x)
		}
	}
	b.WriteString(xcursorMagic)
	w(xcursorFileHeader, xcursorVersion, 1)       // header size, version, one TOC entry
	w(xcursorImageType, 1, xcursorFileHeader+3*4) // TOC: an image of nominal size 1, at offset 28
	w(xcursorImageHdr, xcursorImageType, 1, 1)    // chunk: header size, type, nominal size, version
	w(1, 1, 0, 0, 0)                              // width, height, hotspot x, y, delay
	w(0)                                          // the pixel
	return b.Bytes()
}
