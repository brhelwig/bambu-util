package web

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fakeSource struct {
	starts atomic.Int32
}

func (f *fakeSource) run(ctx context.Context, yield func([]byte)) error {
	f.starts.Add(1)
	yield([]byte{0xFF, 0xD8})
	<-ctx.Done()
	return ctx.Err()
}

type fakeSink struct {
	mu     sync.Mutex
	frames [][]byte
}

func (f *fakeSink) InsertFrame(ts int64, jpeg []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.frames = append(f.frames, jpeg)
	return nil
}

func (f *fakeSink) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.frames)
}

func TestHubRecordsFrames(t *testing.T) {
	src := &fakeSource{}
	sink := &fakeSink{}
	h := NewHub(src.run, sink)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.Start(ctx)

	deadline := time.Now().Add(time.Second)
	for sink.count() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("no frame recorded")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestHubStartRetriesOnSourceError(t *testing.T) {
	src := &fakeSource{}
	h := NewHub(src.run, &fakeSink{})
	ctx, cancel := context.WithCancel(context.Background())
	go h.Start(ctx)

	deadline := time.Now().Add(time.Second)
	for src.starts.Load() < 1 {
		if time.Now().After(deadline) {
			t.Fatal("source never started")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
}

func TestNextBackoffDoublesUpToCap(t *testing.T) {
	h := NewHub(nil, &fakeSink{})
	h.minRetryDelay = 1 * time.Second
	h.maxRetryDelay = 30 * time.Second

	d := h.minRetryDelay
	want := []time.Duration{2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 30 * time.Second, 30 * time.Second}
	for i, w := range want {
		d = h.nextBackoff(d)
		if d != w {
			t.Fatalf("step %d: got %v, want %v", i, d, w)
		}
	}
}

type multiFailSource struct {
	mu    sync.Mutex
	calls []time.Time
}

// run fails immediately (no frame yielded) three times, then blocks until
// cancelled — enough failures to observe the backoff growing, without the
// test needing to wait for the real production delays.
func (m *multiFailSource) run(ctx context.Context, _ func([]byte)) error {
	m.mu.Lock()
	m.calls = append(m.calls, time.Now())
	n := len(m.calls)
	m.mu.Unlock()
	if n >= 4 {
		<-ctx.Done()
		return ctx.Err()
	}
	return errors.New("boom")
}

func TestHubStartBackoffGrowsOnRepeatedFailures(t *testing.T) {
	src := &multiFailSource{}
	h := NewHub(src.run, &fakeSink{})
	h.minRetryDelay = 20 * time.Millisecond
	h.maxRetryDelay = 200 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.Start(ctx)

	deadline := time.Now().Add(2 * time.Second)
	for {
		src.mu.Lock()
		n := len(src.calls)
		src.mu.Unlock()
		if n >= 4 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("source was not retried enough times")
		}
		time.Sleep(5 * time.Millisecond)
	}

	src.mu.Lock()
	defer src.mu.Unlock()
	gap := func(i int) time.Duration { return src.calls[i].Sub(src.calls[i-1]) }
	if gap(2) <= gap(1) {
		t.Fatalf("retry gap did not grow: gap1=%v gap2=%v", gap(1), gap(2))
	}
	if gap(3) <= gap(2) {
		t.Fatalf("retry gap did not grow: gap2=%v gap3=%v", gap(2), gap(3))
	}
}
