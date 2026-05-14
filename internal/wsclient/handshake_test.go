package wsclient

import (
	"crypto/ed25519"
	"encoding/base64"
	"testing"
)

func TestBuildAuthSignaturePayload_ExactFormat(t *testing.T) {
	got := string(BuildAuthSignaturePayload(
		"00000000-0000-0000-0000-000000000001",
		"deadbeef",
		"00000000-0000-0000-0000-000000000002",
		1700000000000,
	))
	want := "musallahboard-auth-v1\n" +
		"00000000-0000-0000-0000-000000000001\n" +
		"deadbeef\n" +
		"00000000-0000-0000-0000-000000000002\n" +
		"1700000000000"
	if got != want {
		t.Fatalf("payload mismatch:\n got=%q\nwant=%q", got, want)
	}
}

func TestSignAuth_RoundTripsThroughVerify(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}

	sessionID := "11111111-1111-1111-1111-111111111111"
	challenge := "aGVsbG8="
	deviceID := "22222222-2222-2222-2222-222222222222"
	ts := int64(1_700_000_000_000)

	sigB64 := SignAuth(priv, sessionID, challenge, deviceID, ts)
	sig, err := base64.StdEncoding.DecodeString(sigB64)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(sig) != ed25519.SignatureSize {
		t.Fatalf("signature size %d, want %d", len(sig), ed25519.SignatureSize)
	}

	payload := BuildAuthSignaturePayload(sessionID, challenge, deviceID, ts)
	if !ed25519.Verify(pub, payload, sig) {
		t.Fatalf("signature did not verify")
	}
}

func TestSignAuth_DifferentInputsProduceDifferentSignatures(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(nil)

	a := SignAuth(priv, "s1", "c", "d", 1)
	b := SignAuth(priv, "s2", "c", "d", 1) // changed sessionID
	c := SignAuth(priv, "s1", "c", "d", 2) // changed timestamp
	if a == b || a == c {
		t.Fatal("signatures must differ when payload differs")
	}
}
