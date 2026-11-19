package wsclient

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/coder/websocket"
)

// session is one authenticated WS connection's worth of state. It owns the
// outbound seq counter, the send-mutex around the transport, and lookups for
// sessionID + deviceID needed by emitted frames.
type session struct {
	conn      *websocket.Conn
	sessionID string
	deviceID  string

	outSeq atomic.Int64 // next seq returned by nextSeq()

	mu sync.Mutex // serializes writes to conn
}

func newSession(conn *websocket.Conn, sessionID, deviceID string) *session {
	return &session{conn: conn, sessionID: sessionID, deviceID: deviceID}
}

func (s *session) nextSeq() int64 { return s.outSeq.Add(1) }

func (s *session) sendJSON(ctx context.Context, frame any) error {
	data, err := json.Marshal(frame)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.conn.Write(ctx, websocket.MessageText, data)
}

// Sender is the surface command handlers (L6c) use to emit ack/progress/result
// frames back up the channel. Implementations are bound to a specific
// (session, commandId) and never block on the network beyond the configured
// write deadline.
type Sender interface {
	Ack(ctx context.Context, commandID string) error
	Progress(ctx context.Context, commandID, stage, message string, percent *int) error
	Result(ctx context.Context, commandID, status string, output any, errorMessage string, durationMs int64) error
}

type sessionSender struct{ s *session }

func (ss sessionSender) Ack(ctx context.Context, commandID string) error {
	return ss.s.sendJSON(ctx, CommandAckFrame{
		Type:      "command_ack",
		Seq:       ss.s.nextSeq(),
		SessionID: ss.s.sessionID,
		CommandID: commandID,
	})
}

func (ss sessionSender) Progress(ctx context.Context, commandID, stage, message string, percent *int) error {
	return ss.s.sendJSON(ctx, CommandProgressFrame{
		Type:      "command_progress",
		Seq:       ss.s.nextSeq(),
		SessionID: ss.s.sessionID,
		CommandID: commandID,
		Stage:     stage,
		Message:   message,
		Percent:   percent,
	})
}

func (ss sessionSender) Result(ctx context.Context, commandID, status string, output any, errorMessage string, durationMs int64) error {
	d := durationMs
	return ss.s.sendJSON(ctx, CommandResultFrame{
		Type:         "command_result",
		Seq:          ss.s.nextSeq(),
		SessionID:    ss.s.sessionID,
		CommandID:    commandID,
		Status:       status,
		Output:       output,
		ErrorMessage: errorMessage,
		DurationMs:   &d,
	})
}
