// Package events fans install notifications out to the kiosk page as
// Server-Sent Events (docs/architecture.md, section 7, /api/local/events).
//
// The page is the only subscriber in practice, but Chromium can briefly hold
// two connections across a reload, so this is a small broadcast hub rather
// than a single channel. A slow subscriber loses events instead of blocking
// an install: the page also polls, so a lost event costs at most a minute.
package events

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/LensBridge/agent/internal/notice"
)

// Event is one SSE message.
type Event struct {
	Name string
	Data any
}

// Hub broadcasts events. The zero value is ready to use.
type Hub struct {
	mu   sync.Mutex
	subs map[chan Event]struct{}
}

// ContentChanged implements importer.Events.
func (h *Hub) ContentChanged(sequence int64) {
	h.Publish(Event{Name: "content", Data: map[string]int64{"sequence": sequence}})
}

// AppChanged implements importer.Events.
func (h *Hub) AppChanged(version string) {
	h.Publish(Event{Name: "app", Data: map[string]string{"version": version}})
}

// Notice implements importer.Events: a banner for the running board.
func (h *Hub) Notice(n notice.Notice) {
	h.Publish(Event{Name: "notice", Data: n})
}

// UpdatesChanged announces a change in the software waiting to install
// (updates.Info), so the ticker can say so without waiting for a poll.
func (h *Hub) UpdatesChanged(info any) {
	h.Publish(Event{Name: "updates", Data: info})
}

// Publish sends e to every subscriber that has room for it.
func (h *Hub) Publish(e Event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for c := range h.subs {
		select {
		case c <- e:
		default:
		}
	}
}

func (h *Hub) subscribe() chan Event {
	c := make(chan Event, 8)
	h.mu.Lock()
	if h.subs == nil {
		h.subs = map[chan Event]struct{}{}
	}
	h.subs[c] = struct{}{}
	h.mu.Unlock()
	return c
}

func (h *Hub) unsubscribe(c chan Event) {
	h.mu.Lock()
	delete(h.subs, c)
	h.mu.Unlock()
}

// PingInterval keeps idle connections open through anything that times them
// out, and lets the server notice a closed page.
const PingInterval = 25 * time.Second

// ServeHTTP streams events to one client until it disconnects.
func (h *Hub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, "retry: 5000\n\n")
	fl.Flush()

	c := h.subscribe()
	defer h.unsubscribe(c)
	t := time.NewTicker(PingInterval)
	defer t.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case e := <-c:
			data, _ := json.Marshal(e.Data)
			if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", e.Name, data); err != nil {
				return
			}
			fl.Flush()
		case <-t.C:
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return
			}
			fl.Flush()
		}
	}
}
