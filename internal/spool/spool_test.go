package spool

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ClearedStack/bellbuoy/internal/work"
)

func job(id string) work.Job {
	return work.Job{ID: id, Lane: work.LaneNormal, Kind: work.KindError, Payload: json.RawMessage(`{"worker":"muster"}`)}
}

func TestBackoffSchedule(t *testing.T) {
	want := []time.Duration{
		time.Second, time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second,
		32 * time.Second, 64 * time.Second, 128 * time.Second, 256 * time.Second, 5 * time.Minute, 5 * time.Minute,
	}
	for attempts, w := range want {
		if got := Backoff(attempts); got != w {
			t.Errorf("Backoff(%d) = %v, want %v", attempts, got, w)
		}
	}
	if Backoff(1000) != 5*time.Minute {
		t.Error("the delay must stay capped however many attempts there are")
	}
}

func TestPutGetDelete(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	rec := &Record{Job: job("job-1"), Done: []string{"internal-git"}, Attempts: 1, RetryAt: 100,
		State: work.State{Task: &work.TaskResult{Repo: "muster", ID: "muster#1", Created: true, Occurrences: 1}},
		Fault: &Fault{Code: "E_PARSE", Reason: "r"}}
	if err := s.Put(rec); err != nil {
		t.Fatal(err)
	}
	if rec.Seq != 1 || !s.Has("job-1") || s.Len() != 1 {
		t.Fatalf("after Put: seq=%d has=%v len=%d", rec.Seq, s.Has("job-1"), s.Len())
	}
	got, err := s.Get("job-1")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, rec) {
		t.Fatalf("read back differs:\n got %+v\nwant %+v", got, rec)
	}
	if err := s.Delete("job-1"); err != nil {
		t.Fatal(err)
	}
	if s.Has("job-1") || s.Len() != 0 {
		t.Fatal("record survived Delete")
	}
	if err := s.Delete("job-1"); err != nil {
		t.Fatal("deleting twice must be fine")
	}
	if _, err := s.Get("job-1"); err == nil {
		t.Fatal("Get after Delete must fail")
	}
}

func TestRecordsSurviveReopen(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(dir)
	for i, id := range []string{"job-b", "job-a", "job-c"} {
		if err := s.Put(&Record{Job: job(id), RetryAt: int64(10 * (i + 1)), Finished: id == "job-a"}); err != nil {
			t.Fatal(err)
		}
	}
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if s.Len() != 3 {
		t.Fatalf("reopened spool holds %d records", s.Len())
	}
	// Oldest first, by the order they were staged, not by name.
	if got := s.Due(time.Unix(1000, 0)); !reflect.DeepEqual(got, []string{"job-b", "job-a", "job-c"}) {
		t.Fatalf("Due = %v", got)
	}
	if got := s.Due(time.Unix(20, 0)); !reflect.DeepEqual(got, []string{"job-b", "job-a"}) {
		t.Fatalf("Due(20) = %v", got)
	}
	if got := s.Unfinished(); !reflect.DeepEqual(got, []string{"job-b", "job-c"}) {
		t.Fatalf("Unfinished = %v", got)
	}
	// A new record must be numbered after the ones already there.
	rec := &Record{Job: job("job-d")}
	s.Put(rec)
	if rec.Seq != 4 {
		t.Fatalf("seq after reopen = %d, want 4", rec.Seq)
	}
}

func TestReplacingARecordKeepsItsPlace(t *testing.T) {
	s, _ := Open(t.TempDir())
	first := &Record{Job: job("job-1")}
	s.Put(first)
	s.Put(&Record{Job: job("job-2")})
	first.Attempts = 5
	first.RetryAt = 0
	s.Put(first)
	if got := s.Due(time.Unix(0, 0)); !reflect.DeepEqual(got, []string{"job-1", "job-2"}) {
		t.Fatalf("order changed: %v", got)
	}
	if s.Len() != 2 {
		t.Fatalf("len = %d", s.Len())
	}
}

func TestBrokenFilesAreLeftAloneAndCounted(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(dir)
	s.Put(&Record{Job: job("job-1")})
	os.WriteFile(filepath.Join(dir, "0123456789abcdef0123456789abcdef.json"), []byte("{not json"), 0o600)
	os.WriteFile(filepath.Join(dir, "ffffffffffffffffffffffffffffffff.json"), []byte(`{"seq":9,"job":{"id":"job-9"}}`), 0o600) // wrong name for its job
	os.WriteFile(filepath.Join(dir, "tmp-leftover.json"), []byte("half"), 0o600)
	os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("ignored"), 0o600)

	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if s.Len() != 1 || !s.Has("job-1") {
		t.Fatalf("len = %d", s.Len())
	}
	if s.Skipped() != 2 {
		t.Fatalf("skipped = %d, want 2", s.Skipped())
	}
	if _, err := os.Stat(filepath.Join(dir, "0123456789abcdef0123456789abcdef.json")); err != nil {
		t.Fatal("a file that could not be read must not be deleted")
	}
	if _, err := os.Stat(filepath.Join(dir, "tmp-leftover.json")); !os.IsNotExist(err) {
		t.Fatal("a half-written file must be cleared away")
	}
}

// A job reference never becomes part of a file path, whatever it holds.
func TestFileNamesAreHashes(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(dir)
	if err := s.Put(&Record{Job: job("../../escape")}); err != nil {
		t.Fatal(err)
	}
	names, _ := os.ReadDir(dir)
	if len(names) != 1 {
		t.Fatalf("%d files in the spool", len(names))
	}
	name := names[0].Name()
	if len(name) != 32+len(".json") || strings.ContainsAny(name, "/\\") || strings.Contains(name, "escape") {
		t.Fatalf("unexpected file name %q", name)
	}
}

func TestOpenErrorNamesNoPath(t *testing.T) {
	file := filepath.Join(t.TempDir(), "plain-file")
	os.WriteFile(file, nil, 0o600)
	_, err := Open(filepath.Join(file, "sub"))
	if err == nil {
		t.Fatal("Open under a plain file must fail")
	}
	if strings.Contains(err.Error(), "plain-file") {
		t.Fatalf("error names the path: %s", err)
	}
}
