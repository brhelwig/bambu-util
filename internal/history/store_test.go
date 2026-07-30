package history

import (
	"errors"
	"fmt"
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

func TestPruneKeepsTheNewestFinishedJobsAndDropsTheRest(t *testing.T) {
	s, _ := Open(":memory:")
	defer s.Close()
	// KeptJobs+1 finished jobs, all long expired. Only the oldest should go.
	for i := 0; i <= KeptJobs; i++ {
		start := int64(100 + i*10)
		id, _ := s.OpenJob(fmt.Sprintf("job%d.3mf", i), start)
		s.CloseJob(id, start+5)
	}

	if err := s.Prune(100000); err != nil {
		t.Fatal(err)
	}
	jobs, _ := s.RecentJobs()
	if len(jobs) != KeptJobs {
		t.Fatalf("want %d jobs kept, got %d: %+v", KeptJobs, len(jobs), jobs)
	}
	for _, j := range jobs {
		if j.Name == "job0.3mf" {
			t.Fatalf("oldest job survived beyond the newest %d: %+v", KeptJobs, jobs)
		}
	}
}

func TestPruneKeepsAndThinsFootageOfKeptJobs(t *testing.T) {
	s, _ := Open(":memory:")
	defer s.Close()
	// One finished job spanning 1000..1099, recorded once a second.
	id, _ := s.OpenJob("kept.3mf", 1000)
	s.CloseJob(id, 1099)
	for ts := int64(1000); ts <= 1099; ts++ {
		s.InsertFrame(ts, []byte{1})
	}

	if err := s.Prune(5000); err != nil {
		t.Fatal(err)
	}

	kept := frameTimestamps(t, s)
	// 100 seconds of footage thinned to one frame per ThinInterval.
	want := 100 / ThinInterval
	if len(kept) != want {
		t.Fatalf("want %d thinned frames, got %d: %v", want, len(kept), kept)
	}
	// The earliest frame of each interval is the one kept, so the survivors are
	// evenly spaced and start at the job's first frame.
	for i, ts := range kept {
		if wantTs := int64(1000 + i*ThinInterval); ts != wantTs {
			t.Fatalf("frame %d is %d, want %d (all: %v)", i, ts, wantTs, kept)
		}
	}
}

func TestPruneDeletesFramesOutsideEveryKeptJob(t *testing.T) {
	s, _ := Open(":memory:")
	defer s.Close()
	id, _ := s.OpenJob("kept.3mf", 1000)
	s.CloseJob(id, 1099)
	s.InsertFrame(1000, []byte{1}) // inside the kept job
	s.InsertFrame(500, []byte{2})  // idle footage, expired
	s.InsertFrame(2000, []byte{3}) // idle footage, expired

	if err := s.Prune(5000); err != nil {
		t.Fatal(err)
	}
	if got := frameTimestamps(t, s); len(got) != 1 || got[0] != 1000 {
		t.Fatalf("got frames %v, want only the kept job's 1000", got)
	}
}

func TestPruneLeavesFramesNewerThanCutoffAtFullRate(t *testing.T) {
	s, _ := Open(":memory:")
	defer s.Close()
	// A job still in progress, all of whose footage is newer than the cutoff.
	s.OpenJob("running.3mf", 1000)
	for ts := int64(1000); ts <= 1099; ts++ {
		s.InsertFrame(ts, []byte{1})
	}

	if err := s.Prune(900); err != nil {
		t.Fatal(err)
	}
	if got := frameTimestamps(t, s); len(got) != 100 {
		t.Fatalf("post-cutoff footage was thinned: %d frames left, want 100", len(got))
	}
}

func TestPruneThinsOnlyTheExpiredPartOfARunningJob(t *testing.T) {
	s, _ := Open(":memory:")
	defer s.Close()
	s.OpenJob("long.3mf", 1000)
	for ts := int64(1000); ts <= 1099; ts++ {
		s.InsertFrame(ts, []byte{1})
	}

	// Cutoff mid-job: 1000..1049 is expired and thins, 1050..1099 stays whole.
	if err := s.Prune(1050); err != nil {
		t.Fatal(err)
	}
	got := frameTimestamps(t, s)
	want := 50/ThinInterval + 50
	if len(got) != want {
		t.Fatalf("got %d frames, want %d: %v", len(got), want, got)
	}
	if got[len(got)-1] != 1099 {
		t.Fatalf("newest frame is %d, want 1099", got[len(got)-1])
	}
}

func TestActiveJobEmpty(t *testing.T) {
	s, _ := Open(":memory:")
	defer s.Close()

	job, err := s.ActiveJob()
	if err != nil {
		t.Fatal(err)
	}
	if job != nil {
		t.Fatalf("got %+v, want nil with no jobs", job)
	}
}

func TestActiveJobIgnoresFinishedJobs(t *testing.T) {
	s, _ := Open(":memory:")
	defer s.Close()
	id, _ := s.OpenJob("done.3mf", 100)
	s.CloseJob(id, 200)

	job, err := s.ActiveJob()
	if err != nil {
		t.Fatal(err)
	}
	if job != nil {
		t.Fatalf("got %+v, want nil when every job is finished", job)
	}
}

func TestActiveJobReturnsTheOpenOne(t *testing.T) {
	s, _ := Open(":memory:")
	defer s.Close()
	done, _ := s.OpenJob("done.3mf", 100)
	s.CloseJob(done, 200)
	open, _ := s.OpenJob("running.3mf", 300)

	job, err := s.ActiveJob()
	if err != nil {
		t.Fatal(err)
	}
	if job == nil || job.ID != open || job.Name != "running.3mf" || job.Start != 300 {
		t.Fatalf("got %+v, want the open running.3mf row", job)
	}
}

func TestCloseOrphanJobsLeavesASingleOpenRowAlone(t *testing.T) {
	s, _ := Open(":memory:")
	defer s.Close()
	s.OpenJob("running.3mf", 1000)

	n, err := s.CloseOrphanJobs()
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("closed %d rows, want 0 — the only open row is the running print", n)
	}
	jobs, _ := s.RecentJobs()
	if len(jobs) != 1 || jobs[0].End != nil {
		t.Fatalf("running print was closed: %+v", jobs)
	}
}

func TestCloseOrphanJobsClosesStrandedRowsAtTheirLastFrame(t *testing.T) {
	s, _ := Open(":memory:")
	defer s.Close()
	// What the old restart bug left behind: two rows for the same file, both
	// open, plus a genuinely running third print.
	s.OpenJob("pencil.3mf", 1000)
	s.OpenJob("pencil.3mf", 2000)
	s.OpenJob("running.3mf", 3000)
	for _, ts := range []int64{1000, 1500, 1900, 2000, 2400, 3000, 3500} {
		s.InsertFrame(ts, []byte{1})
	}

	n, err := s.CloseOrphanJobs()
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("closed %d rows, want 2", n)
	}

	jobs, _ := s.RecentJobs() // newest first
	if len(jobs) != 3 {
		t.Fatalf("want 3 rows, got %+v", jobs)
	}
	if jobs[0].Name != "running.3mf" || jobs[0].End != nil {
		t.Fatalf("newest row should stay open: %+v", jobs[0])
	}
	// Each orphan ends at its last frame before the next print began, so no
	// orphan claims footage belonging to a later print.
	if jobs[1].End == nil || *jobs[1].End != 2400 {
		t.Fatalf("orphan starting at 2000 should end at its last frame 2400: %+v", jobs[1])
	}
	if jobs[2].End == nil || *jobs[2].End != 1900 {
		t.Fatalf("orphan starting at 1000 should end at 1900, before the next print: %+v", jobs[2])
	}
}

func TestCloseOrphanJobsFallsBackToStartWithoutFootage(t *testing.T) {
	s, _ := Open(":memory:")
	defer s.Close()
	s.OpenJob("footage-already-pruned.3mf", 1000)
	s.OpenJob("running.3mf", 5000)

	if _, err := s.CloseOrphanJobs(); err != nil {
		t.Fatal(err)
	}
	jobs, _ := s.RecentJobs()
	orphan := jobs[len(jobs)-1]
	if orphan.End == nil || *orphan.End != orphan.Start {
		t.Fatalf("want the orphan closed at its own start, got %+v", orphan)
	}
}

func TestPruneIgnoresAllButTheNewestOpenRow(t *testing.T) {
	s, _ := Open(":memory:")
	defer s.Close()
	// A stranded open row must not shield every later frame from the cutoff.
	s.OpenJob("stranded.3mf", 100)
	s.OpenJob("running.3mf", 9000)
	s.InsertFrame(500, []byte{1})  // inside the stranded row, expired
	s.InsertFrame(9500, []byte{2}) // inside the running print

	if err := s.Prune(5000); err != nil {
		t.Fatal(err)
	}
	if got := frameTimestamps(t, s); len(got) != 1 || got[0] != 9500 {
		t.Fatalf("got frames %v, want only the running print's 9500", got)
	}
}

// frameTimestamps lists every stored frame timestamp, oldest first.
func frameTimestamps(t *testing.T, s *Store) []int64 {
	t.Helper()
	rows, err := s.db.Query(`SELECT ts FROM frames ORDER BY ts ASC`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var ts int64
		if err := rows.Scan(&ts); err != nil {
			t.Fatal(err)
		}
		out = append(out, ts)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}
