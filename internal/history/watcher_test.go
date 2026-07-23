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
