package plainforge

import (
	"context"
	"errors"
	"testing"
)

func TestMemory(t *testing.T) {
	ctx := context.Background()
	m := NewMemory()

	if _, found, err := m.FindOpen(ctx, "aa"); found || err != nil {
		t.Fatalf("empty store: found=%v err=%v", found, err)
	}
	a, err := m.Open(ctx, NewTask{Title: "t", Body: "b", Fingerprint: "aa"})
	if err != nil || a.ID != 1 || a.Occurrences != 1 || !a.Open {
		t.Fatalf("open: %+v %v", a, err)
	}
	if b, _ := m.Open(ctx, NewTask{Fingerprint: "bb"}); b.ID != 2 {
		t.Fatalf("second id = %d", b.ID)
	}
	if got, found, _ := m.FindOpen(ctx, "aa"); !found || got != a {
		t.Fatalf("find: %+v %v", got, found)
	}
	if got, err := m.AddOccurrence(ctx, 1); err != nil || got.Occurrences != 2 {
		t.Fatalf("add: %+v %v", got, err)
	}
	if _, err := m.AddOccurrence(ctx, 9); !errors.Is(err, ErrBadResponse) {
		t.Fatalf("add to missing task: %v", err)
	}

	if !m.Close(1) || m.Close(1) || m.Close(9) {
		t.Fatal("close did not report correctly")
	}
	if _, found, _ := m.FindOpen(ctx, "aa"); found {
		t.Fatal("closed task was found as open")
	}
	if _, err := m.AddOccurrence(ctx, 1); !errors.Is(err, ErrBadResponse) {
		t.Fatalf("add to closed task: %v", err)
	}

	all := m.All()
	if len(all) != 2 || all[0].Open || all[0].Occurrences != 2 || !all[1].Open {
		t.Fatalf("all: %+v", all)
	}
	all[1].Title = "changed"
	if m.All()[1].Title == "changed" {
		t.Fatal("All returned the store's own slice")
	}
}

func TestMemoryFailNextChangesNothing(t *testing.T) {
	ctx := context.Background()
	m := NewMemory()
	m.FailNext(ErrTimeout)
	if _, err := m.Open(ctx, NewTask{Fingerprint: "aa"}); !errors.Is(err, ErrTimeout) {
		t.Fatalf("err = %v", err)
	}
	if len(m.All()) != 0 {
		t.Fatal("failed Open wrote a task")
	}
	if _, err := m.Open(ctx, NewTask{Fingerprint: "aa"}); err != nil {
		t.Fatalf("failure was not used up: %v", err)
	}
}

func TestMemoryExpiredContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := NewMemory().FindOpen(ctx, "aa"); !errors.Is(err, ErrTimeout) {
		t.Fatalf("err = %v", err)
	}
}
