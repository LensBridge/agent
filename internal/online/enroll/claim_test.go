package enroll

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/hmac"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The fixed vector the backend's own test uses (spec C2): device
// 00000000-0000-0000-0000-000000000001, claim secret 32 bytes of 0x01, the
// Ed25519 public key of the seed 32 bytes of 0x02.
const (
	vectorDevice = "00000000-0000-0000-0000-000000000001"
	vectorSecret = "AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE"
	vectorPub    = "gTl3Dqh9F19Wo1Rmw0x+zMuNipG07jeiXfYPW4/Js5Q="
	vectorProof  = "2_nUd8LSmcq1kdXUvLW5ZQkAr_yDjiTuIh2AzpJH2sA"
)

var vectorKey = ed25519.NewKeyFromSeed(bytes.Repeat([]byte{2}, 32))

func TestClaimProofVector(t *testing.T) {
	if got := base64.StdEncoding.EncodeToString(vectorKey.Public().(ed25519.PublicKey)); got != vectorPub {
		t.Fatalf("public key = %s", got)
	}
	if got := ClaimProof(bytes.Repeat([]byte{1}, 32), vectorDevice, vectorPub); got != vectorProof {
		t.Fatalf("proof = %s, want %s", got, vectorProof)
	}
}

func TestClaim(t *testing.T) {
	var got map[string]string
	status := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/agent/claim" {
			http.NotFound(w, r)
			return
		}
		json.NewDecoder(r.Body).Decode(&got)
		if status != http.StatusOK {
			w.WriteHeader(status)
			w.Write([]byte(`{"message":"device already has a different key"}`))
			return
		}
		json.NewEncoder(w).Encode(map[string]any{
			"deviceId": vectorDevice, "websocketUrl": "wss://api.example.org/api/agent/ws",
			"contentSigningKeys": []any{},
		})
	}))
	defer srv.Close()
	p := ClaimParams{BackendURL: srv.URL + "/", DeviceID: vectorDevice, ClaimSecret: vectorSecret,
		Key: vectorKey, AgentVersion: "0.4.0", HTTP: srv.Client()}

	r, err := Claim(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if *r.WebsocketUrl != "wss://api.example.org/api/agent/ws" {
		t.Fatalf("reply = %+v", r)
	}
	if got["deviceId"] != vectorDevice || got["publicKey"] != vectorPub || got["agentVersion"] != "0.4.0" {
		t.Fatalf("request = %v", got)
	}
	// What the backend checks: the proof, compared in constant time.
	if !hmac.Equal([]byte(got["proof"]), []byte(vectorProof)) {
		t.Fatalf("proof = %s", got["proof"])
	}

	status = http.StatusConflict
	if _, err := Claim(context.Background(), p); !errors.Is(err, ErrKeyConflict) {
		t.Fatalf("409: err = %v", err)
	}
	p.ClaimSecret = "short"
	if _, err := Claim(context.Background(), p); err == nil {
		t.Fatal("a malformed claim secret was sent")
	}
}
