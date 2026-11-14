package plainforge

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/ClearedStack/bellbuoy/internal/codes"
)

var ctx = context.Background()

func TestLedgerSurvivesReopening(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tasks.ledger")
	l, err := OpenLedger(path)
	if err != nil {
		t.Fatal(err)
	}
	first, _ := l.OpenTask(ctx, "muster", TaskDraft{Key: "k1", Title: "one"})
	l.OpenTask(ctx, "keelworks", TaskDraft{Key: "k2", Title: "two"})
	l.AddOccurrence(ctx, "muster", first.ID)
	l.AddOccurrence(ctx, "muster", first.ID)
	created, err := l.ProposeChange(ctx, Change{Repo: "muster", Branch: "edit/ed-1", Path: "a.json", Content: "{}"})
	if err != nil || !created {
		t.Fatalf("created=%v err=%v", created, err)
	}
	want := l.Tasks()

	l, err = OpenLedger(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := l.Tasks(); !reflect.DeepEqual(got, want) || got[0].Occurrences != 3 {
		t.Fatalf("tasks after reopening = %+v", got)
	}
	if len(l.Changes()) != 1 {
		t.Fatal("the proposed change did not survive")
	}
	// Found again, counted on, and numbered on from where it stopped.
	found, ok, _ := l.FindOpenTask(ctx, "muster", "k1")
	if !ok || found.ID != first.ID {
		t.Fatalf("task not found again: %+v %v", found, ok)
	}
	next, _ := l.OpenTask(ctx, "muster", TaskDraft{Key: "k3"})
	if next.ID != "muster#2" {
		t.Fatalf("next task is %s, want muster#2", next.ID)
	}
	created, _ = l.ProposeChange(ctx, Change{Repo: "muster", Branch: "edit/ed-1", Path: "a.json", Content: "{}"})
	if created {
		t.Fatal("the same change was proposed twice after reopening")
	}
}

func TestLedgerThatCannotBeWrittenUndoesTheChange(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tasks.ledger")
	l, _ := OpenLedger(path)
	task, _ := l.OpenTask(ctx, "muster", TaskDraft{Key: "k1"})

	// Put a folder where the new file has to be written.
	if err := os.Mkdir(path+".tmp", 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := l.AddOccurrence(ctx, "muster", task.ID); codes.From(err) == nil || codes.From(err).Code != codes.Timeout {
		t.Fatalf("err = %v, want E_TIMEOUT", err)
	}
	if _, err := l.OpenTask(ctx, "muster", TaskDraft{Key: "k2"}); codes.From(err) == nil {
		t.Fatal("opening a task succeeded although it could not be written")
	}
	if got := l.Tasks(); len(got) != 1 || got[0].Occurrences != 1 {
		t.Fatalf("a change that was not written was kept in memory: %+v", got)
	}

	// Once it can be written again the retry counts once, not twice.
	os.Remove(path + ".tmp")
	if got, err := l.AddOccurrence(ctx, "muster", task.ID); err != nil || got.Occurrences != 2 {
		t.Fatalf("retry: %+v %v", got, err)
	}
	reopened, _ := OpenLedger(path)
	if got := reopened.Tasks(); len(got) != 1 || got[0].Occurrences != 2 {
		t.Fatalf("on disk: %+v", got)
	}
}

func TestLedgerRefusesAFileItCannotUnderstand(t *testing.T) {
	dir := t.TempDir()
	for name, content := range map[string]string{
		"not json":      "{",
		"wrong shape":   `{"tasks":"many"}`,
		"unknown field": `{"tasks":[],"changes":[],"extra":1}`,
		"trailing":      `{"tasks":[],"changes":[]} more`,
	} {
		path := filepath.Join(dir, "bad.ledger")
		os.WriteFile(path, []byte(content), 0o600)
		_, err := OpenLedger(path)
		if e := codes.From(err); e == nil || e.Code != codes.Parse {
			t.Errorf("%s: err = %v, want E_PARSE", name, err)
		}
	}
	if _, err := OpenLedger(filepath.Join(dir, "not-there-yet.ledger")); err != nil {
		t.Fatalf("a ledger that does not exist yet is an empty one: %v", err)
	}
}

func TestLedgerHasNoMirror(t *testing.T) {
	l, _ := OpenLedger(filepath.Join(t.TempDir(), "tasks.ledger"))
	if e := codes.From(l.SyncMirror(ctx, "muster", []string{"source"})); e == nil || e.Code != codes.NeedsSubtool {
		t.Fatalf("err = %v, want E_NEEDS_SUBTOOL", e)
	}
	if len(l.Mirrors()) != 0 {
		t.Fatal("a mirror request was recorded")
	}
}
