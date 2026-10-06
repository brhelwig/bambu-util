// Package capacity caps the database file size by deleting the oldest data
// across all sources (camera frames, event log), overriding their own
// retention. Freed pages are returned to the disk with incremental vacuum, a
// chunk at a time so the database isn't locked for long.
package capacity

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"sort"
	"time"
)

// Item is one deletable row, with its age and size.
type Item struct {
	ID    int64
	When  int64 // unix milliseconds, so sources of different precision compare
	Bytes int64
}

// Source is one kind of data the cap may delete from.
type Source interface {
	// Name is what this source is called when something is logged about it.
	Name() string
	// Oldest returns at most n items, oldest first.
	Oldest(n int) ([]Item, error)
	// DeleteThrough removes every item up to and including id.
	DeleteThrough(id int64) error
}

const (
	// lowWater is how far under the limit a pass cuts.
	lowWater = 0.9

	// reclaimChunk is how many pages one incremental vacuum returns, kept small
	// so the write lock is held briefly.
	reclaimChunk = 256

	// batch is how many items are read from each source in one round.
	batch = 2000

	// maxRounds bounds one pass; a database far over its limit comes down over
	// several ticks.
	maxRounds = 8
)

// Enforcer holds one database to a limit.
type Enforcer struct {
	db      *sql.DB
	sources []Source
	limit   func() int64 // bytes; zero means no limit
}

// New returns an enforcer over db. limit is read on each pass.
func New(db *sql.DB, limit func() int64, sources ...Source) *Enforcer {
	return &Enforcer{db: db, sources: sources, limit: limit}
}

// Run enforces the limit on every tick of interval until ctx is cancelled.
// Call once, from main.
func Run(ctx context.Context, e *Enforcer, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := e.Once(); err != nil {
				log.Printf("capacity: %v", err)
			}
		}
	}
}

// Once runs a single pass.
func (e *Enforcer) Once() error {
	limit := e.limit()
	if limit <= 0 {
		return nil // switched off: nothing is deleted and nothing is compacted
	}
	size, err := e.size()
	if err != nil {
		return err
	}
	if size <= limit {
		return nil
	}
	// Convert lazily, only once the cap is actually exceeded.
	if err := e.convert(); err != nil {
		return err
	}

	target := int64(float64(limit) * lowWater)
	for round := 0; round < maxRounds; round++ {
		size, err = e.size()
		if err != nil {
			return err
		}
		if size <= target {
			return nil
		}
		deleted, err := e.deleteOldest(size - target)
		if err != nil {
			return err
		}
		if err := e.reclaim(); err != nil {
			return err
		}
		if deleted == 0 {
			// Nothing left to delete.
			after, err := e.size()
			if err != nil {
				return err
			}
			if after > limit {
				log.Printf("capacity: database is %d MB against a %d MB limit and there is nothing left to delete",
					after>>20, limit>>20)
			}
			return nil
		}
	}
	return nil
}

// size is the main database file's size (not counting the WAL).
func (e *Enforcer) size() (int64, error) {
	var pages, pageSize int64
	if err := e.db.QueryRow(`PRAGMA page_count`).Scan(&pages); err != nil {
		return 0, fmt.Errorf("page count: %w", err)
	}
	if err := e.db.QueryRow(`PRAGMA page_size`).Scan(&pageSize); err != nil {
		return 0, fmt.Errorf("page size: %w", err)
	}
	return pages * pageSize, nil
}

// convert switches an older database to incremental auto-vacuum, which takes
// one full VACUUM.
func (e *Enforcer) convert() error {
	var mode int
	if err := e.db.QueryRow(`PRAGMA auto_vacuum`).Scan(&mode); err != nil {
		return fmt.Errorf("read auto-vacuum mode: %w", err)
	}
	if mode == 2 {
		return nil
	}
	log.Print("capacity: converting the database so space can be returned to the disk; this may take a while")
	started := time.Now()
	if _, err := e.db.Exec(`PRAGMA auto_vacuum = INCREMENTAL`); err != nil {
		return fmt.Errorf("set auto-vacuum mode: %w", err)
	}
	if _, err := e.db.Exec(`VACUUM`); err != nil {
		return fmt.Errorf("convert database: %w", err)
	}
	log.Printf("capacity: converted in %s", time.Since(started).Round(time.Millisecond))
	return nil
}

// deleteOldest removes the oldest items across all sources until want bytes
// are freed, and reports how many it deleted.
func (e *Enforcer) deleteOldest(want int64) (int, error) {
	type candidate struct {
		Item
		source int
	}
	var all []candidate
	for i, s := range e.sources {
		items, err := s.Oldest(batch)
		if err != nil {
			return 0, fmt.Errorf("%s: %w", s.Name(), err)
		}
		for _, item := range items {
			all = append(all, candidate{Item: item, source: i})
		}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].When < all[j].When })

	// One cut-off per source: the last of its items the walk reached.
	through := make(map[int]int64, len(e.sources))
	var freed int64
	var count int
	for _, c := range all {
		if freed >= want {
			break
		}
		through[c.source] = c.ID
		freed += c.Bytes
		count++
	}
	for i, id := range through {
		if err := e.sources[i].DeleteThrough(id); err != nil {
			return 0, fmt.Errorf("%s: %w", e.sources[i].Name(), err)
		}
	}
	return count, nil
}

// reclaim returns freed pages to the disk a chunk at a time, stopping when the
// free list is empty.
func (e *Enforcer) reclaim() error {
	for {
		var free int64
		if err := e.db.QueryRow(`PRAGMA freelist_count`).Scan(&free); err != nil {
			return fmt.Errorf("free list: %w", err)
		}
		if free == 0 {
			return nil
		}
		if _, err := e.db.Exec(fmt.Sprintf(`PRAGMA incremental_vacuum(%d)`, reclaimChunk)); err != nil {
			return fmt.Errorf("return pages: %w", err)
		}
	}
}
