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
	store  *Store
	now    func() time.Time
	openID int64
	inJob  bool
}

// NewJobWatcher creates a watcher writing job rows to store. It adopts a row
// left open by an earlier process instead of opening a second one: whether a
// print is being recorded lives only in this struct, so a restart mid-print
// would otherwise list that print twice and leave the first row open forever.
// Rows already stranded that way are closed on the way past.
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
		w.inJob = true
	}
	return w
}

// Poll opens a job row when a print starts and closes the open one when it
// ends. Repeated calls with the same state are no-ops, so it's safe to call on
// every status poll. States that mean neither — "unknown" before the printer's
// first report, or PREPARE while it heats — leave the row alone; closing on
// those would end a print that is still running.
func (w *JobWatcher) Poll(gcodeState, jobName string) {
	switch {
	case p1s.JobActive(gcodeState) && !w.inJob:
		id, err := w.store.OpenJob(jobName, w.now().Unix())
		if err != nil {
			log.Printf("history: open job: %v", err)
			return
		}
		w.openID = id
		w.inJob = true
	case p1s.JobEnded(gcodeState) && w.inJob:
		if err := w.store.CloseJobAtLastFrame(w.openID, w.now().Unix()); err != nil {
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
