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

// RunPruner deletes frames (and fully-expired job rows) that the policy no
// longer covers, on every tick of interval, until ctx is cancelled. The policy
// is read each tick rather than captured, so changing it in the settings takes
// effect without a restart. Call once, from main.
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
