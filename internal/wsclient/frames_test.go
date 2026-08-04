package wsclient

import (
	"encoding/json"
	"testing"

	"github.com/LensBridge/agent/internal/telemetry"
)

// The wire names here are a contract with the backend's HeartbeatFrame.Telemetry.
// Renaming a field without renaming it there means the value is silently dropped,
// which is how agentVersion went missing for the entire fleet.
func TestTelemetryFromSnapshotWireNames(t *testing.T) {
	snap := telemetry.Snapshot{
		UptimeSec:         3600,
		CPUTempC:          51.5,
		ThrottleFlags:     "0x0",
		MemUsedMB:         512,
		MemTotalMB:        2048,
		DiskUsedPct:       41.6,
		KioskAlive:        true,
		DisplayedFrameKey: "poster-3",
		IPAddrs:           []string{"10.0.0.7"},
		SSID:              "MSA-WIFI",
		AgentVersion:      "1.2.3",
	}

	raw, err := json.Marshal(TelemetryFromSnapshot(snap))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	want := map[string]any{
		"uptimeSec":         float64(3600),
		"cpuTempC":          51.5,
		"throttleFlags":     "0x0",
		"memUsedMb":         float64(512),
		"memTotalMb":        float64(2048),
		"diskUsedPct":       float64(42), // rounded to nearest percent
		"kioskAlive":        true,
		"displayedFrameKey": "poster-3",
		"wifiSsid":          "MSA-WIFI",
		"agentVersion":      "1.2.3",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %v (%T), want %v", k, got[k], got[k], v)
		}
	}
	if ips, ok := got["ipv4"].([]any); !ok || len(ips) != 1 || ips[0] != "10.0.0.7" {
		t.Errorf("ipv4 = %v, want [10.0.0.7]", got["ipv4"])
	}
}

// The frame key is a kiosk slide key, never a UUID. Asserting a value the
// backend could not parse as one keeps that explicit.
func TestTelemetryFrameKeyIsNotAUUID(t *testing.T) {
	tel := TelemetryFromSnapshot(telemetry.Snapshot{DisplayedFrameKey: "next-prayer"})
	if tel.DisplayedFrameKey != "next-prayer" {
		t.Fatalf("DisplayedFrameKey = %q", tel.DisplayedFrameKey)
	}
}

// Missing readings must serialize as absent, not as a misleading zero.
func TestTelemetryFromSnapshotOmitsUnreadValues(t *testing.T) {
	raw, err := json.Marshal(TelemetryFromSnapshot(telemetry.Snapshot{}))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	for _, absent := range []string{"uptimeSec", "cpuTempC", "memUsedMb", "memTotalMb",
		"diskUsedPct", "displayedFrameKey", "agentVersion", "wifiSsid", "ipv4"} {
		if _, present := got[absent]; present {
			t.Errorf("%s should be omitted when unread, got %v", absent, got[absent])
		}
	}
	// kioskAlive is always sent: "we could not tell" and "the board is down"
	// must not look the same to the fleet dashboard.
	if got["kioskAlive"] != false {
		t.Errorf("kioskAlive = %v, want false", got["kioskAlive"])
	}
}
