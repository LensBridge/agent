package importer

import (
	"errors"

	"github.com/LensBridge/agent/internal/core/config"
	"github.com/LensBridge/agent/internal/core/mbu"
)

// provision applies a verified setup file (docs/architecture.md, section 8):
//
//   - a board with no identity takes the file's device id and name;
//   - a board with an identity takes only a file for its own device id with a
//     strictly newer sequence, which updates its name and, when the file has
//     one, its backend and claim secret;
//   - a file for another device is skipped (another board's, on a USB kit).
//     Giving a board another identity means reflashing it.
//
// A file without a backend never removes one: it says nothing about the
// online layer. Everything is written to agent.toml in one atomic save.
//
// The daemon applies it itself, without a root step: agent.toml is the
// daemon's own file (it is the service user's, in the service user's
// directory), so a root-owned copy of this logic would guard nothing the
// daemon could not already change. What a setup file can do is bounded by
// what is in it: an identity and a backend address, never a key, software or
// anything that runs.
func (im *Importer) provision(pkg *mbu.Package, res Result, show func(mbu.Type, string)) Result {
	m := pkg.Manifest
	raw, err := pkg.ReadFile(mbu.ProvisionFile, mbu.MaxProvisionBytes)
	if err != nil {
		return failed(res, "A file is damaged or is not a MusallahBoard update", err)
	}
	p, err := mbu.ParseProvision(raw, m)
	if err != nil {
		return failed(res, "This setup file cannot be used", err)
	}
	if r, done := provisionDecision(im.d.Config.Get(), m, res); done {
		return r
	}

	show(m.Type, "Setting up this board")
	first := false
	err = im.d.Config.Update(func(c *config.Config) error {
		// Decided again under the config's lock: two setup files racing
		// (USB and an upload) cannot both win.
		if _, done := provisionDecision(c, m, res); done {
			return errSuperseded
		}
		first = !c.Provisioned()
		c.DeviceID, c.DisplayName, c.ProvisionSequence = m.DeviceID, p.DisplayName, m.Sequence
		if p.BackendURL != "" {
			if c.BackendURL != p.BackendURL {
				// A new backend: the board claims its key there.
				c.WebSocketURL = ""
			}
			c.BackendURL = p.BackendURL
			c.ClaimSecret = ""
			if c.WebSocketURL == "" {
				c.ClaimSecret = p.ClaimSecret
			}
		}
		return nil
	})
	if errors.Is(err, errSuperseded) {
		r, _ := provisionDecision(im.d.Config.Get(), m, res)
		return r
	}
	if err != nil {
		return failed(res, "Saving this board's settings failed", err)
	}
	im.d.Logger.Info("setup file applied", "deviceId", m.DeviceID, "displayName", p.DisplayName,
		"sequence", m.Sequence, "backendUrl", p.BackendURL, "first", first)
	res.Action = ActionInstalled
	switch {
	case first && p.DisplayName != "":
		res.Message = "Set up as " + p.DisplayName
	case first:
		res.Message = "This board is set up"
	default:
		res.Message = "Board settings updated"
	}
	return res
}

var errSuperseded = errors.New("superseded")

// provisionDecision is whether a setup file with manifest m may be applied
// to a board with config c; if not, the result to report and true.
func provisionDecision(c *config.Config, m *mbu.Manifest, res Result) (Result, bool) {
	switch {
	case !c.Provisioned():
		return res, false
	case c.DeviceID != m.DeviceID:
		return skippedForeign(res.File, m.Type), true
	case m.Sequence < c.ProvisionSequence:
		res.Action, res.Message = ActionOutdated, "The board already has newer settings"
		return res, true
	case m.Sequence == c.ProvisionSequence:
		res.Action, res.Message = ActionUnchanged, "This setup file is already applied"
		return res, true
	}
	return res, false
}
