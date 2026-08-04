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
	"context"
	"encoding/base64"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/LensBridge/agent/internal/api"
	"github.com/LensBridge/agent/internal/config"
	"github.com/LensBridge/agent/internal/keystore"
	"github.com/LensBridge/agent/internal/kioskurl"
)

type Params struct {
	Token        string
	BackendURL   string
	ConfigPath   string
	KeyPath      string // optional; defaults to config.DefaultKeyPath
	AgentVersion string
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
	hardwareModel := readHardwareModel()

	// Client and request/response types are generated from the backend's
	// openapi.yaml (see internal/api/oapi-codegen.yaml). A field renamed on the
	// server now breaks this build instead of silently decoding to a zero value.
	client, err := api.NewClientWithResponses(
		strings.TrimRight(p.BackendURL, "/"),
		api.WithHTTPClient(&http.Client{Timeout: 30 * time.Second}),
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

	// The backend sometimes constructs the WebSocket URL from the HTTP request's
	// Host header, which drops the port when behind a reverse proxy or when the
	// client connects directly without a Host: port. Patch it back in if the
	// backend URL carried an explicit port that the returned WS URL is missing.
	websocketURL := restorePort(*enrolled.WebsocketUrl, p.BackendURL)

	// Persist the private key BEFORE the config: if we crash between the two,
	// re-running enroll is harmless (token is consumed, but no orphan config
	// claims to point at a key file that doesn't exist).
	if err := keystore.Save(keyPath, priv); err != nil {
		return fmt.Errorf("save key: %w", err)
	}
	_ = chownToDirOwner(keyPath, filepath.Dir(keyPath))

	cfg := &config.Config{
		DeviceID:     deviceID,
		BackendURL:   strings.TrimRight(p.BackendURL, "/"),
		WebSocketURL: websocketURL,
		KeyPath:      keyPath,
	}
	if err := config.Save(p.ConfigPath, cfg); err != nil {
		return fmt.Errorf("save config: %w", err)
	}
	_ = chownToDirOwner(p.ConfigPath, filepath.Dir(p.ConfigPath))

	// Compose the kiosk URL now. This file is also the kiosk unit's enrollment
	// sentinel — writing it here is what releases cage/Chromium to launch the
	// board for the first time. Non-fatal: if the operator hasn't provisioned
	// the base board URL yet, the daemon retries this on every startup.
	if err := kioskurl.Write(kioskurl.DefaultBoardURLPath, kioskurl.DefaultOutPath, deviceID); err != nil {
		logger.Warn("could not compose kiosk url (kiosk will wait)", "err", err)
	} else {
		logger.Info("kiosk url written", "path", kioskurl.DefaultOutPath)
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

// restorePort patches wsURL's host with the port from backendURL when the
// backend omits it — this happens when the backend derives the WebSocket URL
// from the HTTP Host header, which proxies or direct HTTP/1.0 clients may
// omit the port from.
func restorePort(wsURL, backendURL string) string {
	ws, err := url.Parse(wsURL)
	if err != nil || ws.Port() != "" {
		return wsURL // already has a port (or unparseable — leave alone)
	}
	be, err := url.Parse(backendURL)
	if err != nil || be.Port() == "" {
		return wsURL // backend URL has no explicit port either — nothing to restore
	}
	ws.Host = ws.Hostname() + ":" + be.Port()
	return ws.String()
}
