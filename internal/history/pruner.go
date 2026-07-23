package history

import (
	"context"
	"log"
	"time"
)

// RunPruner deletes frames (and fully-expired job rows) older than
// retention, on every tick of interval, until ctx is cancelled. Call once,
// from main.
func RunPruner(ctx context.Context, store *Store, retention, interval time.Duration, now func() time.Time) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			cutoff := now().Add(-retention).Unix()
			if err := store.Prune(cutoff); err != nil {
				log.Printf("history: prune: %v", err)
			}
		}
	}
}
