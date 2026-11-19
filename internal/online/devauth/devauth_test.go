package devauth

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"net/http"
	"strings"
	"testing"
	"time"
)

const testDevice = "3f2a1b4c-5d6e-4f70-8a9b-0c1d2e3f4a5b"

// The message format is a contract with the backend; a change here without
// the same change there breaks every board's sync.
func TestMessageGolden(t *testing.T) {
	got := string(Message("POST", "/api/agent/content-bundle", testDevice, 1790258531000, []byte(`{"days":7}`)))
	want := "musallahboard-http-v1\n" +
		"POST\n" +
		"/api/agent/content-bundle\n" +
		testDevice + "\n" +
		"1790258531000\n" +
		// lowercase hex sha256 of {"days":7}
		"a2e0f104633a5821e6d1ec00745f8f1a10727d392249d4ddf92f4fcf7d3095ed"
	if got != want {
		t.Fatalf("message mismatch:\n got=%q\nwant=%q", got, want)
	}
}

func TestEmptyBodyHash(t *testing.T) {
	got := string(Message("GET", "/x", "d", 1, nil))
	const emptySHA = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	if !strings.HasSuffix(got, "\n"+emptySHA) {
		t.Fatalf("empty body must hash as the empty string: %q", got)
	}
}

func TestSignFixedKeyGolden(t *testing.T) {
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{7}, 32))
	req, _ := http.NewRequest("POST", "https://api.example/api/agent/content-bundle?ignored=1", nil)
	body := []byte(`{"days":7,"haveMedia":[]}`)
	now := time.UnixMilli(1790258531123)
	Sign(req, body, testDevice, key, now)

	if req.Header.Get(HeaderDeviceID) != testDevice {
		t.Errorf("device header = %q", req.Header.Get(HeaderDeviceID))
	}
	if req.Header.Get(HeaderTimestamp) != "1790258531123" {
		t.Errorf("timestamp header = %q", req.Header.Get(HeaderTimestamp))
	}
	sig, err := base64.StdEncoding.DecodeString(req.Header.Get(HeaderSignature))
	if err != nil {
		t.Fatal(err)
	}
	// The query string is not part of the signed path.
	msg := Message("POST", "/api/agent/content-bundle", testDevice, 1790258531123, body)
	if !ed25519.Verify(key.Public().(ed25519.PublicKey), msg, sig) {
		t.Fatal("signature does not verify over the documented message")
	}
	// Ed25519 is deterministic, so a fixed key and message give a fixed
	// signature: a golden value any other implementation can check against.
	const golden = "UCYvISeIAOj3C+g314gFko/54swdK2XPyNRCJu4xMCiK1N4Idgm1B443tj7+31hroI7pruE6JNfaav7R8uLdDQ=="
	if got := req.Header.Get(HeaderSignature); got != golden {
		t.Errorf("signature = %s, want %s", got, golden)
	}
}
