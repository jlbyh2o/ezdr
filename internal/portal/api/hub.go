package api

import (
	"context"
	"sync"

	clientv1 "github.com/jlbyh2o/ezdr/internal/gen/ezdr/client/v1"
)

// Hub tracks which hosts have an open command stream, so the portal can show
// them as online and disconnect removed hosts immediately.
type Hub struct {
	mu    sync.Mutex
	conns map[string]map[*hubConn]struct{} // host ID -> open streams
}

type hubConn struct {
	cancel context.CancelFunc
	// send queues messages for the stream; Subscribe writes them out.
	send chan *clientv1.SubscribeResponse
}

// NewHub returns an empty Hub.
func NewHub() *Hub {
	return &Hub{conns: make(map[string]map[*hubConn]struct{})}
}

// connect registers a stream for hostID. The returned context is canceled
// when the host is disconnected, and the channel delivers messages to send on
// the stream. release must be called when the stream ends.
func (h *Hub) connect(ctx context.Context, hostID string) (context.Context, <-chan *clientv1.SubscribeResponse, func()) {
	ctx, cancel := context.WithCancel(ctx)
	c := &hubConn{cancel: cancel, send: make(chan *clientv1.SubscribeResponse, 16)}
	h.mu.Lock()
	if h.conns[hostID] == nil {
		h.conns[hostID] = make(map[*hubConn]struct{})
	}
	h.conns[hostID][c] = struct{}{}
	h.mu.Unlock()
	return ctx, c.send, func() {
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

// Send queues msg on one of hostID's open streams. It reports false if the
// host has no open stream or its queue is full.
func (h *Hub) Send(hostID string, msg *clientv1.SubscribeResponse) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for c := range h.conns[hostID] {
		select {
		case c.send <- msg:
			return true
		default:
		}
	}
	return false
}
