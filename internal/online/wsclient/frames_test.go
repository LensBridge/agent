package wsclient

import (
	"encoding/json"
	"testing"
)

// The wire names here are a contract with the backend's HeartbeatFrame.Telemetry.
// Renaming a field without renaming it there means the value is silently dropped,
// which is how agentVersion went missing for the entire fleet.
func TestTelemetryWireNames(t *testing.T) {
	up, temp, used, total, disk, alive := int64(3600), 51.5, 512, 2048, 42, true
	tel := Telemetry{
		UptimeSec:         &up,
		CPUTempC:          &temp,
		ThrottleFlags:     "0x0",
		MemUsedMb:         &used,
		MemTotalMb:        &total,
		DiskUsedPct:       &disk,
		KioskAlive:        &alive,
		DisplayedFrameKey: "poster-3",
		IPv4:              []string{"10.0.0.7"},
		WifiSSID:          "MSA-WIFI",
		AgentVersion:      "1.2.3",
	}

	raw, err := json.Marshal(tel)
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
		"diskUsedPct":       float64(42),
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

// Missing readings must serialize as absent, not as a misleading zero.
func TestTelemetryOmitsUnreadValues(t *testing.T) {
	dead := false
	raw, err := json.Marshal(Telemetry{KioskAlive: &dead})
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
	if got["kioskAlive"] != false {
		t.Errorf("kioskAlive = %v, want false", got["kioskAlive"])
	}
}
