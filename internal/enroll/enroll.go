// Package enroll handles first-boot device registration.
//
// On the Pi: generate a fresh Ed25519 keypair, send the public key (raw 32
// bytes, base64) along with a one-time admin-issued token to the backend, and
// persist the (deviceId, websocketUrl) reply alongside the private key.
//
// The private key never leaves the device. The token is single-use and
// time-bound — the backend rejects replays, so re-running `agent enroll`
// requires a fresh token from the admin UI.
package enroll

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/utmmsa/musallahboard-agent/internal/config"
	"github.com/utmmsa/musallahboard-agent/internal/keystore"
)

type Params struct {
	Token        string
	BackendURL   string
	ConfigPath   string
	KeyPath      string // optional; defaults to config.DefaultKeyPath
	AgentVersion string
}

type enrollRequest struct {
	Token         string `json:"token"`
	PublicKey     string `json:"publicKey"`
	Hostname      string `json:"hostname"`
	HardwareModel string `json:"hardwareModel,omitempty"`
	AgentVersion  string `json:"agentVersion,omitempty"`
}

type enrollResponse struct {
	DeviceID     string `json:"deviceId"`
	WebSocketURL string `json:"websocketUrl"`
}

// Run performs first-boot enrollment. Idempotent only if the same token is
// retried on the same network failure — once the backend has consumed the
// token, the same one will be rejected on retry.
func Run(ctx context.Context, logger *slog.Logger, p Params) error {
	keyPath := p.KeyPath
	if keyPath == "" {
		keyPath = config.DefaultKeyPath
	}

	pub, priv, err := keystore.Generate()
	if err != nil {
		return fmt.Errorf("keygen: %w", err)
	}

	hostname, _ := os.Hostname()
	body := enrollRequest{
		Token:         p.Token,
		PublicKey:     base64.StdEncoding.EncodeToString(pub),
		Hostname:      hostname,
		HardwareModel: readHardwareModel(),
		AgentVersion:  p.AgentVersion,
	}
	bodyBytes, _ := json.Marshal(body)

	url := strings.TrimRight(p.BackendURL, "/") + "/api/agent/enroll"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(bodyBytes))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	httpClient := &http.Client{Timeout: 30 * time.Second}
	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("post: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("backend returned %d: %s", resp.StatusCode, msg)
	}

	var er enrollResponse
	if err := json.NewDecoder(resp.Body).Decode(&er); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	if er.DeviceID == "" || er.WebSocketURL == "" {
		return fmt.Errorf("backend response missing deviceId or websocketUrl")
	}

	// Persist the private key BEFORE the config: if we crash between the two,
	// re-running enroll is harmless (token is consumed, but no orphan config
	// claims to point at a key file that doesn't exist).
	if err := keystore.Save(keyPath, priv); err != nil {
		return fmt.Errorf("save key: %w", err)
	}

	cfg := &config.Config{
		DeviceID:     er.DeviceID,
		BackendURL:   strings.TrimRight(p.BackendURL, "/"),
		WebSocketURL: er.WebSocketURL,
		KeyPath:      keyPath,
	}
	if err := config.Save(p.ConfigPath, cfg); err != nil {
		return fmt.Errorf("save config: %w", err)
	}

	logger.Info("device enrolled",
		"deviceId", er.DeviceID,
		"configPath", p.ConfigPath,
		"keyPath", keyPath,
		"websocketUrl", er.WebSocketURL,
	)
	return nil
}

func readHardwareModel() string {
	b, err := os.ReadFile("/proc/device-tree/model")
	if err != nil {
		return ""
	}
	return strings.TrimRight(strings.TrimSpace(string(b)), "\x00")
}
