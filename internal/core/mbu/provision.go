package mbu

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// A provision package ("setup file", docs/architecture.md section 4.1) gives a
// board its identity: which device it is, its display name and, for a board
// that will be online, its backend and a one-time claim secret. It is made by
// the backend and signed with the content key, and carries exactly one file,
// provision.json. It can never carry keys, software or anything executable:
// the content key may only ever affect content and identity.

// ProvisionFile is the one file in a provision package.
const ProvisionFile = "provision.json"

// MaxProvisionBytes bounds provision.json.
const MaxProvisionBytes = 64 << 10

// ProvisionFormat is the provision.json format this agent reads.
const ProvisionFormat = 1

// ClaimSecretBytes is the size of a claim secret.
const ClaimSecretBytes = 32

// Provision is provision.json.
type Provision struct {
	Format      int    `json:"format"`
	DeviceID    string `json:"deviceId"`
	DisplayName string `json:"displayName"`
	Sequence    int64  `json:"sequence"`
	IssuedAt    string `json:"issuedAt,omitempty"`
	// BackendURL is absent for a board that stays offline.
	BackendURL string `json:"backendUrl,omitempty"`
	// ClaimSecret (base64url, 32 bytes) is present exactly when BackendURL
	// is.
	ClaimSecret string `json:"claimSecret,omitempty"`
}

func (m *Manifest) checkProvision() error {
	if !uuidLikeRE.MatchString(m.DeviceID) {
		return fmt.Errorf("provision package has no valid deviceId")
	}
	if m.Sequence <= 0 {
		return fmt.Errorf("provision package has no positive sequence")
	}
	if len(m.Files) != 1 || m.Files[0].Path != ProvisionFile {
		return fmt.Errorf("provision package must list exactly one file, %s", ProvisionFile)
	}
	if m.Files[0].Bytes > MaxProvisionBytes {
		return fmt.Errorf("%s is larger than %d KiB", ProvisionFile, MaxProvisionBytes>>10)
	}
	return nil
}

// ParseProvision decodes provision.json from a verified provision package
// with manifest m, and checks it: it must name the same device and sequence
// as the signed manifest, and carry a backend URL and a claim secret together
// or neither.
func ParseProvision(raw []byte, m *Manifest) (*Provision, error) {
	var p Provision
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, fmt.Errorf("%s is not valid JSON: %w", ProvisionFile, err)
	}
	switch {
	case p.Format != ProvisionFormat:
		return nil, fmt.Errorf("%s format %d is not supported by this agent (it reads %d)", ProvisionFile, p.Format, ProvisionFormat)
	case !strings.EqualFold(p.DeviceID, m.DeviceID):
		return nil, fmt.Errorf("%s is for device %s, its manifest for %s", ProvisionFile, p.DeviceID, m.DeviceID)
	case p.Sequence != m.Sequence:
		return nil, fmt.Errorf("%s has sequence %d, its manifest %d", ProvisionFile, p.Sequence, m.Sequence)
	case len(p.DisplayName) > 200 || strings.ContainsAny(p.DisplayName, "\n\r\x00"):
		return nil, fmt.Errorf("%s has an unusable displayName", ProvisionFile)
	case (p.BackendURL == "") != (p.ClaimSecret == ""):
		return nil, fmt.Errorf("%s must carry backendUrl and claimSecret together, or neither", ProvisionFile)
	}
	if p.IssuedAt != "" {
		if _, err := time.Parse(time.RFC3339, p.IssuedAt); err != nil {
			return nil, fmt.Errorf("%s issuedAt %q is not an RFC 3339 time", ProvisionFile, p.IssuedAt)
		}
	}
	if p.BackendURL != "" {
		u, err := url.Parse(p.BackendURL)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return nil, fmt.Errorf("%s backendUrl %q is not a plain http(s) base URL", ProvisionFile, p.BackendURL)
		}
		p.BackendURL = strings.TrimRight(p.BackendURL, "/")
		if _, err := DecodeClaimSecret(p.ClaimSecret); err != nil {
			return nil, fmt.Errorf("%s: %w", ProvisionFile, err)
		}
	}
	p.DeviceID = m.DeviceID
	return &p, nil
}

// DecodeClaimSecret decodes a base64url claim secret, padded or not.
func DecodeClaimSecret(s string) ([]byte, error) {
	b, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(s, "="))
	if err != nil || len(b) != ClaimSecretBytes {
		return nil, fmt.Errorf("claimSecret is not %d bytes of base64url", ClaimSecretBytes)
	}
	return b, nil
}
