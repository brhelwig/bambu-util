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
