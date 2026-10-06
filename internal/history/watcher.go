package history

import (
	"context"
	"log"
	"time"

	"github.com/brhelwig/bambu-util/internal/p1s"
)

// JobWatcher opens and closes job rows in a Store based on gcode_state
// transitions, so recorded frames can be grouped and played back per print.
type JobWatcher struct {
	store    *Store
	now      func() time.Time
	openID   int64
	openName string
	inJob    bool
}

// NewJobWatcher creates a watcher writing job rows to store. It adopts a row
// left open by an earlier process, so a restart mid-print doesn't record the
// print twice, and closes any older open rows.
func NewJobWatcher(store *Store) *JobWatcher {
	w := &JobWatcher{store: store, now: time.Now}
	if n, err := store.CloseOrphanJobs(); err != nil {
		log.Printf("history: close orphan jobs: %v", err)
	} else if n > 0 {
		log.Printf("history: closed %d job row(s) left open by an earlier process", n)
	}
	open, err := store.ActiveJob()
	if err != nil {
		log.Printf("history: adopt open job: %v", err)
		return w
	}
	if open != nil {
		w.openID = open.ID
		w.openName = open.Name
		w.inJob = true
	}
	return w
}

// Poll opens a job row when a print starts and closes it when it ends.
// Repeated calls with the same state are no-ops. States that are neither
// running nor ended ("unknown", PREPARE) leave the row alone.
func (w *JobWatcher) Poll(gcodeState, jobName string) {
	switch {
	case p1s.JobActive(gcodeState):
		// A different name means a job boundary was missed (e.g. while the app
		// was down), so start a new row. An empty name is just a partial report.
		if w.inJob && jobName != "" && jobName != w.openName {
			w.closeOpen()
		}
		if !w.inJob {
			w.openFor(jobName)
		}
	case p1s.JobEnded(gcodeState) && w.inJob:
		w.closeOpen()
	}
}

func (w *JobWatcher) openFor(jobName string) {
	id, err := w.store.OpenJob(jobName, w.now().Unix())
	if err != nil {
		log.Printf("history: open job: %v", err)
		return
	}
	w.openID = id
	w.openName = jobName
	w.inJob = true
}

func (w *JobWatcher) closeOpen() {
	if err := w.store.CloseJobAtLastFrame(w.openID, w.now().Unix()); err != nil {
		// Left open on purpose: the next poll in the same state retries.
		log.Printf("history: close job: %v", err)
		return
	}
	w.inJob = false
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
