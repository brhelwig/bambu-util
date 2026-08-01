package history

import (
	"context"
	"sync"
	"testing"
	"time"
)

// waitFor gives a background loop until it has done the thing, then fails.
func waitFor(t *testing.T, what string, done func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !done() {
		if time.Now().After(deadline) {
			t.Fatalf("%s never happened", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// requireStopped fails unless the loop returns once its context is cancelled.
func requireStopped(t *testing.T, stopped <-chan struct{}) {
	t.Helper()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("the loop kept running after its context was cancelled")
	}
}

func TestPrunerDeletesOnEveryTickAndStopsWhenCancelled(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, ts := range []int64{100, 200, 5000} {
		if err := s.InsertFrame(ts, []byte{1}); err != nil {
			t.Fatal(err)
		}
	}

	now := func() time.Time { return time.Unix(5000, 0) }
	policy := func() Policy { return Policy{Window: 1000 * time.Second, KeptJobs: 0} }

	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() {
		RunPruner(ctx, s, policy, time.Millisecond, now)
		close(stopped)
	}()

	waitFor(t, "the frames outside the window being deleted", func() bool {
		oldest, _, err := s.Range()
		return err == nil && oldest != nil && *oldest == 5000
	})

	cancel()
	requireStopped(t, stopped)
}

// The policy is read on every tick rather than captured once, so that changing
// the retention on the settings page takes effect without a restart.
func TestPrunerRereadsThePolicyEachTick(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, ts := range []int64{1000, 3000, 5000} {
		if err := s.InsertFrame(ts, []byte{1}); err != nil {
			t.Fatal(err)
		}
	}

	var mu sync.Mutex
	window := 10000 * time.Second
	policy := func() Policy {
		mu.Lock()
		defer mu.Unlock()
		return Policy{Window: window, KeptJobs: 0}
	}
	now := func() time.Time { return time.Unix(5000, 0) }

	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	// Registered after the store's own cleanup so it runs first: the loop has
	// to be gone before the database it prunes is closed under it.
	defer func() {
		cancel()
		requireStopped(t, stopped)
	}()
	go func() {
		RunPruner(ctx, s, policy, time.Millisecond, now)
		close(stopped)
	}()

	// Nothing should go while the window covers everything.
	time.Sleep(20 * time.Millisecond)
	if oldest, _, err := s.Range(); err != nil || oldest == nil || *oldest != 1000 {
		t.Fatalf("oldest frame = %v (err %v), want the wide window to have kept 1000", oldest, err)
	}

	mu.Lock()
	window = 2500 * time.Second
	mu.Unlock()

	waitFor(t, "the narrowed window being applied", func() bool {
		oldest, _, err := s.Range()
		return err == nil && oldest != nil && *oldest == 3000
	})
}

func TestJobWatcherPollsWhatItIsGivenAndStopsWhenCancelled(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	var mu sync.Mutex
	state, name := "IDLE", ""
	snapshot := func() (string, string) {
		mu.Lock()
		defer mu.Unlock()
		return state, name
	}

	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() {
		NewJobWatcher(s).Run(ctx, time.Millisecond, snapshot)
		close(stopped)
	}()

	// Idle is not a print, so nothing should be recorded yet.
	time.Sleep(20 * time.Millisecond)
	if job, err := s.ActiveJob(); err != nil || job != nil {
		t.Fatalf("active job = %v (err %v), want none while idle", job, err)
	}

	mu.Lock()
	state, name = "RUNNING", "benchy.gcode"
	mu.Unlock()

	waitFor(t, "the started print being recorded", func() bool {
		job, err := s.ActiveJob()
		return err == nil && job != nil && job.Name == "benchy.gcode"
	})

	cancel()
	requireStopped(t, stopped)
}
