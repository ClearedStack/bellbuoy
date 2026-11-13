package tasks

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/ClearedStack/bellbuoy/internal/codes"
	"github.com/ClearedStack/bellbuoy/internal/fingerprint"
	"github.com/ClearedStack/bellbuoy/internal/plainforge"
)

type fixture struct {
	About    string `json:"about"`
	Repo     string `json:"repo"`
	Failures []struct {
		Repeat  int    `json:"repeat"`
		Worker  string `json:"worker"`
		Code    string `json:"code"`
		Pattern string `json:"pattern"`
	} `json:"failures"`
	Want []plainforge.Task `json:"want"`
}

// Each file in testdata is a saved run: failures in, the exact task store
// expected out. The keys in those files were worked out separately.
func TestFixtures(t *testing.T) {
	files, err := filepath.Glob("testdata/*.json")
	if err != nil || len(files) == 0 {
		t.Fatalf("no fixtures: %v", err)
	}
	for _, file := range files {
		t.Run(filepath.Base(file), func(t *testing.T) {
			raw, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			var fx fixture
			if err := json.Unmarshal(raw, &fx); err != nil {
				t.Fatal(err)
			}
			forge := plainforge.NewMemory()
			opened := 0
			for _, f := range fx.Failures {
				draft := plainforge.TaskDraft{
					Key:   fingerprint.Of(f.Worker, codes.Code(f.Code), f.Pattern),
					Title: f.Worker + ": " + f.Code,
				}
				for i := 0; i < f.Repeat; i++ {
					_, created, err := OpenOrAdd(context.Background(), forge, fx.Repo, draft)
					if err != nil {
						t.Fatal(err)
					}
					if created {
						opened++
					}
				}
			}
			got := forge.Tasks()
			if !reflect.DeepEqual(got, fx.Want) {
				a, _ := json.MarshalIndent(got, "", "  ")
				b, _ := json.MarshalIndent(fx.Want, "", "  ")
				t.Fatalf("task store differs\n got: %s\nwant: %s", a, b)
			}
			if opened != len(fx.Want) {
				t.Fatalf("created was reported %d times for %d tasks", opened, len(fx.Want))
			}
		})
	}
}

func TestClosedTaskDoesNotCount(t *testing.T) {
	ctx := context.Background()
	forge := plainforge.NewMemory()
	draft := plainforge.TaskDraft{Key: "k", Title: "t"}
	first, created, err := OpenOrAdd(ctx, forge, "muster", draft)
	if err != nil || !created {
		t.Fatalf("first: created=%v err=%v", created, err)
	}
	if !forge.CloseTask("muster", first.ID) {
		t.Fatal("could not close")
	}
	second, created, err := OpenOrAdd(ctx, forge, "muster", draft)
	if err != nil || !created || second.ID == first.ID || second.Occurrences != 1 {
		t.Fatalf("a failure returning after its task closed must open a new task: %+v created=%v err=%v", second, created, err)
	}
}

func TestSameKeyInAnotherRepoIsSeparate(t *testing.T) {
	ctx := context.Background()
	forge := plainforge.NewMemory()
	draft := plainforge.TaskDraft{Key: "k", Title: "t"}
	OpenOrAdd(ctx, forge, "muster", draft)
	_, created, _ := OpenOrAdd(ctx, forge, "keelworks", draft)
	if !created || len(forge.Tasks()) != 2 {
		t.Fatal("tasks are per repository")
	}
}

func TestOpenOnce(t *testing.T) {
	ctx := context.Background()
	forge := plainforge.NewMemory()
	draft := plainforge.TaskDraft{Key: "edit-1", Title: "t"}
	for i := 0; i < 5; i++ {
		task, created, err := OpenOnce(ctx, forge, "bellbuoy", draft)
		if err != nil || created != (i == 0) || task.Occurrences != 1 {
			t.Fatalf("round %d: %+v created=%v err=%v", i, task, created, err)
		}
	}
	if len(forge.Tasks()) != 1 {
		t.Fatal("OpenOnce opened more than one task")
	}
}

func TestOutsideTroubleIsPassedOn(t *testing.T) {
	ctx := context.Background()
	forge := plainforge.NewMemory()
	forge.SetFault(codes.TimedOut("stand-in"))
	if _, created, err := OpenOrAdd(ctx, forge, "muster", plainforge.TaskDraft{Key: "k"}); err == nil || created {
		t.Fatal("a failing store must not report a task")
	}
	if _, created, err := OpenOnce(ctx, forge, "muster", plainforge.TaskDraft{Key: "k"}); err == nil || created {
		t.Fatal("a failing store must not report a task")
	}
	forge.SetFault(nil)
	if len(forge.Tasks()) != 0 {
		t.Fatal("a failed call left something behind")
	}
}
