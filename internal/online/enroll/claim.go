package enroll

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/LensBridge/agent/internal/core/mbu"
	"github.com/LensBridge/agent/internal/online/api"
)

// The claim (spec C2) binds the board's own device key to a device the
// admin created in the console, using the claim secret from its setup file
// instead of an enrollment token:
//
//	POST /api/agent/claim
//	{"deviceId", "publicKey": <base64 std, 32 bytes>, "proof", "agentVersion", "hostname"}
//
// proof = base64url, unpadded, of HMAC-SHA256(key = the claim secret's 32
// bytes, message = deviceId + "\n" + publicKey), publicKey exactly as sent.
// The reply is the enrollment reply (api.AgentEnrollResponse). The backend
// binds a device with no key, answers the same again for the key it already
// has (so a lost reply is retried safely), and refuses a different key with
// 409. The private key never leaves the board.

// ErrKeyConflict is a 409: the backend has another key bound to this device.
// It needs a person: a new setup file, after the console resets the device.
var ErrKeyConflict = errors.New("the backend already has a different key for this device")

// ClaimProof is the claim's proof (see above).
func ClaimProof(secret []byte, deviceID, publicKey string) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(deviceID + "\n" + publicKey))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// ClaimParams is one claim.
type ClaimParams struct {
	BackendURL   string
	DeviceID     string
	ClaimSecret  string // base64url, as in the setup file
	Key          ed25519.PrivateKey
	AgentVersion string
	HTTP         *http.Client // optional
}

// Claim sends one claim and returns the backend's reply, checked to be for
// this device and to carry a WebSocket URL.
func Claim(ctx context.Context, p ClaimParams) (*api.AgentEnrollResponse, error) {
	secret, err := mbu.DecodeClaimSecret(p.ClaimSecret)
	if err != nil {
		return nil, err
	}
	pub := base64.StdEncoding.EncodeToString(p.Key.Public().(ed25519.PublicKey))
	hostname, _ := os.Hostname()
	body, err := json.Marshal(map[string]string{
		"deviceId":     p.DeviceID,
		"publicKey":    pub,
		"proof":        ClaimProof(secret, p.DeviceID, pub),
		"agentVersion": p.AgentVersion,
		"hostname":     hostname,
	})
	if err != nil {
		return nil, err
	}
	client := p.HTTP
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	u := strings.TrimRight(p.BackendURL, "/") + "/api/agent/claim"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("bad backend URL %q: %w", p.BackendURL, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "musallahboard-agent/"+p.AgentVersion)
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("could not reach %s: %w", u, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("reading the answer from %s: %w", u, err)
	}
	switch {
	case resp.StatusCode == http.StatusConflict:
		return nil, fmt.Errorf("%w (%s)", ErrKeyConflict, message(raw))
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("%s returned %d: %s", u, resp.StatusCode, message(raw))
	}
	var r api.AgentEnrollResponse
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, fmt.Errorf("%s did not return the expected JSON: %w", u, err)
	}
	if r.DeviceId == nil || !strings.EqualFold(r.DeviceId.String(), p.DeviceID) {
		return nil, fmt.Errorf("the backend answered the claim for another device (%v)", r.DeviceId)
	}
	if r.WebsocketUrl == nil || *r.WebsocketUrl == "" {
		return nil, errors.New("the backend's claim reply has no websocketUrl")
	}
	return &r, nil
}

// message is an error body's {"message"} or its first 300 bytes.
func message(raw []byte) string {
	var m struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(raw, &m) == nil && m.Message != "" {
		return m.Message
	}
	return truncate(raw, 300)
}
