package history

import (
	"context"
	"log"
	"time"
)

// JobWatcher opens and closes job rows in a Store based on gcode_state
// transitions, so recorded frames can be grouped and played back per print.
type JobWatcher struct {
	store  *Store
	now    func() time.Time
	openID int64
	inJob  bool
}

// NewJobWatcher creates a watcher writing job rows to store.
func NewJobWatcher(store *Store) *JobWatcher {
	return &JobWatcher{store: store, now: time.Now}
}

// Poll opens a job row on a transition into "RUNNING", and closes the open
// one on a transition out of it. Repeated calls with the same state are
// no-ops, so it's safe to call on every status poll.
func (w *JobWatcher) Poll(gcodeState, jobName string) {
	running := gcodeState == "RUNNING"
	switch {
	case running && !w.inJob:
		id, err := w.store.OpenJob(jobName, w.now().Unix())
		if err != nil {
			log.Printf("history: open job: %v", err)
			return
		}
		w.openID = id
		w.inJob = true
	case !running && w.inJob:
		if err := w.store.CloseJob(w.openID, w.now().Unix()); err != nil {
			log.Printf("history: close job: %v", err)
			return
		}
		w.inJob = false
	}
}

// Run calls snapshot and Polls its result on every tick of interval, until
// ctx is cancelled.
func (w *JobWatcher) Run(ctx context.Context, interval time.Duration, snapshot func() (gcodeState, jobName string)) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			gs, name := snapshot()
			w.Poll(gs, name)
		}
	}
}
