// Package packaging embeds the files the agent installs itself, so a board
// set up by the curl|bash setup.sh (which has no repository checkout) gets the
// same bytes as one installed from this directory.
package packaging

import _ "embed"

// WaitingHTML is waiting.html: the kiosk's "waiting for enrollment" splash,
// installed by `musallahboard-agent splash install`.
//
//go:embed waiting.html
var WaitingHTML []byte

// StartingHTML is starting.html: the kiosk's first page on an enrolled board,
// which waits for the agent to answer before moving to the board.
//
//go:embed starting.html
var StartingHTML []byte
