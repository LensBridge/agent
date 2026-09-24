package enroll

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/LensBridge/agent/internal/trust"
)

// SigningKey is one content signing key as the backend publishes it, in the
// enrollment response (contentSigningKeys) and at GET /api/agent/signing-keys.
type SigningKey struct {
	KeyID     string `json:"keyId"`
	PublicKey string `json:"publicKey"`
}

// PinContentKeys adds keys to the trust store at trustPath as content keys and
// returns the ids it added (keys already trusted are left as they are). Every
// key is checked before anything is written: a key whose id does not match
// its public key means the backend or the connection is broken, and none of
// that response is trusted.
//
// Keys are only ever added, never removed: a key the backend stopped
// publishing may still have signed the content on a USB stick in someone's
// pocket. Removal is a deliberate `trust remove`.
func PinContentKeys(trustPath string, keys []SigningKey, note string) ([]string, error) {
	pubs := make([][]byte, 0, len(keys))
	for _, k := range keys {
		pub, err := trust.ParsePublicKey(k.PublicKey)
		if err != nil {
			return nil, fmt.Errorf("content key %s: %w", k.KeyID, err)
		}
		if k.KeyID != "" && trust.KeyID(pub) != k.KeyID {
			return nil, fmt.Errorf("content key %s does not match its public key (its id is %s)", k.KeyID, trust.KeyID(pub))
		}
		pubs = append(pubs, pub)
	}
	s, err := trust.LoadForWrite(trustPath)
	if err != nil {
		return nil, err
	}
	var added []string
	for _, pub := range pubs {
		if s.Add(trust.RoleContent, pub, note) {
			added = append(added, trust.KeyID(pub))
		}
	}
	if len(added) == 0 {
		return nil, nil
	}
	if err := s.Save(trustPath); err != nil {
		return nil, fmt.Errorf("save %s: %w", trustPath, err)
	}
	return added, nil
}

// FetchSigningKeys gets the backend's published content keys (GET
// /api/agent/signing-keys). This is trust on first use over TLS, the same
// footing enrollment itself stands on.
func FetchSigningKeys(ctx context.Context, backendURL string, client *http.Client) ([]SigningKey, error) {
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	u := strings.TrimRight(backendURL, "/") + "/api/agent/signing-keys"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("bad backend URL %q: %w", backendURL, err)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("could not reach %s: %w", u, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("reading the answer from %s: %w", u, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s returned %d: %s", u, resp.StatusCode, truncate(raw, 300))
	}
	var body struct {
		Content []SigningKey `json:"content"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, fmt.Errorf("%s did not return the expected JSON: %w", u, err)
	}
	return body.Content, nil
}
