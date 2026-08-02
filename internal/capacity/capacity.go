// Package capacity holds the database file to a size, by deleting the oldest
// data — camera frames and event-log entries alike, whichever is older — until
// the file is back under it.
//
// It is a backstop rather than a retention rule. The camera window and the
// event log's own budget decide what is worth keeping; this decides what the
// disk can actually hold, and it overrules them, because a full disk takes the
// whole app down with it.
//
// Deleting rows does not shrink a SQLite file on its own: the pages go on a
// free list and the file stays the size it was. Returning them to the disk is a
// separate step, done here a few pages at a time so the database is not locked
// for one long stretch.
package capacity

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"sort"
	"time"
)

// Item is one thing the cap may delete: what it is worth deleting, and when it
// happened, so the oldest across every source can be found.
type Item struct {
	ID    int64
	When  int64 // unix milliseconds, so sources of different precision compare
	Bytes int64
}

// Source is one kind of data the cap may delete from. Each source knows its own
// table; nothing here does.
type Source interface {
	// Name is what this source is called when something is logged about it.
	Name() string
	// Oldest returns at most n items, oldest first.
	Oldest(n int) ([]Item, error)
	// DeleteThrough removes every item up to and including id.
	DeleteThrough(id int64) error
}

const (
	// lowWater is how far under the limit a pass cuts, so a file sitting right
	// on its limit does not delete and compact on every single tick.
	lowWater = 0.9

	// reclaimChunk is how many pages one incremental vacuum returns. Small
	// enough that the write lock is held briefly and repeatedly rather than
	// once for a long time, which is what stops a big reclaim stalling the
	// camera and the page.
	reclaimChunk = 256

	// batch is how many items are read from each source in one round.
	batch = 2000

	// maxRounds bounds the work one pass will do. A database far over its limit
	// comes down over several ticks instead of holding everything up while it
	// finishes.
	maxRounds = 8
)

// Enforcer holds one database to a limit.
type Enforcer struct {
	db      *sql.DB
	sources []Source
	limit   func() int64 // bytes; zero means no limit
}

// New returns an enforcer over db. The limit is read at each pass rather than
// held, so changing it on the settings page takes effect without a restart.
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

// Once runs a single pass. It reports how many bytes the file came down by.
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
	// Only a database that is actually over its limit pays for the conversion,
	// so anyone who never turns the cap on never waits for it.
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
			// Nothing left that may be deleted. Reclaiming has already returned
			// whatever it could; saying so beats looping in silence.
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

// size is the database file's size. The write-ahead log is a separate file of a
// few megabytes that SQLite folds back on its own, and is not counted.
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

// convert puts a database made before the cap existed into incremental
// auto-vacuum mode, without which no space is ever returned to the disk. It
// takes one whole-file rebuild, so it happens once and only for a database that
// needs it.
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

// deleteOldest removes the oldest items across every source until it has freed
// want bytes, and reports how many items it deleted. Which source an item comes
// from does not matter — only how old it is.
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
