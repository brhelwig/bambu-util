package history

import (
	"context"
	"log"
	"time"
)

// Policy is the retention rule in force right now: how far back frames are
// kept, and how many finished prints keep theirs regardless.
type Policy struct {
	Window   time.Duration
	KeptJobs int
}

// RunPruner applies the retention policy, read fresh each tick, on every tick
// of interval until ctx is cancelled.
func RunPruner(ctx context.Context, store *Store, policy func() Policy, interval time.Duration, now func() time.Time) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p := policy()
			cutoff := now().Add(-p.Window).Unix()
			if err := store.Prune(cutoff, p.KeptJobs); err != nil {
				log.Printf("history: prune: %v", err)
			}
		}
	}
}
