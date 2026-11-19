package wsclient

import (
	"crypto/ed25519"
	"encoding/base64"
	"strconv"
)

// AuthVersionPrefix is the first line of the auth signature payload. Must
// match com.ibrasoft.lensbridge.service.agent.handshake.AuthSignaturePayload
// on the backend — bumping this here without bumping the backend will break
// every device.
const AuthVersionPrefix = "musallahboard-auth-v1"

// BuildAuthSignaturePayload constructs the exact bytes the agent must sign.
// The format mirrors the backend's AuthSignaturePayload.build():
//
//	"musallahboard-auth-v1\n" + sessionId + "\n" + challenge + "\n" + deviceId + "\n" + timestampMillis
func BuildAuthSignaturePayload(sessionID, challengeBase64, deviceID string, timestampMillis int64) []byte {
	var s string
	s += AuthVersionPrefix + "\n"
	s += sessionID + "\n"
	s += challengeBase64 + "\n"
	s += deviceID + "\n"
	s += strconv.FormatInt(timestampMillis, 10)
	return []byte(s)
}

// SignAuth signs the canonical auth payload and returns a standard-base64
// signature (Java's Base64.getEncoder()). This is the value placed in
// AuthFrame.Signature.
func SignAuth(priv ed25519.PrivateKey, sessionID, challengeBase64, deviceID string, timestampMillis int64) string {
	payload := BuildAuthSignaturePayload(sessionID, challengeBase64, deviceID, timestampMillis)
	sig := ed25519.Sign(priv, payload)
	return base64.StdEncoding.EncodeToString(sig)
}
