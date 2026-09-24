// Package updateanim embeds the board's "Working on updates" screen
// (index.html beside this file) so the agent can serve it with no other files
// on disk. The page is driven over CDP through window.mbUpdate; see the
// comment at the bottom of index.html and internal/updatescreen.
package updateanim

import _ "embed"

// HTML is update-anim/index.html.
//
//go:embed index.html
var HTML []byte
