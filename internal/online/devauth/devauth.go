// Package devauth signs device-authenticated HTTP requests to the backend
// (docs/architecture.md, section 9.2).
//
// The device proves who it is with the same Ed25519 key it uses for the
// WebSocket handshake, over a message that binds the method, path, time and
// body. The prefix line is domain separation: a signature made here can never
// be replayed as a WebSocket auth (musallahboard-auth-v1) or as a package
// signature (musallahboard-mbu-v2), and the other way round.
package devauth

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"strconv"
	"time"
)

// Prefix is the first line of the signed message. It must match the backend.
const Prefix = "musallahboard-http-v1"

// Header names.
const (
	HeaderDeviceID  = "X-MB-Device-Id"
	HeaderTimestamp = "X-MB-Timestamp"
	HeaderSignature = "X-MB-Signature"
)

// Message builds the exact bytes that are signed: six lines joined by "\n",
// with no trailing newline. path is the request path without its query
// string; the query is deliberately not covered, so nothing in it may matter
// to an authenticated endpoint.
func Message(method, path, deviceID string, timestampMillis int64, body []byte) []byte {
	sum := sha256.Sum256(body)
	s := Prefix + "\n" +
		method + "\n" +
		path + "\n" +
		deviceID + "\n" +
		strconv.FormatInt(timestampMillis, 10) + "\n" +
		hex.EncodeToString(sum[:])
	return []byte(s)
}

// Sign sets the three device-auth headers on req. body must be exactly the
// bytes req will send (nil or empty for none). The path signed is
// req.URL.Path, or "/" when it is empty, which is what the server sees.
func Sign(req *http.Request, body []byte, deviceID string, key ed25519.PrivateKey, now time.Time) {
	path := req.URL.Path
	if path == "" {
		path = "/"
	}
	ts := now.UnixMilli()
	sig := ed25519.Sign(key, Message(req.Method, path, deviceID, ts, body))
	req.Header.Set(HeaderDeviceID, deviceID)
	req.Header.Set(HeaderTimestamp, strconv.FormatInt(ts, 10))
	req.Header.Set(HeaderSignature, base64.StdEncoding.EncodeToString(sig))
}
