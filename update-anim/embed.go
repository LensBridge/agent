// Package updateanim embeds the board's "Working on updates" screen
// (index.html beside this file) so the agent can serve it with no other files
// on disk. The page is driven over CDP through window.mbUpdate; see the
// comment at the bottom of index.html and internal/core/kiosk/screen.go.
//
// It is also the look of every other screen the board shows on its own (the
// setup screen, "Waiting for the board app", the kiosk's starting page):
// Style is index.html's stylesheet, and Page wraps a body in it, so there is
// one copy of the font, the colour-cycling ground and the type.
package updateanim

import (
	_ "embed"
	"strings"
)

// HTML is update-anim/index.html.
//
//go:embed index.html
var HTML []byte

// Style is the contents of index.html's <style> element: the embedded
// Montserrat, the cycling ground on html, .wash, .veil, .screen, .headline,
// .caption, .lines, .footer-line, .warn, the .throb/.sweep throbber, .smiley
// and .alert.
var Style = style(string(HTML))

func style(h string) string {
	i := strings.Index(h, "<style>")
	j := strings.Index(h, "</style>")
	if i < 0 || j < i {
		panic("update-anim/index.html has no <style> element")
	}
	return h[i+len("<style>") : j]
}

// Page is a whole board screen in the update screen's style: title, Style
// plus extraCSS, and body inside <body>, after the fade-in veil and the wash.
// body should lay itself out in a <main class="screen">.
func Page(title, extraCSS, body string) string {
	var b strings.Builder
	b.WriteString(`<!DOCTYPE html><html lang="en"><head><meta charset="utf-8">` +
		`<meta name="viewport" content="width=device-width, initial-scale=1, viewport-fit=cover"><title>`)
	b.WriteString(title)
	b.WriteString("</title><style>")
	b.WriteString(Style)
	b.WriteString(SharedCSS)
	b.WriteString(extraCSS)
	b.WriteString(`</style></head><body><div class="veil" aria-hidden="true"></div>` +
		`<div class="wash" aria-hidden="true"></div>`)
	b.WriteString(body)
	b.WriteString("</body></html>")
	return b.String()
}

// SharedCSS is what the other screens add to Style: a .focal box the size of
// the throbber, holding whichever of the throbber, smiley, alert or USB stick
// the screen's data-icon names, so the type below never moves when it
// changes; and .where, the quiet address line at the foot of the screen.
const SharedCSS = `
.focal { width: clamp(110px, 16vmin, 230px); height: clamp(110px, 16vmin, 230px);
         display: grid; place-items: center; }
.focal > * { grid-area: 1 / 1; display: none; }
.screen[data-icon="working"] .throb,
.screen[data-icon="problem"] .alert,
.screen[data-icon="usb"] .usb { display: block; }
.screen[data-icon="ok"] .smiley { display: flex; }
.usb { width: 100%; height: 100%; overflow: visible;
       filter: drop-shadow(0 0 10px rgba(248, 225, 93, 0.45)) drop-shadow(0 1px 2px rgba(0, 0, 0, 0.45));
       animation: nudge 2.8s cubic-bezier(0.45, 0.05, 0.35, 1) infinite; }
.usb .body, .usb .plug { fill: none; stroke: var(--gold); stroke-width: 4; stroke-linejoin: round; }
.usb .pin, .usb .led { fill: var(--gold); }
@keyframes nudge { 0%, 100% { transform: translateY(0); } 50% { transform: translateY(-4%); } }
.where { position: fixed; left: 0; right: 0; bottom: 4vmin; margin: 0; text-align: center;
         font-size: clamp(11px, 1.6vmin, 24px); font-weight: 500; letter-spacing: 0.12em;
         font-variant-numeric: tabular-nums; color: rgba(255, 255, 255, 0.45); }
.where:empty { display: none; }
@media (prefers-reduced-motion: reduce) { .usb { animation: none; } }
`

// Throbber, Smiley, Alert and USB are the focal marks, for a .focal box.
const (
	Throbber = `<svg class="throb" viewBox="0 0 200 200" aria-hidden="true"><circle class="sweep" cx="100" cy="100" r="76" /></svg>`
	Smiley   = `<p class="smiley" aria-hidden="true">:)</p>`
	Alert    = `<svg class="alert" viewBox="0 0 200 200" aria-hidden="true"><circle class="ring" cx="100" cy="100" r="76" />` +
		`<path class="bar" d="M100 60 V112" /><circle class="dot" cx="100" cy="140" r="6" /></svg>`
	USB = `<svg class="usb" viewBox="0 0 200 200" aria-hidden="true">` +
		`<rect class="plug" x="74" y="22" width="52" height="44" rx="5" />` +
		`<rect class="pin" x="86" y="36" width="10" height="10" rx="1.5" /><rect class="pin" x="104" y="36" width="10" height="10" rx="1.5" />` +
		`<rect class="body" x="56" y="66" width="88" height="112" rx="16" />` +
		`<circle class="led" cx="100" cy="150" r="6" /></svg>`
)
