package web

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

type fakeSource struct {
	starts atomic.Int32
	done   atomic.Bool
}

func (f *fakeSource) run(ctx context.Context, yield func([]byte)) error {
	f.starts.Add(1)
	yield([]byte{0xFF, 0xD8})
	<-ctx.Done()
	f.done.Store(true)
	return ctx.Err()
}

func TestHubStartsOncePerViewerSet(t *testing.T) {
	src := &fakeSource{}
	h := NewHub(src.run)

	ch1, detach1 := h.Attach()
	select {
	case <-ch1:
	case <-time.After(time.Second):
		t.Fatal("no frame delivered to first viewer")
	}
	_, detach2 := h.Attach()
	if n := src.starts.Load(); n != 1 {
		t.Fatalf("source started %d times, want 1", n)
	}

	detach1()
	if src.done.Load() {
		t.Fatal("source stopped while a viewer remained")
	}
	detach2()
	deadline := time.Now().Add(time.Second)
	for !src.done.Load() {
		if time.Now().After(deadline) {
			t.Fatal("source not stopped after last viewer left")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
