package wsclient

import "encoding/json"

// IncomingFrame is the union of every frame the backend sends. Fields are all
// optional except {type, seq, sessionId} — which type is in play is decided by
// inspecting Type, not by Go's type system.
//
// We keep this flat (rather than a sum type per frame) because Go's JSON
// unmarshalling can't dispatch by a discriminator without a custom decoder,
// and a flat struct is simpler than the alternative.
type IncomingFrame struct {
	Type      string `json:"type"`
	Seq       int64  `json:"seq"`
	SessionID string `json:"sessionId"`

	// hello
	Challenge  string `json:"challenge,omitempty"`
	ServerTime int64  `json:"serverTime,omitempty"`

	// auth_ok
	DeviceID            string `json:"deviceId,omitempty"`
	HeartbeatIntervalMs int    `json:"heartbeatIntervalMs,omitempty"`

	// command
	CommandID  string          `json:"commandId,omitempty"`
	Kind       string          `json:"kind,omitempty"`
	IssuedBy   string          `json:"issuedBy,omitempty"`
	DeadlineMs int             `json:"deadlineMs,omitempty"`
	Payload    json.RawMessage `json:"payload,omitempty"`
}

// AuthFrame is the agent's seq:1 reply to hello.
type AuthFrame struct {
	Type      string `json:"type"`
	Seq       int64  `json:"seq"`
	SessionID string `json:"sessionId"`
	DeviceID  string `json:"deviceId"`
	Timestamp int64  `json:"timestamp"`
	Signature string `json:"signature"`
}

// HeartbeatFrame matches the backend's HeartbeatFrame.Telemetry shape exactly.
type HeartbeatFrame struct {
	Type      string    `json:"type"`
	Seq       int64     `json:"seq"`
	SessionID string    `json:"sessionId"`
	Telemetry Telemetry `json:"telemetry"`
}

// Telemetry mirrors HeartbeatFrame.Telemetry on the backend. Pointer types
// for fields that Java models as boxed (`Double`, `Boolean`) so a missing
// reading round-trips as `null` instead of `0`/`false`.
type Telemetry struct {
	UptimeSec     *int64   `json:"uptimeSec,omitempty"`
	CPUTempC      *float64 `json:"cpuTempC,omitempty"`
	ThrottleFlags string   `json:"throttleFlags,omitempty"`
	MemUsedMb     *int     `json:"memUsedMb,omitempty"`
	MemTotalMb    *int     `json:"memTotalMb,omitempty"`
	DiskUsedPct   *int     `json:"diskUsedPct,omitempty"`
	KioskAlive    *bool    `json:"kioskAlive,omitempty"`
	IPv4          []string `json:"ipv4,omitempty"`
	WifiSSID      string   `json:"wifiSsid,omitempty"`

	// AgentVersion lets the admin fleet list show what is actually running.
	// Without it the backend keeps reporting whatever version enrolled, so a
	// binary push looks like it never landed.
	AgentVersion string `json:"agentVersion,omitempty"`

	// DisplayedFrameKey is the kiosk's current slide key ("week", "poster-3"),
	// not an id. The backend typed it as a UUID once; every heartbeat from a
	// working board failed to parse and the session was closed as a bad frame.
	DisplayedFrameKey string `json:"displayedFrameKey,omitempty"`

	// Board is what the board runs and shows (localserver.BoardReport): the
	// admin portal's view of a board it cannot see.
	Board any `json:"board,omitempty"`
}

// CommandAckFrame, CommandProgressFrame, CommandResultFrame are the three
// frames the agent emits during the lifetime of one admin command.
type CommandAckFrame struct {
	Type      string `json:"type"`
	Seq       int64  `json:"seq"`
	SessionID string `json:"sessionId"`
	CommandID string `json:"commandId"`
}

type CommandProgressFrame struct {
	Type      string `json:"type"`
	Seq       int64  `json:"seq"`
	SessionID string `json:"sessionId"`
	CommandID string `json:"commandId"`
	Stage     string `json:"stage,omitempty"`
	Message   string `json:"message,omitempty"`
	Percent   *int   `json:"percent,omitempty"`
}

type CommandResultFrame struct {
	Type         string `json:"type"`
	Seq          int64  `json:"seq"`
	SessionID    string `json:"sessionId"`
	CommandID    string `json:"commandId"`
	Status       string `json:"status"`
	Output       any    `json:"output,omitempty"`
	ErrorMessage string `json:"errorMessage,omitempty"`
	DurationMs   *int64 `json:"durationMs,omitempty"`
}
