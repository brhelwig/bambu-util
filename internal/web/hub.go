// Package web serves the phone-facing HTTP interface.
package web

import (
	"context"
	"log"
	"time"
)

// CameraSource connects to the printer camera and passes frames to yield
// until ctx is cancelled.
type CameraSource func(ctx context.Context, yield func([]byte)) error

// FrameSink is what the hub needs to persist a frame for history playback.
type FrameSink interface {
	InsertFrame(ts int64, jpeg []byte) error
}

// Default retry backoff bounds for Start. Overridable per-Hub (tests set
// smaller values so backoff growth doesn't slow the suite down).
const (
	defaultMinRetryDelay = 1 * time.Second
	defaultMaxRetryDelay = 30 * time.Second
)

// Hub holds the printer's camera connection for the life of the process and
// writes every frame to the history store, which all camera views read from.
// The printer serves one camera client, so Bambu Studio's view won't work
// while this runs.
type Hub struct {
	source                       CameraSource
	store                        FrameSink
	now                          func() time.Time
	minRetryDelay, maxRetryDelay time.Duration
}

// NewHub creates a Hub. Call Start once to begin the camera connection.
func NewHub(source CameraSource, store FrameSink) *Hub {
	return &Hub{
		source:        source,
		store:         store,
		now:           time.Now,
		minRetryDelay: defaultMinRetryDelay,
		maxRetryDelay: defaultMaxRetryDelay,
	}
}

// Start connects to the camera and keeps it connected — retrying on
// failure with exponential backoff — until ctx is cancelled. Call once,
// from main.
func (h *Hub) Start(ctx context.Context) {
	delay := h.minRetryDelay
	for ctx.Err() == nil {
		gotFrame := false
		err := h.source(ctx, func(frame []byte) {
			gotFrame = true
			if err := h.store.InsertFrame(h.now().Unix(), frame); err != nil {
				log.Printf("camera: storing frame: %v", err)
			}
		})
		if ctx.Err() != nil {
			return
		}
		if gotFrame {
			// It was working before it dropped; reset the backoff.
			delay = h.minRetryDelay
		}
		log.Printf("camera: stream ended, retrying in %s: %v", delay, err)
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return
		}
		delay = h.nextBackoff(delay)
	}
}

// nextBackoff doubles prev, capped at maxRetryDelay.
func (h *Hub) nextBackoff(prev time.Duration) time.Duration {
	next := prev * 2
	if next > h.maxRetryDelay || next <= 0 { // next<=0 guards against overflow
		return h.maxRetryDelay
	}
	return next
}
