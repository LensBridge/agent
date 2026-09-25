// Package notice is what the board tells the people in front of it about an
// update: a headline and a few short lines, in plain language, with a tone
// (docs/architecture.md, section 8). The same notice is shown on the update
// screen when something was installed, or as a banner over the running board
// when nothing was (already up to date, a USB stick with nothing on it, a
// package that could not be verified).
package notice

import "fmt"

// Tone is how a notice looks.
type Tone string

const (
	// Progress is something under way ("Reading USB stick…"). It stays up
	// until replaced.
	Progress Tone = "progress"
	OK       Tone = "ok"
	// Neutral is "nothing needed doing": already up to date, nothing for
	// this board.
	Neutral Tone = "neutral"
	Problem Tone = "problem"
)

// MaxLines is how many lines a notice shows; more are folded into
// "and N more".
const MaxLines = 3

// Notice is one message for the screen.
type Notice struct {
	Tone     Tone     `json:"tone"`
	Headline string   `json:"headline"`
	Lines    []string `json:"lines"`
	// Seconds is how long a banner stays up; 0 means until replaced.
	Seconds int `json:"seconds"`
}

// Seconds a banner of each tone stays up: long enough to read from across a
// room, and longer for a problem, which someone may need to act on.
func DefaultSeconds(t Tone) int {
	switch t {
	case Progress:
		return 0
	case Problem:
		return 14
	}
	return 8
}

// New builds a notice with the default duration for its tone and the lines
// folded to MaxLines.
func New(t Tone, headline string, lines ...string) Notice {
	return Notice{Tone: t, Headline: headline, Lines: Fold(lines), Seconds: DefaultSeconds(t)}
}

// Fold drops empty and repeated lines and keeps at most MaxLines, replacing
// the rest with "and N more".
func Fold(lines []string) []string {
	out := []string{}
	seen := map[string]bool{}
	for _, l := range lines {
		if l == "" || seen[l] {
			continue
		}
		seen[l] = true
		out = append(out, l)
	}
	if len(out) > MaxLines {
		more := len(out) - (MaxLines - 1)
		out = append(out[:MaxLines-1], fmt.Sprintf("and %d more", more))
	}
	return out
}
