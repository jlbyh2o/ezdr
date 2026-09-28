package api

import (
	"context"
	"errors"
	"sync"

	clientv1 "github.com/jlbyh2o/ezdr/internal/gen/ezdr/client/v1"
	"github.com/jlbyh2o/ezdr/internal/portal/store"
)

// Hub tracks which hosts have an open command stream, so the portal can show
// them as online and disconnect removed hosts immediately.
type Hub struct {
	mu    sync.Mutex
	conns map[string]map[*hubConn]struct{} // host ID -> open streams
	// waiting maps action IDs to requests awaiting their acknowledgement.
	waiting map[string]*pendingAction
}

type pendingAction struct {
	hostID string
	ack    chan *clientv1.AckActionRequest
}

type hubConn struct {
	cancel context.CancelFunc
	// done is closed when the stream ends.
	done <-chan struct{}
	// send queues messages for the stream; Subscribe writes them out.
	send chan *clientv1.SubscribeResponse
}

// NewHub returns an empty Hub.
func NewHub() *Hub {
	return &Hub{conns: make(map[string]map[*hubConn]struct{}), waiting: make(map[string]*pendingAction)}
}

// connect registers a stream for hostID. The returned context is canceled
// when the host is disconnected, and the channel delivers messages to send on
// the stream. release must be called when the stream ends.
func (h *Hub) connect(ctx context.Context, hostID string) (context.Context, <-chan *clientv1.SubscribeResponse, func()) {
	ctx, cancel := context.WithCancel(ctx)
	c := &hubConn{cancel: cancel, done: ctx.Done(), send: make(chan *clientv1.SubscribeResponse, 16)}
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
	return h.send(hostID, msg) != nil
}

// send queues msg like Send and returns the stream it was queued on.
func (h *Hub) send(hostID string, msg *clientv1.SubscribeResponse) *hubConn {
	h.mu.Lock()
	defer h.mu.Unlock()
	for c := range h.conns[hostID] {
		select {
		case c.send <- msg:
			return c
		default:
		}
	}
	return nil
}

// ErrHostOffline is returned when an action can't be sent to a host, or the
// stream it was sent on ended before the host acknowledged it (the
// acknowledgement may be lost, so callers may resend idempotent actions).
var ErrHostOffline = errors.New("the host isn't connected")

// Request sends an action to hostID and waits for its acknowledgement, until
// ctx is done.
func (h *Hub) Request(ctx context.Context, hostID string, a *clientv1.Action) (*clientv1.AckActionRequest, error) {
	a.Id = store.NewID()
	p := &pendingAction{hostID: hostID, ack: make(chan *clientv1.AckActionRequest, 1)}
	h.mu.Lock()
	h.waiting[a.Id] = p
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		delete(h.waiting, a.Id)
		h.mu.Unlock()
	}()
	c := h.send(hostID, &clientv1.SubscribeResponse{Message: &clientv1.SubscribeResponse_Action{Action: a}})
	if c == nil {
		return nil, ErrHostOffline
	}
	select {
	case ack := <-p.ack:
		return ack, nil
	case <-c.done:
		return nil, ErrHostOffline
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// deliver passes an acknowledgement to the request waiting for it. Only the
// host the action was sent to can acknowledge it.
func (h *Hub) deliver(hostID string, ack *clientv1.AckActionRequest) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if p := h.waiting[ack.ActionId]; p != nil && p.hostID == hostID {
		select {
		case p.ack <- ack:
		default:
		}
	}
}
