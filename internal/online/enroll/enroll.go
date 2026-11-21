// Package enroll handles first-boot device registration.
//
// On the Pi: generate a fresh Ed25519 keypair, send the public key (raw 32
// bytes, base64) along with a one-time admin-issued token to the backend, and
// persist the (deviceId, websocketUrl) reply alongside the private key. The
// reply also carries the backend's content signing keys, which are pinned in
// the trust store so the board can verify the content packages it syncs.
//
// The private key never leaves the device. The token is single-use and
// time-bound — the backend rejects replays, so re-running `agent enroll`
// requires a fresh token from the admin UI.
package enroll

import (
	"context"
	"encoding/base64"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/LensBridge/agent/internal/core/config"
	"github.com/LensBridge/agent/internal/core/trust"
	"github.com/LensBridge/agent/internal/online/api"
	"github.com/LensBridge/agent/internal/online/keystore"
)

type Params struct {
	Token      string
	BackendURL string
	ConfigPath string
	KeyPath    string // optional; defaults to config.KeyPath
	// TrustPath is the trust store the backend's content keys are pinned
	// in; optional, defaults to trust.DefaultPath.
	TrustPath    string
	AgentVersion string
	// InsecureDev allows a plain http:// backend and a ws:// WebSocket URL,
	// for development boards only. It is saved as insecure_dev = true.
	InsecureDev bool
	// HTTPClient is optional; tests pass their TLS server's client.
	HTTPClient *http.Client
}

// Run performs first-boot enrollment. Idempotent only if the same token is
// retried on the same network failure — once the backend has consumed the
// token, the same one will be rejected on retry.
func Run(ctx context.Context, logger *slog.Logger, p Params) error {
	keyPath := p.KeyPath
	if keyPath == "" {
		keyPath = config.KeyPath
	}

	// Before anything goes on the wire: the reply pins the content keys and
	// the WebSocket URL, so it must come over TLS.
	if err := config.CheckBackendURL(p.BackendURL, p.InsecureDev); err != nil {
		return err
	}
	if p.InsecureDev {
		logger.Warn("INSECURE DEVELOPMENT ENROLLMENT: this board will accept a plaintext backend (insecure_dev = true in its config); anyone on the network can impersonate the backend. Never use this on a deployed board.",
			"backend", p.BackendURL)
	}

	pub, priv, err := keystore.Generate()
	if err != nil {
		return fmt.Errorf("keygen: %w", err)
	}

	hostname, _ := os.Hostname()
	hardwareModel := readHardwareModel()

	// Client and request/response types are generated from the backend's
	// openapi.yaml (see internal/online/api/oapi-codegen.yaml). A field renamed on the
	// server now breaks this build instead of silently decoding to a zero value.
	httpClient := p.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	client, err := api.NewClientWithResponses(
		strings.TrimRight(p.BackendURL, "/"),
		api.WithHTTPClient(httpClient),
	)
	if err != nil {
		return fmt.Errorf("build client: %w", err)
	}

	resp, err := client.EnrollAgentWithResponse(ctx, api.EnrollAgentJSONRequestBody{
		Token:         p.Token,
		PublicKey:     base64.StdEncoding.EncodeToString(pub),
		Hostname:      hostname,
		HardwareModel: optional(hardwareModel),
		AgentVersion:  optional(p.AgentVersion),
	})
	if err != nil {
		return fmt.Errorf("post: %w", err)
	}

	enrolled := resp.JSON200
	if enrolled == nil {
		// 400 and 401 both carry a MessageResponse; the default case covers
		// anything the spec does not enumerate.
		if msg := firstNonNil(resp.JSON400, resp.JSON401, resp.JSONDefault); msg != nil && msg.Message != nil {
			return fmt.Errorf("backend returned %d: %s", resp.StatusCode(), *msg.Message)
		}
		return fmt.Errorf("backend returned %d: %s", resp.StatusCode(), truncate(resp.Body, 4096))
	}
	if enrolled.DeviceId == nil || enrolled.WebsocketUrl == nil {
		return fmt.Errorf("backend response missing deviceId or websocketUrl")
	}

	deviceID := enrolled.DeviceId.String()

	var contentKeys []SigningKey
	for _, k := range enrolled.ContentSigningKeys {
		contentKeys = append(contentKeys, SigningKey{KeyID: k.KeyId, PublicKey: k.PublicKey})
	}

	websocketURL := *enrolled.WebsocketUrl
	if err := config.CheckWebSocketURL(websocketURL, p.BackendURL, p.InsecureDev); err != nil {
		// Nothing is saved: the board stays unenrolled. The backend has
		// consumed the token, so a retry needs a new one once it is fixed.
		return fmt.Errorf("the backend's reply cannot be used: %w", err)
	}

	// Persist the private key BEFORE the config: if we crash between the two,
	// re-running enroll is harmless (token is consumed, but no orphan config
	// claims to point at a key file that doesn't exist).
	if err := keystore.Save(keyPath, priv); err != nil {
		return fmt.Errorf("save key: %w", err)
	}

	// The board's other settings (a setup file's name, service_port, ...)
	// stay; the online identity is the backend's answer.
	cfg, err := config.Load(p.ConfigPath)
	if err != nil {
		cfg = &config.Config{}
	}
	if cfg.DeviceID != deviceID {
		if cfg.DeviceID != "" {
			logger.Warn("the backend enrolled this board as a different device than its setup file named",
				"setupDeviceId", cfg.DeviceID, "deviceId", deviceID)
		}
		cfg.DeviceID, cfg.DisplayName, cfg.ProvisionSequence = deviceID, "", 0
	}
	cfg.BackendURL = strings.TrimRight(p.BackendURL, "/")
	cfg.WebSocketURL = websocketURL
	cfg.ClaimSecret = ""
	if p.InsecureDev {
		on := true
		cfg.InsecureDevSet = &on
	}
	if err := config.Save(p.ConfigPath, cfg); err != nil {
		return fmt.Errorf("save config: %w", err)
	}

	// Pin the backend's content keys. They arrive over the same TLS
	// connection that just established this device's identity, which is as
	// much as any later fetch could offer. Failing here would strand a
	// device the backend already counts as enrolled, so it is a warning
	// with the fix in it, not an error.
	trustPath := p.TrustPath
	if trustPath == "" {
		trustPath = trust.DefaultPath
	}
	if len(contentKeys) == 0 {
		logger.Warn("the backend sent no content signing keys, so this board cannot verify content until they are fetched: run `sudo musallahboard-agent trust fetch` once the backend has a content key configured")
	} else if added, err := PinContentKeys(trustPath, contentKeys, "lensbridge"); err != nil {
		logger.Warn("could not pin the backend's content signing keys; run `sudo musallahboard-agent trust fetch`", "err", err, "path", trustPath)
	} else {
		logger.Info("content signing keys pinned", "received", len(contentKeys), "added", len(added), "path", trustPath)
	}

	logger.Info("device enrolled",
		"deviceId", deviceID,
		"configPath", p.ConfigPath,
		"keyPath", keyPath,
		"websocketUrl", websocketURL,
	)
	return nil
}

// optional maps an empty string to nil, matching the spec's omitempty fields:
// the generated struct uses *string so an absent value is distinguishable from
// a deliberately empty one.
func optional(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// firstNonNil returns the first non-nil MessageResponse, so the caller can
// report whichever error shape the backend actually sent.
func firstNonNil(candidates ...*api.MessageResponse) *api.MessageResponse {
	for _, c := range candidates {
		if c != nil {
			return c
		}
	}
	return nil
}

func truncate(b []byte, max int) string {
	if len(b) > max {
		return string(b[:max])
	}
	return string(b)
}

func readHardwareModel() string {
	b, err := os.ReadFile("/proc/device-tree/model")
	if err != nil {
		return ""
	}
	return strings.TrimRight(strings.TrimSpace(string(b)), "\x00")
}
