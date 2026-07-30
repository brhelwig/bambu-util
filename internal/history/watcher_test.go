package history

import (
	"testing"
	"time"
)

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

func TestJobWatcherKeepsOneRowAcrossPauseAndResume(t *testing.T) {
	s, _ := Open(":memory:")
	defer s.Close()
	w := NewJobWatcher(s)

	w.Poll("RUNNING", "a.3mf")
	w.Poll("PAUSE", "a.3mf")
	w.Poll("RUNNING", "a.3mf")

	jobs, _ := s.RecentJobs()
	if len(jobs) != 1 {
		t.Fatalf("pause/resume split the print into %d rows: %+v", len(jobs), jobs)
	}
	if jobs[0].End != nil {
		t.Fatalf("print was closed while still running: %+v", jobs[0])
	}
}

func TestJobWatcherAdoptsARowLeftOpenByAnEarlierProcess(t *testing.T) {
	s, _ := Open(":memory:")
	defer s.Close()
	NewJobWatcher(s).Poll("RUNNING", "a.3mf")

	// Same store, fresh watcher: what a restart mid-print looks like.
	restarted := NewJobWatcher(s)
	restarted.Poll("RUNNING", "a.3mf")

	jobs, _ := s.RecentJobs()
	if len(jobs) != 1 {
		t.Fatalf("restart mid-print opened a second row: %+v", jobs)
	}

	// The adopted row is the one that gets closed, so nothing is left open.
	restarted.Poll("FINISH", "a.3mf")
	jobs, _ = s.RecentJobs()
	if len(jobs) != 1 || jobs[0].End == nil {
		t.Fatalf("adopted row was not closed: %+v", jobs)
	}
}

func TestJobWatcherClosesRowsStrandedByTheOldRestartBug(t *testing.T) {
	s, _ := Open(":memory:")
	defer s.Close()
	// The state the screenshots in issue #26 show: one print listed three
	// times, two of the rows never closed.
	s.OpenJob("pencil.3mf", 1000)
	s.OpenJob("pencil.3mf", 2000)
	s.OpenJob("pencil.3mf", 3000)

	NewJobWatcher(s)

	jobs, _ := s.RecentJobs()
	open := 0
	for _, j := range jobs {
		if j.End == nil {
			open++
		}
	}
	if len(jobs) != 3 {
		t.Fatalf("rows should be kept, only closed: %+v", jobs)
	}
	if open != 1 {
		t.Fatalf("want exactly 1 row still open, got %d: %+v", open, jobs)
	}
}

func TestJobWatcherStartsAFreshRowWhenADifferentPrintIsRunning(t *testing.T) {
	s, _ := Open(":memory:")
	defer s.Close()
	first := NewJobWatcher(s)
	first.now = func() time.Time { return time.Unix(1000, 0) }
	first.Poll("RUNNING", "a.3mf")

	// The service was down while a.3mf finished and b.3mf started, so the row it
	// adopts belongs to a print that is no longer the one running.
	restarted := NewJobWatcher(s)
	restarted.now = func() time.Time { return time.Unix(5000, 0) }
	restarted.Poll("RUNNING", "b.3mf")

	jobs, _ := s.RecentJobs() // newest first
	if len(jobs) != 2 {
		t.Fatalf("want a row per print, got %+v", jobs)
	}
	if jobs[0].Name != "b.3mf" || jobs[0].End != nil {
		t.Fatalf("newest row should be b.3mf, still running: %+v", jobs[0])
	}
	if jobs[1].Name != "a.3mf" || jobs[1].End == nil {
		t.Fatalf("a.3mf should have been closed, not left to swallow b's footage: %+v", jobs[1])
	}
}

func TestJobWatcherKeepsTheOpenRowWhenTheNameIsMissing(t *testing.T) {
	s, _ := Open(":memory:")
	defer s.Close()
	w := NewJobWatcher(s)
	w.Poll("RUNNING", "a.3mf")

	// A report that carries no job name says nothing about which print is
	// running, so it must not split the row.
	w.Poll("RUNNING", "")

	jobs, _ := s.RecentJobs()
	if len(jobs) != 1 || jobs[0].End != nil {
		t.Fatalf("a nameless report split the print: %+v", jobs)
	}
}

func TestJobWatcherIgnoresUnknownState(t *testing.T) {
	s, _ := Open(":memory:")
	defer s.Close()
	NewJobWatcher(s).Poll("RUNNING", "a.3mf")

	// A restart polls before the printer's first report, when GcodeState
	// reports "unknown". That is absence of information, not a finished print.
	restarted := NewJobWatcher(s)
	restarted.Poll("unknown", "")

	jobs, _ := s.RecentJobs()
	if len(jobs) != 1 || jobs[0].End != nil {
		t.Fatalf("unknown state closed the open print: %+v", jobs)
	}
}

func TestJobWatcherClosesAtTheLastRecordedFrame(t *testing.T) {
	s, _ := Open(":memory:")
	defer s.Close()
	w := NewJobWatcher(s)
	w.now = func() time.Time { return time.Unix(9000, 0) }

	w.Poll("RUNNING", "a.3mf")
	for _, ts := range []int64{9000, 9100, 9200} {
		s.InsertFrame(ts, []byte{1})
	}
	// Recording stops here; the process notices the print ended much later.
	w.now = func() time.Time { return time.Unix(50000, 0) }
	w.Poll("FINISH", "a.3mf")

	jobs, _ := s.RecentJobs()
	if jobs[0].End == nil || *jobs[0].End != 9200 {
		t.Fatalf("want the job closed at its last frame 9200, got %+v", jobs[0])
	}
}

func TestJobWatcherClosesAtNowWithoutFootage(t *testing.T) {
	s, _ := Open(":memory:")
	defer s.Close()
	w := NewJobWatcher(s)
	w.now = func() time.Time { return time.Unix(9000, 0) }

	w.Poll("RUNNING", "a.3mf")
	w.now = func() time.Time { return time.Unix(9500, 0) }
	w.Poll("FINISH", "a.3mf")

	jobs, _ := s.RecentJobs()
	if jobs[0].End == nil || *jobs[0].End != 9500 {
		t.Fatalf("want fallback to now (9500) with no frames, got %+v", jobs[0])
	}
}

func TestJobWatcherIgnoresPrepareWhileRunning(t *testing.T) {
	s, _ := Open(":memory:")
	defer s.Close()
	w := NewJobWatcher(s)

	w.Poll("RUNNING", "a.3mf")
	w.Poll("PREPARE", "a.3mf")

	jobs, _ := s.RecentJobs()
	if len(jobs) != 1 || jobs[0].End != nil {
		t.Fatalf("PREPARE closed the open print: %+v", jobs)
	}
}
