# Camera History Recording Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Record the printer's chamber camera continuously into a rolling SQLite-backed buffer, so the app can scrub back through recent footage (DVR-style) and jump to any print job's footage as a fast-forwarded timelapse.

**Architecture:** `Hub` (in `internal/web`) stops connecting to the camera on demand and instead connects once at process startup, staying connected for the app's lifetime; every frame is both broadcast to live viewers (as today) and written to a new `internal/history.Store` (SQLite via the pure-Go `modernc.org/sqlite` driver, required because the Docker build uses `CGO_ENABLED=0`). A `history.JobWatcher` polls the existing `p1s.StateCache` for `gcode_state` transitions to tag frames with print jobs. A pruner loop deletes anything older than a configurable retention window. Three new HTTP endpoints expose the buffer to a new History section in the existing single-page UI, which scrubs by repeatedly fetching the nearest frame at a chosen timestamp — no video encoding, no new streaming protocol.

**Tech Stack:** Go 1.26, `modernc.org/sqlite` (pure-Go SQLite driver), existing `net/http` + vanilla JS frontend.

**Spec:** `docs/superpowers/specs/2026-07-23-camera-history-recording-design.md` / https://github.com/brhelwig/bambu-util/issues/20

---

## File Structure

- `internal/history/store.go` — new. `Store` type: SQLite-backed frame + job storage (`Open`, `Close`, `InsertFrame`, `FrameAtOrAfter`, `Range`, `Prune`, `OpenJob`, `CloseJob`, `RecentJobs`, `Job` struct, `ErrNoFrame`).
- `internal/history/store_test.go` — new.
- `internal/history/watcher.go` — new. `JobWatcher`: opens/closes job rows on `gcode_state` transitions.
- `internal/history/watcher_test.go` — new.
- `internal/history/pruner.go` — new. `RunPruner`: ticker loop calling `Store.Prune` (thin glue, matches `Server.EnforceAutoOff` — not directly unit tested, same as that function).
- `internal/web/hub.go` — modified. `Hub` no longer connects on demand; add `FrameSink` interface, `Start`, always-on broadcast that also writes to the store.
- `internal/web/hub_test.go` — modified. Replace the on-demand-connect test with tests for always-on recording/broadcast.
- `internal/web/server.go` — modified. `NewServer` takes a `*history.Store`; add three history endpoints.
- `internal/web/server_test.go` — modified. Test helper gains a store; new tests for the history endpoints.
- `internal/web/static/index.html` — modified. New History card: scrub bar, play/pause, speed, recent jobs list.
- `cmd/bambu-util/main.go` — modified. New `DATA_DIR` / `RECORDING_RETENTION` env vars; wire up the store, `Hub.Start`, pruner, and job watcher.
- `go.mod`, `go.sum` — modified. Add `modernc.org/sqlite`.
- `README.md` — modified. Document the new config vars, the History feature, and the Bambu Studio camera-exclusivity tradeoff.
- `CHANGELOG.md` — modified. New `Unreleased` entry.

---

### Task 1: Add the SQLite dependency

**Files:**
- Modify: `go.mod`, `go.sum`

- [ ] **Step 1: Add the dependency**

Run: `go get modernc.org/sqlite`

- [ ] **Step 2: Verify it cross-compiles the way the Dockerfile does (CGO_ENABLED=0, arm64)**

Run: `CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o /tmp/bu-arm64-check ./cmd/bambu-util && rm /tmp/bu-arm64-check && echo OK`
Expected: `OK` (this works today even before `internal/history` exists, since nothing imports the new dependency yet — if `go build` complains the import isn't used anywhere, that's fine, this step is just confirming the module resolves and cross-compiles; skip build verification if `go vet ./...` already passes)

- [ ] **Step 3: Commit**

```bash
git add go.mod go.sum
git commit -m "build: add modernc.org/sqlite dependency"
```

---

### Task 2: `history.Store` — schema, frame insert, and nearest-frame lookup

**Files:**
- Create: `internal/history/store.go`
- Create: `internal/history/store_test.go`

- [ ] **Step 1: Write the failing tests**

```go
// internal/history/store_test.go
package history

import (
	"errors"
	"testing"
)

func TestInsertAndFrameAtOrAfter(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if err := s.InsertFrame(100, []byte{1}); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertFrame(200, []byte{2}); err != nil {
		t.Fatal(err)
	}

	jpeg, ts, err := s.FrameAtOrAfter(150)
	if err != nil {
		t.Fatal(err)
	}
	if ts != 200 || len(jpeg) != 1 || jpeg[0] != 2 {
		t.Fatalf("got ts=%d jpeg=%v, want ts=200 jpeg=[2]", ts, jpeg)
	}
}

func TestFrameAtOrAfterExactMatch(t *testing.T) {
	s, _ := Open(":memory:")
	defer s.Close()
	s.InsertFrame(100, []byte{1})

	_, ts, err := s.FrameAtOrAfter(100)
	if err != nil {
		t.Fatal(err)
	}
	if ts != 100 {
		t.Fatalf("ts = %d, want 100", ts)
	}
}

func TestFrameAtOrAfterNoneFound(t *testing.T) {
	s, _ := Open(":memory:")
	defer s.Close()
	s.InsertFrame(100, []byte{1})

	if _, _, err := s.FrameAtOrAfter(200); !errors.Is(err, ErrNoFrame) {
		t.Fatalf("got %v, want ErrNoFrame", err)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/history/... -run TestInsert -v`
Expected: FAIL — `package history: no Go files` (package doesn't exist yet)

- [ ] **Step 3: Write the implementation**

```go
// internal/history/store.go

// Package history stores recorded camera frames and print-job boundaries in
// SQLite, so the app can serve a scrollback buffer and per-job timelapses.
package history

import (
	"database/sql"
	"errors"

	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS frames (
  id  INTEGER PRIMARY KEY,
  ts  INTEGER NOT NULL,
  jpeg BLOB NOT NULL
);
CREATE INDEX IF NOT EXISTS frames_ts ON frames(ts);

CREATE TABLE IF NOT EXISTS jobs (
  id       INTEGER PRIMARY KEY,
  name     TEXT NOT NULL,
  start_ts INTEGER NOT NULL,
  end_ts   INTEGER
);
`

// Store persists camera frames and job boundaries in a SQLite database.
type Store struct {
	db *sql.DB
}

// Open opens (creating if needed) the SQLite database at path. Use
// ":memory:" for a throwaway in-process database, e.g. in tests.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	// modernc.org/sqlite serializes writes at the connection level; capping
	// the pool at one connection avoids "database is locked" errors under
	// concurrent access and gives :memory: a single, consistent database
	// instead of a fresh one per connection.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

// Close closes the underlying database.
func (s *Store) Close() error {
	return s.db.Close()
}

// InsertFrame records one camera frame at the given unix-second timestamp.
func (s *Store) InsertFrame(ts int64, jpeg []byte) error {
	_, err := s.db.Exec(`INSERT INTO frames (ts, jpeg) VALUES (?, ?)`, ts, jpeg)
	return err
}

// ErrNoFrame is returned by FrameAtOrAfter when no frame exists at or after
// the requested timestamp.
var ErrNoFrame = errors.New("history: no frame at or after ts")

// FrameAtOrAfter returns the stored frame with the smallest timestamp that
// is >= ts, along with that timestamp. Returns ErrNoFrame if none exists.
func (s *Store) FrameAtOrAfter(ts int64) (jpeg []byte, gotTs int64, err error) {
	row := s.db.QueryRow(`SELECT ts, jpeg FROM frames WHERE ts >= ? ORDER BY ts ASC LIMIT 1`, ts)
	if err := row.Scan(&gotTs, &jpeg); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, 0, ErrNoFrame
		}
		return nil, 0, err
	}
	return jpeg, gotTs, nil
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/history/... -v`
Expected: PASS (all three tests)

- [ ] **Step 5: Commit**

```bash
git add internal/history/store.go internal/history/store_test.go
git commit -m "feat: SQLite-backed frame store with nearest-timestamp lookup"
```

---

### Task 3: `history.Store` — range and retention pruning

**Files:**
- Modify: `internal/history/store.go`
- Modify: `internal/history/store_test.go`

- [ ] **Step 1: Write the failing tests**

```go
// Add to internal/history/store_test.go

func TestRangeEmpty(t *testing.T) {
	s, _ := Open(":memory:")
	defer s.Close()

	oldest, newest, err := s.Range()
	if err != nil {
		t.Fatal(err)
	}
	if oldest != nil || newest != nil {
		t.Fatalf("got %v..%v, want nil..nil", oldest, newest)
	}
}

func TestRangeWithFrames(t *testing.T) {
	s, _ := Open(":memory:")
	defer s.Close()
	s.InsertFrame(100, []byte{1})
	s.InsertFrame(300, []byte{2})
	s.InsertFrame(200, []byte{3})

	oldest, newest, err := s.Range()
	if err != nil {
		t.Fatal(err)
	}
	if oldest == nil || newest == nil || *oldest != 100 || *newest != 300 {
		t.Fatalf("got %v..%v, want 100..300", oldest, newest)
	}
}

func TestPruneDeletesOldFrames(t *testing.T) {
	s, _ := Open(":memory:")
	defer s.Close()
	s.InsertFrame(100, []byte{1})
	s.InsertFrame(500, []byte{2})

	if err := s.Prune(300); err != nil {
		t.Fatal(err)
	}
	oldest, newest, _ := s.Range()
	if oldest == nil || *oldest != 500 || *newest != 500 {
		t.Fatalf("got %v..%v, want 500..500", oldest, newest)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/history/... -run 'TestRange|TestPrune' -v`
Expected: FAIL with "undefined: (\*Store).Range" / "undefined: (\*Store).Prune"

- [ ] **Step 3: Write the implementation**

```go
// Add to internal/history/store.go

// Range returns the oldest and newest frame timestamps currently stored,
// or nil, nil if the store is empty.
func (s *Store) Range() (oldest, newest *int64, err error) {
	var minTs, maxTs sql.NullInt64
	row := s.db.QueryRow(`SELECT MIN(ts), MAX(ts) FROM frames`)
	if err := row.Scan(&minTs, &maxTs); err != nil {
		return nil, nil, err
	}
	if minTs.Valid {
		v := minTs.Int64
		oldest = &v
	}
	if maxTs.Valid {
		v := maxTs.Int64
		newest = &v
	}
	return oldest, newest, nil
}

// Prune deletes frames older than cutoff, and any job row that finished
// before cutoff. A job with no end yet (still in progress) is never pruned,
// regardless of how old its start is.
func (s *Store) Prune(cutoff int64) error {
	if _, err := s.db.Exec(`DELETE FROM frames WHERE ts < ?`, cutoff); err != nil {
		return err
	}
	_, err := s.db.Exec(`DELETE FROM jobs WHERE end_ts IS NOT NULL AND end_ts < ?`, cutoff)
	return err
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/history/... -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/history/store.go internal/history/store_test.go
git commit -m "feat: buffer range query and retention pruning"
```

---

### Task 4: `history.Store` — print job tracking

**Files:**
- Modify: `internal/history/store.go`
- Modify: `internal/history/store_test.go`

- [ ] **Step 1: Write the failing tests**

```go
// Add to internal/history/store_test.go

func TestJobLifecycle(t *testing.T) {
	s, _ := Open(":memory:")
	defer s.Close()

	id, err := s.OpenJob("benchy.3mf", 100)
	if err != nil {
		t.Fatal(err)
	}
	jobs, err := s.RecentJobs()
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 || jobs[0].Name != "benchy.3mf" || jobs[0].Start != 100 || jobs[0].End != nil {
		t.Fatalf("want 1 open job named benchy.3mf starting at 100, got %+v", jobs)
	}

	if err := s.CloseJob(id, 200); err != nil {
		t.Fatal(err)
	}
	jobs, _ = s.RecentJobs()
	if len(jobs) != 1 || jobs[0].End == nil || *jobs[0].End != 200 {
		t.Fatalf("want closed job ending at 200, got %+v", jobs)
	}
}

func TestPrunePreservesOngoingJobRegardlessOfStartAge(t *testing.T) {
	s, _ := Open(":memory:")
	defer s.Close()
	s.OpenJob("old-but-running.3mf", 0)

	if err := s.Prune(1000); err != nil {
		t.Fatal(err)
	}
	jobs, _ := s.RecentJobs()
	if len(jobs) != 1 {
		t.Fatalf("ongoing job was pruned: %+v", jobs)
	}
}

func TestPruneDeletesExpiredFinishedJobs(t *testing.T) {
	s, _ := Open(":memory:")
	defer s.Close()
	id, _ := s.OpenJob("old.3mf", 0)
	s.CloseJob(id, 50)

	if err := s.Prune(1000); err != nil {
		t.Fatal(err)
	}
	jobs, _ := s.RecentJobs()
	if len(jobs) != 0 {
		t.Fatalf("expired finished job was not pruned: %+v", jobs)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/history/... -run 'TestJobLifecycle|TestPrune' -v`
Expected: FAIL with "undefined: (\*Store).OpenJob" etc.

- [ ] **Step 3: Write the implementation**

```go
// Add to internal/history/store.go

// Job is one print job's recorded time range. End is nil while the job is
// still in progress.
type Job struct {
	ID    int64
	Name  string
	Start int64
	End   *int64
}

// OpenJob records the start of a print job and returns its id.
func (s *Store) OpenJob(name string, startTs int64) (int64, error) {
	res, err := s.db.Exec(`INSERT INTO jobs (name, start_ts, end_ts) VALUES (?, ?, NULL)`, name, startTs)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// CloseJob records the end of a print job.
func (s *Store) CloseJob(id, endTs int64) error {
	_, err := s.db.Exec(`UPDATE jobs SET end_ts = ? WHERE id = ?`, endTs, id)
	return err
}

// RecentJobs returns every job row currently stored, newest-started first.
// Prune keeps this bounded to jobs whose frames could still exist.
func (s *Store) RecentJobs() ([]Job, error) {
	rows, err := s.db.Query(`SELECT id, name, start_ts, end_ts FROM jobs ORDER BY start_ts DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var jobs []Job
	for rows.Next() {
		var j Job
		var end sql.NullInt64
		if err := rows.Scan(&j.ID, &j.Name, &j.Start, &end); err != nil {
			return nil, err
		}
		if end.Valid {
			v := end.Int64
			j.End = &v
		}
		jobs = append(jobs, j)
	}
	return jobs, rows.Err()
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/history/... -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/history/store.go internal/history/store_test.go
git commit -m "feat: track print jobs for per-job timelapse lookup"
```

---

### Task 5: `history.JobWatcher` — detect job boundaries from printer state

**Files:**
- Create: `internal/history/watcher.go`
- Create: `internal/history/watcher_test.go`

- [ ] **Step 1: Write the failing tests**

```go
// internal/history/watcher_test.go
package history

import "testing"

func TestJobWatcherOpensAndClosesOnTransition(t *testing.T) {
	s, _ := Open(":memory:")
	defer s.Close()
	w := NewJobWatcher(s)

	w.Poll("IDLE", "")
	if jobs, _ := s.RecentJobs(); len(jobs) != 0 {
		t.Fatalf("job opened while idle: %+v", jobs)
	}

	w.Poll("RUNNING", "benchy.3mf")
	jobs, _ := s.RecentJobs()
	if len(jobs) != 1 || jobs[0].Name != "benchy.3mf" || jobs[0].End != nil {
		t.Fatalf("want 1 open job named benchy.3mf, got %+v", jobs)
	}

	w.Poll("FINISH", "benchy.3mf")
	jobs, _ = s.RecentJobs()
	if len(jobs) != 1 || jobs[0].End == nil {
		t.Fatalf("want closed job after leaving RUNNING, got %+v", jobs)
	}
}

func TestJobWatcherIgnoresRepeatedRunningPolls(t *testing.T) {
	s, _ := Open(":memory:")
	defer s.Close()
	w := NewJobWatcher(s)

	w.Poll("RUNNING", "a.3mf")
	w.Poll("RUNNING", "a.3mf")
	w.Poll("RUNNING", "a.3mf")

	jobs, _ := s.RecentJobs()
	if len(jobs) != 1 {
		t.Fatalf("want exactly 1 job opened across repeated RUNNING polls, got %d", len(jobs))
	}
}

func TestJobWatcherIgnoresRepeatedNonRunningPolls(t *testing.T) {
	s, _ := Open(":memory:")
	defer s.Close()
	w := NewJobWatcher(s)

	w.Poll("IDLE", "")
	w.Poll("IDLE", "")
	jobs, _ := s.RecentJobs()
	if len(jobs) != 0 {
		t.Fatalf("want no jobs, got %+v", jobs)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/history/... -run TestJobWatcher -v`
Expected: FAIL with "undefined: NewJobWatcher"

- [ ] **Step 3: Write the implementation**

```go
// internal/history/watcher.go
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
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/history/... -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/history/watcher.go internal/history/watcher_test.go
git commit -m "feat: detect print job boundaries from gcode_state"
```

---

### Task 6: `history.RunPruner` — periodic retention enforcement

**Files:**
- Create: `internal/history/pruner.go`

This is thin ticker glue around the already-tested `Store.Prune`, the same shape as `Server.EnforceAutoOff` in `internal/web/autooff.go` — which also has no direct test, only its underlying `autoOff.due()` logic does. No test file for this one, for the same reason.

- [ ] **Step 1: Write the implementation**

```go
// internal/history/pruner.go
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
```

- [ ] **Step 2: Verify the package still builds and all history tests still pass**

Run: `go build ./internal/history/... && go test ./internal/history/... -v`
Expected: build succeeds, all tests PASS

- [ ] **Step 3: Commit**

```bash
git add internal/history/pruner.go
git commit -m "feat: periodic retention pruning loop"
```

---

### Task 7: `web.Hub` — always-on camera connection and recording

**Files:**
- Modify: `internal/web/hub.go`
- Modify: `internal/web/hub_test.go`

This replaces Hub's on-demand connect/disconnect (which existed so Bambu Studio could use the camera whenever nobody was viewing the page) with a connection that's held for the life of the process — the accepted tradeoff from the spec. Every frame is now also written to a `FrameSink` (the history store), whether or not any viewer is attached.

- [ ] **Step 1: Write the failing tests**

```go
// internal/web/hub_test.go
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

func TestHubBroadcastsToAttachedViewers(t *testing.T) {
	src := &fakeSource{}
	h := NewHub(src.run, &fakeSink{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.Start(ctx)

	ch, detach := h.Attach()
	defer detach()
	select {
	case f := <-ch:
		if len(f) == 0 {
			t.Fatal("empty frame delivered")
		}
	case <-time.After(time.Second):
		t.Fatal("no frame delivered to viewer")
	}
}

func TestHubRecordsFramesWithNoViewerAttached(t *testing.T) {
	src := &fakeSource{}
	sink := &fakeSink{}
	h := NewHub(src.run, sink)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.Start(ctx)

	deadline := time.Now().Add(time.Second)
	for sink.count() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("no frame recorded despite no viewer ever attaching")
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
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/web/... -run TestHub -v`
Expected: FAIL to compile — `NewHub` signature mismatch (old signature takes one arg), `h.Start` undefined

- [ ] **Step 3: Write the implementation**

```go
// internal/web/hub.go
// Package web serves the phone-facing HTTP interface.
package web

import (
	"context"
	"log"
	"sync"
	"time"
)

// CameraSource connects to the printer camera and passes frames to yield
// until ctx is cancelled.
type CameraSource func(ctx context.Context, yield func([]byte)) error

// FrameSink is what the hub needs to persist a frame for history playback.
type FrameSink interface {
	InsertFrame(ts int64, jpeg []byte) error
}

// Default retry backoff bounds for Start. Overridable per-Hub (tests set
// smaller values so backoff growth doesn't slow the suite down).
const (
	defaultMinRetryDelay = 1 * time.Second
	defaultMaxRetryDelay = 30 * time.Second
)

// Hub connects to the printer camera once, at Start, and holds that
// connection for the process's lifetime, fanning each frame out to live
// viewers and into the history store. Earlier versions only held the camera
// while at least one viewer was attached, so Bambu Studio could use it the
// rest of the time — always-on recording means that's no longer true; the
// printer's camera port serves one client, so Bambu Studio's live view will
// not work while this app is running. Accepted tradeoff, see the design
// spec.
type Hub struct {
	source                       CameraSource
	store                        FrameSink
	now                          func() time.Time
	minRetryDelay, maxRetryDelay time.Duration
	mu                           sync.Mutex
	viewers                      map[chan []byte]struct{}
}

// NewHub creates a Hub. Call Start once to begin the camera connection.
func NewHub(source CameraSource, store FrameSink) *Hub {
	return &Hub{
		source:        source,
		store:         store,
		now:           time.Now,
		minRetryDelay: defaultMinRetryDelay,
		maxRetryDelay: defaultMaxRetryDelay,
		viewers:       map[chan []byte]struct{}{},
	}
}

// Attach registers a viewer. The channel receives JPEG frames; call detach
// when the viewer leaves. Does not affect the camera connection, which runs
// independently once Start has been called.
func (h *Hub) Attach() (frames <-chan []byte, detach func()) {
	ch := make(chan []byte, 1)
	h.mu.Lock()
	h.viewers[ch] = struct{}{}
	h.mu.Unlock()
	return ch, func() {
		h.mu.Lock()
		delete(h.viewers, ch)
		h.mu.Unlock()
	}
}

// Start connects to the camera and keeps it connected — retrying on
// failure with exponential backoff — until ctx is cancelled. Call once,
// from main.
func (h *Hub) Start(ctx context.Context) {
	delay := h.minRetryDelay
	for ctx.Err() == nil {
		gotFrame := false
		err := h.source(ctx, func(frame []byte) {
			gotFrame = true
			h.broadcast(frame)
		})
		if ctx.Err() != nil {
			return
		}
		if gotFrame {
			// The connection was live for a while before dropping — a
			// transient blip, not a struggling printer/network. Don't
			// carry a long backoff into the next attempt.
			delay = h.minRetryDelay
		}
		log.Printf("camera stream ended, retrying in %s: %v", delay, err)
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return
		}
		delay = h.nextBackoff(delay)
	}
}

// nextBackoff doubles prev, capped at maxRetryDelay.
func (h *Hub) nextBackoff(prev time.Duration) time.Duration {
	next := prev * 2
	if next > h.maxRetryDelay || next <= 0 { // next<=0 guards against overflow
		return h.maxRetryDelay
	}
	return next
}

func (h *Hub) broadcast(frame []byte) {
	if err := h.store.InsertFrame(h.now().Unix(), frame); err != nil {
		log.Printf("history: insert frame: %v", err)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.viewers {
		select {
		case ch <- frame:
		default: // viewer still writing the previous frame — drop this one
		}
	}
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/web/... -run TestHub -v`
Expected: PASS (note: the rest of `internal/web` will not compile yet — `server.go`/`server_test.go` still call the old `NewHub`/`NewServer` signatures. That's fixed in Task 8.)

- [ ] **Step 5: Commit**

```bash
git add internal/web/hub.go internal/web/hub_test.go
git commit -m "feat: hold the camera connection continuously and record every frame"
```

---

### Task 8: `web.Server` — wire in the store and add history endpoints

**Files:**
- Modify: `internal/web/server.go`
- Modify: `internal/web/server_test.go`

- [ ] **Step 1: Update `NewServer` and the test helper so the existing suite compiles again**

In `internal/web/server.go`, update the imports and `Server`/`NewServer`:

```go
import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"strconv"
	"time"

	"github.com/brhelwig/bambu-util/internal/history"
	"github.com/brhelwig/bambu-util/internal/p1s"
)
```

```go
type Server struct {
	cache   *p1s.StateCache
	cmd     Commander
	hub     *Hub
	store   *history.Store
	autoOff *autoOff
}

func NewServer(cache *p1s.StateCache, cmd Commander, hub *Hub, store *history.Store) *Server {
	return &Server{cache: cache, cmd: cmd, hub: hub, store: store, autoOff: newAutoOff()}
}
```

In `internal/web/server_test.go`, add the import and replace `newTestServer` with a shared builder plus a thin wrapper, so all existing call sites (`newTestServer(connected, state)`) keep compiling unchanged:

```go
import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/brhelwig/bambu-util/internal/history"
	"github.com/brhelwig/bambu-util/internal/p1s"
)
```

```go
func buildTestServer(connected bool, state string) (*httptest.Server, *fakeCommander, *history.Store) {
	cache := p1s.NewStateCache()
	cache.SetConnected(connected)
	if state != "" {
		cache.Merge(map[string]any{"gcode_state": state, "bed_temper": 20.5})
	}
	cmd := &fakeCommander{}
	store, err := history.Open(":memory:")
	if err != nil {
		panic(err)
	}
	hub := NewHub(func(ctx context.Context, yield func([]byte)) error {
		yield([]byte{0xFF, 0xD8, 0xFF, 0xD9})
		<-ctx.Done()
		return ctx.Err()
	}, store)
	return httptest.NewServer(NewServer(cache, cmd, hub, store).Handler()), cmd, store
}

func newTestServer(connected bool, state string) (*httptest.Server, *fakeCommander) {
	ts, cmd, _ := buildTestServer(connected, state)
	return ts, cmd
}
```

Run: `go build ./... && go test ./internal/web/... -v`
Expected: build succeeds; every pre-existing test still PASSes (none of their behavior changed, only how the test server is constructed)

- [ ] **Step 2: Commit the compile fix on its own**

```bash
git add internal/web/server.go internal/web/server_test.go
git commit -m "refactor: thread the history store through the server constructor"
```

- [ ] **Step 3: Write the failing tests for the new endpoints**

```go
// Add to internal/web/server_test.go, alongside the other imports add "bytes" and "io"

func TestHistoryRangeEmpty(t *testing.T) {
	ts, _, _ := buildTestServer(true, "IDLE")
	defer ts.Close()

	resp, err := ts.Client().Get(ts.URL + "/camera/history/range")
	if err != nil {
		t.Fatal(err)
	}
	var r map[string]any
	json.NewDecoder(resp.Body).Decode(&r)
	if r["oldest"] != nil || r["newest"] != nil {
		t.Fatalf("want null range on an empty store, got %v", r)
	}
}

func TestHistoryRangeAndFrame(t *testing.T) {
	ts, _, store := buildTestServer(true, "IDLE")
	defer ts.Close()
	store.InsertFrame(100, []byte{0xFF, 0xD8, 0xFF, 0xD9})
	store.InsertFrame(200, []byte{0xFF, 0xD8, 0x01, 0xFF, 0xD9})

	resp, _ := ts.Client().Get(ts.URL + "/camera/history/range")
	var r map[string]any
	json.NewDecoder(resp.Body).Decode(&r)
	if r["oldest"] != float64(100) || r["newest"] != float64(200) {
		t.Fatalf("bad range: %v", r)
	}

	resp2, _ := ts.Client().Get(ts.URL + "/camera/history/frame?ts=150")
	body, _ := io.ReadAll(resp2.Body)
	if resp2.StatusCode != 200 || !bytes.Equal(body, []byte{0xFF, 0xD8, 0x01, 0xFF, 0xD9}) {
		t.Fatalf("frame at ts=150: status %d body %x, want the ts=200 frame", resp2.StatusCode, body)
	}
	if ct := resp2.Header.Get("Content-Type"); ct != "image/jpeg" {
		t.Fatalf("Content-Type = %q, want image/jpeg", ct)
	}
}

func TestHistoryFrameNotFound(t *testing.T) {
	ts, _, _ := buildTestServer(true, "IDLE")
	defer ts.Close()

	resp, _ := ts.Client().Get(ts.URL + "/camera/history/frame?ts=999")
	if resp.StatusCode != 404 {
		t.Fatalf("status %d, want 404", resp.StatusCode)
	}
}

func TestHistoryFrameInvalidTs(t *testing.T) {
	ts, _, _ := buildTestServer(true, "IDLE")
	defer ts.Close()

	resp, _ := ts.Client().Get(ts.URL + "/camera/history/frame?ts=notanumber")
	if resp.StatusCode != 400 {
		t.Fatalf("status %d, want 400", resp.StatusCode)
	}
}

func TestHistoryJobs(t *testing.T) {
	ts, _, store := buildTestServer(true, "IDLE")
	defer ts.Close()
	id, _ := store.OpenJob("benchy.3mf", 100)
	store.CloseJob(id, 200)
	store.OpenJob("ongoing.3mf", 300)

	resp, _ := ts.Client().Get(ts.URL + "/camera/history/jobs")
	var jobs []map[string]any
	json.NewDecoder(resp.Body).Decode(&jobs)
	if len(jobs) != 2 {
		t.Fatalf("got %d jobs, want 2", len(jobs))
	}
}
```

- [ ] **Step 4: Run the tests to verify they fail**

Run: `go test ./internal/web/... -run TestHistory -v`
Expected: FAIL with 404s (routes don't exist yet)

- [ ] **Step 5: Write the implementation**

In `internal/web/server.go`, register the routes in `Handler()`:

```go
	mux.HandleFunc("GET /camera/stream", s.camera)
	mux.HandleFunc("GET /camera/history/range", s.historyRange)
	mux.HandleFunc("GET /camera/history/frame", s.historyFrame)
	mux.HandleFunc("GET /camera/history/jobs", s.historyJobs)
```

Add the handlers, e.g. after the existing `camera` method:

```go
func (s *Server) historyRange(w http.ResponseWriter, _ *http.Request) {
	oldest, newest, err := s.store.Range()
	if err != nil {
		http.Error(w, "range query failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"oldest": oldest, "newest": newest})
}

func (s *Server) historyFrame(w http.ResponseWriter, r *http.Request) {
	ts, err := strconv.ParseInt(r.URL.Query().Get("ts"), 10, 64)
	if err != nil {
		http.Error(w, "invalid ts", http.StatusBadRequest)
		return
	}
	jpeg, _, err := s.store.FrameAtOrAfter(ts)
	if errors.Is(err, history.ErrNoFrame) {
		http.Error(w, "no frame at or after ts", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, "frame query failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "image/jpeg")
	w.Write(jpeg)
}

func (s *Server) historyJobs(w http.ResponseWriter, _ *http.Request) {
	jobs, err := s.store.RecentJobs()
	if err != nil {
		http.Error(w, "jobs query failed", http.StatusInternalServerError)
		return
	}
	out := make([]map[string]any, len(jobs))
	for i, j := range jobs {
		out[i] = map[string]any{"id": j.ID, "name": j.Name, "start": j.Start, "end": j.End}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}
```

- [ ] **Step 6: Run the tests to verify they pass**

Run: `go test ./internal/web/... -v`
Expected: PASS (every test in the package, old and new)

- [ ] **Step 7: Commit**

```bash
git add internal/web/server.go internal/web/server_test.go
git commit -m "feat: serve camera history range/frame/jobs over HTTP"
```

---

### Task 9: Wire it all up in `main`

**Files:**
- Modify: `cmd/bambu-util/main.go`

- [ ] **Step 1: Write the implementation**

```go
// cmd/bambu-util/main.go
// bambu-util serves a phone-friendly control page for a Bambu P1S on the
// local network: bed actions and live status over the printer's MQTT
// interface, camera via its chamber-image stream, recorded continuously
// into a rolling history buffer.
package main

import (
	"context"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/brhelwig/bambu-util/internal/history"
	"github.com/brhelwig/bambu-util/internal/p1s"
	"github.com/brhelwig/bambu-util/internal/web"
)

func mustEnv(key string) string {
	v := os.Getenv(key)
	if v == "" {
		log.Fatalf("missing required env var %s", key)
	}
	return v
}

// DefaultRetention is how long recorded frames are kept when
// RECORDING_RETENTION isn't set.
const DefaultRetention = 24 * time.Hour

func jobNameString(fields map[string]any) string {
	if v, ok := p1s.JobName(fields).(string); ok {
		return v
	}
	return ""
}

func main() {
	ip := mustEnv("PRINTER_IP")
	serial := mustEnv("PRINTER_SERIAL")
	accessCode := mustEnv("PRINTER_ACCESS_CODE")
	addr := os.Getenv("LISTEN_ADDR")
	if addr == "" {
		addr = ":8081"
	}
	dataDir := os.Getenv("DATA_DIR")
	if dataDir == "" {
		dataDir = "./data"
	}
	retention := DefaultRetention
	if v := os.Getenv("RECORDING_RETENTION"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			log.Fatalf("invalid RECORDING_RETENTION %q: %v", v, err)
		}
		retention = d
	}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		log.Fatalf("create data dir %s: %v", dataDir, err)
	}

	cache := p1s.NewStateCache()
	client := p1s.NewClient(ip, serial, accessCode, cache)
	client.Start()
	defer client.Stop()

	store, err := history.Open(filepath.Join(dataDir, "bambu-util.db"))
	if err != nil {
		log.Fatalf("open history store: %v", err)
	}
	defer store.Close()

	hub := web.NewHub(func(ctx context.Context, yield func([]byte)) error {
		return p1s.StreamFrames(ctx, net.JoinHostPort(ip, "6000"), "bblp", accessCode, yield)
	}, store)

	srv := web.NewServer(cache, client, hub, store)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go hub.Start(ctx)
	go srv.EnforceAutoOff(ctx)
	go history.RunPruner(ctx, store, retention, 5*time.Minute, time.Now)
	go history.NewJobWatcher(store).Run(ctx, 2*time.Second, func() (string, string) {
		fields, _ := cache.Snapshot()
		return p1s.GcodeState(fields), jobNameString(fields)
	})

	log.Printf("bambu-util listening on %s (printer %s, recording retention %s)", addr, ip, retention)
	log.Fatal(http.ListenAndServe(addr, srv.Handler()))
}
```

- [ ] **Step 2: Verify it builds, both natively and the way the Dockerfile does**

Run: `go build ./... && CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o /tmp/bu-arm64 ./cmd/bambu-util && rm /tmp/bu-arm64 && echo OK`
Expected: `OK`

- [ ] **Step 3: Run the full test suite**

Run: `go test ./...`
Expected: PASS, all packages

- [ ] **Step 4: Commit**

```bash
git add cmd/bambu-util/main.go
git commit -m "feat: wire up always-on recording, retention pruning, and job tracking in main"
```

---

### Task 10: History section in the UI

**Files:**
- Modify: `internal/web/static/index.html`

No test file — this codebase has no frontend test harness; the existing page isn't covered by automated tests either. Verify manually per Step 3.

- [ ] **Step 1: Add the History card markup**

Insert after the existing camera card (the `<div class="card">` containing `#cam` and `#lampBtn`) and before `<div class="actions">`:

```html
<div class="card" id="historyCard">
  <h2>History</h2>
  <div id="historyEmpty" style="color:#9aa0a6">No recordings yet</div>
  <div id="historyControls" style="display:none">
    <img id="historyImg" alt="recorded frame">
    <input type="range" id="historySlider" min="0" max="1" step="1" value="0">
    <div class="row"><span id="historyTime">…</span>
      <span>
        <button id="historyPlayBtn" style="padding:6px 14px; font-size:0.85rem">Play</button>
        <select id="historySpeedSelect">
          <option value="1">1x</option>
          <option value="5">5x</option>
          <option value="20" selected>20x</option>
          <option value="60">60x</option>
        </select>
      </span>
    </div>
    <h2 style="margin-top:14px">Recent jobs</h2>
    <div id="historyJobs"></div>
  </div>
</div>
```

- [ ] **Step 2: Add CSS for the new elements**

In the `<style>` block, alongside the existing `#cam` rule:

```css
  #historyImg { width:100%; border-radius:12px; display:block; background:#0d0f12; min-height:120px; }
  select { border:0; border-radius:8px; padding:8px; font-size:0.9rem; background:#2a313b; color:#e8eaed; }
```

- [ ] **Step 3: Add the History JS**

Append near the end of the `<script>` block, after the existing print-control wiring:

```js
// History: DVR-style scrub through the recorded buffer, and per-job
// timelapse playback, both driven by repeatedly fetching the nearest stored
// frame at a chosen timestamp — no video file, no streaming protocol.
let historyRange = null; // {oldest, newest} in unix seconds, or nulls if empty
let historyPos = 0;
let historyPlaying = false;
let historyTimer = null;
let historyBoundEnd = null; // caps playback when watching a single job's clip

function fmtHistoryTime(ts) {
  return new Date(ts * 1000).toLocaleString([], {
    month: "short", day: "numeric", hour: "2-digit", minute: "2-digit", second: "2-digit",
  });
}

function showHistoryFrame(ts) {
  const img = new Image();
  img.onload = () => { $("historyImg").src = img.src; };
  img.src = `/camera/history/frame?ts=${Math.floor(ts)}`; // onerror: hold the last frame shown
  $("historyTime").textContent = fmtHistoryTime(ts);
}

function stopHistoryPlayback() {
  historyPlaying = false;
  historyBoundEnd = null;
  $("historyPlayBtn").textContent = "Play";
  if (historyTimer) { clearInterval(historyTimer); historyTimer = null; }
}

function startHistoryPlayback() {
  historyPlaying = true;
  $("historyPlayBtn").textContent = "Pause";
  const speed = Number($("historySpeedSelect").value);
  historyTimer = setInterval(() => {
    historyPos += speed;
    const cap = historyBoundEnd ?? historyRange.newest;
    if (historyPos >= cap) {
      historyPos = cap;
      showHistoryFrame(historyPos);
      $("historySlider").value = historyPos;
      stopHistoryPlayback();
      return;
    }
    $("historySlider").value = historyPos;
    showHistoryFrame(historyPos);
  }, 1000);
}

$("historyPlayBtn").addEventListener("click", () => {
  if (historyPlaying) stopHistoryPlayback();
  else startHistoryPlayback();
});

$("historySlider").addEventListener("input", () => {
  stopHistoryPlayback();
  historyPos = Number($("historySlider").value);
  showHistoryFrame(historyPos);
});

function playJob(job) {
  stopHistoryPlayback();
  historyPos = job.start;
  historyBoundEnd = job.end || historyRange.newest;
  $("historySlider").value = historyPos;
  showHistoryFrame(historyPos);
  startHistoryPlayback();
}

function renderHistoryJobs(jobs) {
  const container = $("historyJobs");
  container.innerHTML = "";
  jobs.forEach(j => {
    const row = document.createElement("div");
    row.className = "row";
    const label = document.createElement("span");
    label.textContent = j.name || "(unnamed job)";
    const btn = document.createElement("button");
    btn.textContent = j.end ? "Timelapse" : "Watch job so far";
    btn.style.cssText = "padding:6px 14px; font-size:0.8rem; border-radius:8px";
    btn.addEventListener("click", () => playJob(j));
    row.append(label, btn);
    container.appendChild(row);
  });
}

async function refreshHistoryRange() {
  try {
    const r = await fetch("/camera/history/range");
    historyRange = await r.json();
  } catch {
    return;
  }
  const has = historyRange.oldest != null && historyRange.newest != null;
  $("historyEmpty").style.display = has ? "none" : "";
  $("historyControls").style.display = has ? "" : "none";
  if (!has) return;
  $("historySlider").min = historyRange.oldest;
  $("historySlider").max = historyRange.newest;
  if (!historyPlaying) {
    historyPos = historyRange.newest;
    $("historySlider").value = historyPos;
    showHistoryFrame(historyPos);
  }
}

async function refreshHistoryJobs() {
  try {
    const r = await fetch("/camera/history/jobs");
    renderHistoryJobs(await r.json());
  } catch {
    // leave the last-known job list in place
  }
}

refreshHistoryRange();
refreshHistoryJobs();
setInterval(refreshHistoryRange, 30000);
setInterval(refreshHistoryJobs, 30000);
```

- [ ] **Step 4: Manually verify in a browser**

Run: `PRINTER_IP=192.0.2.10 PRINTER_SERIAL=00000000 PRINTER_ACCESS_CODE=x DATA_DIR=/tmp/bu-history-check go run ./cmd/bambu-util`

Open `http://localhost:8081/?demo` (the printer connection will fail since `192.0.2.10` isn't real, but the page and its `/camera/history/*` calls run against the real embedded server). Confirm:
- The History card shows "No recordings yet" (no camera reachable, so nothing's been recorded).
- No JS console errors.

Then stop the server and clean up: `rm -rf /tmp/bu-history-check`

- [ ] **Step 5: Commit**

```bash
git add internal/web/static/index.html
git commit -m "feat: add DVR scrub bar and per-job timelapse playback to the UI"
```

---

### Task 11: Documentation

**Files:**
- Modify: `README.md`
- Modify: `CHANGELOG.md`

- [ ] **Step 1: Update the Features list in `README.md`**

Replace:

```markdown
- Chamber camera view (~1 fps), relayed as MJPEG; the bridge only connects to
  the printer camera while someone is watching
```

with:

```markdown
- Chamber camera view (~1 fps), relayed as MJPEG. The bridge holds the
  camera connection continuously (not just while someone is watching) so it
  can record — this means Bambu Studio's own camera view will not work
  while bambu-util is running, since the printer only serves one camera
  client at a time
- Rolling history buffer of recorded frames (`RECORDING_RETENTION`, default
  24h): scrub back through recent footage, or jump to any print job and
  fast-forward through just that job's footage as a timelapse
```

- [ ] **Step 2: Add the new config vars to the table in `README.md`**

Replace:

```markdown
| `LISTEN_ADDR` | no | Listen address, default `:8081` |
```

with:

```markdown
| `LISTEN_ADDR` | no | Listen address, default `:8081` |
| `DATA_DIR` | no | Directory for the recording database, default `./data`. Mount a volume here so the history buffer survives restarts. |
| `RECORDING_RETENTION` | no | How long to keep recorded frames, as a Go duration (`12h`, `48h`, ...), default `24h` |
```

- [ ] **Step 3: Add an `Unreleased` entry to `CHANGELOG.md`**

Insert after the top-level intro paragraph, before `## [0.4.0]`:

```markdown
## [Unreleased]

### Added

- Always-on camera recording into a rolling history buffer (default 24h,
  configurable via `RECORDING_RETENTION`), stored in SQLite under `DATA_DIR`.
- History UI: scrub back through recent footage, or jump to a print job and
  fast-forward through just that job's footage as a timelapse.

### Changed

- The camera connection is now held continuously so it can record, instead
  of only while a viewer is on the page. Bambu Studio's own camera view will
  not work while bambu-util is running.

```

- [ ] **Step 4: Commit**

```bash
git add README.md CHANGELOG.md
git commit -m "docs: document always-on recording, history UI, and new config vars"
```

---

### Task 12: Final verification

**Files:** none (verification only)

- [ ] **Step 1: Full test suite**

Run: `go test ./... -v`
Expected: PASS, every package

- [ ] **Step 2: Vet**

Run: `go vet ./...`
Expected: no output

- [ ] **Step 3: Native build**

Run: `go build ./...`
Expected: no output, exit 0

- [ ] **Step 4: Docker-equivalent cross build**

Run: `CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags="-s -w" -o /tmp/bu-final-check ./cmd/bambu-util && rm /tmp/bu-final-check && echo OK`
Expected: `OK`

- [ ] **Step 5: Full Docker image build**

Run: `docker build -t bambu-util:history-check .`
Expected: build succeeds

- [ ] **Step 6: Confirm nothing was left uncommitted**

Run: `git status --short`
Expected: empty (clean working tree)
