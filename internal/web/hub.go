// Package web serves the phone-facing HTTP interface.
package web

import (
	"context"
	"log"
	"sync"
	"time"
)

// CameraSource connects to the printer camera and passes frames to yield
// until ctx is cancelled.
type CameraSource func(ctx context.Context, yield func([]byte)) error

// Hub fans camera frames out to viewers, holding a printer connection only
// while at least one viewer is attached (so Bambu Studio can have the camera
// the rest of the time).
type Hub struct {
	source  CameraSource
	mu      sync.Mutex
	viewers map[chan []byte]struct{}
	cancel  context.CancelFunc
}

func NewHub(source CameraSource) *Hub {
	return &Hub{source: source, viewers: map[chan []byte]struct{}{}}
}

// Attach registers a viewer. The channel receives JPEG frames; call detach
// when the viewer leaves.
func (h *Hub) Attach() (frames <-chan []byte, detach func()) {
	ch := make(chan []byte, 1)
	h.mu.Lock()
	h.viewers[ch] = struct{}{}
	if h.cancel == nil {
		ctx, cancel := context.WithCancel(context.Background())
		h.cancel = cancel
		go h.run(ctx)
	}
	h.mu.Unlock()
	return ch, func() {
		h.mu.Lock()
		delete(h.viewers, ch)
		if len(h.viewers) == 0 && h.cancel != nil {
			h.cancel()
			h.cancel = nil
		}
		h.mu.Unlock()
	}
}

func (h *Hub) run(ctx context.Context) {
	for ctx.Err() == nil {
		err := h.source(ctx, h.broadcast)
		if ctx.Err() == nil {
			log.Printf("camera stream ended, retrying: %v", err)
			select {
			case <-time.After(2 * time.Second):
			case <-ctx.Done():
			}
		}
	}
}

func (h *Hub) broadcast(frame []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.viewers {
		select {
		case ch <- frame:
		default: // viewer still writing the previous frame — drop this one
		}
	}
}
