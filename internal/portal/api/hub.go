package api

import (
	"context"
	"sync"
)

// Hub tracks which hosts have an open command stream, so the portal can show
// them as online and disconnect removed hosts immediately.
type Hub struct {
	mu    sync.Mutex
	conns map[string]map[*hubConn]struct{} // host ID -> open streams
}

type hubConn struct {
	cancel context.CancelFunc
}

// NewHub returns an empty Hub.
func NewHub() *Hub {
	return &Hub{conns: make(map[string]map[*hubConn]struct{})}
}

// connect registers a stream for hostID. The returned context is canceled
// when the host is disconnected; release must be called when the stream ends.
func (h *Hub) connect(ctx context.Context, hostID string) (context.Context, func()) {
	ctx, cancel := context.WithCancel(ctx)
	c := &hubConn{cancel: cancel}
	h.mu.Lock()
	if h.conns[hostID] == nil {
		h.conns[hostID] = make(map[*hubConn]struct{})
	}
	h.conns[hostID][c] = struct{}{}
	h.mu.Unlock()
	return ctx, func() {
		cancel()
		h.mu.Lock()
		delete(h.conns[hostID], c)
		if len(h.conns[hostID]) == 0 {
			delete(h.conns, hostID)
		}
		h.mu.Unlock()
	}
}

// Online reports whether hostID has an open stream.
func (h *Hub) Online(hostID string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.conns[hostID]) > 0
}

// Disconnect closes all streams for hostID.
func (h *Hub) Disconnect(hostID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for c := range h.conns[hostID] {
		c.cancel()
	}
}
