package internalgit_test

import (
	"bytes"
	"context"
	"flag"
	"os"
	"path/filepath"
	"testing"

	"github.com/clearedstack/bellbuoy/internal/replay"
)

var update = flag.Bool("update", false, "rewrite the expected.json of every fixture from the current behaviour")

// TestFixtures replays every saved input and compares the result, byte for
// byte, with the saved expected output.
func TestFixtures(t *testing.T) {
	dirs, err := filepath.Glob(filepath.Join("fixtures", "*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(dirs) == 0 {
		t.Fatal("no fixtures found")
	}
	for _, dir := range dirs {
		t.Run(filepath.Base(dir), func(t *testing.T) {
			got := replayFile(t, filepath.Join(dir, "input.jsonl"))
			wantPath := filepath.Join(dir, "expected.json")
			if *update {
				if err := os.WriteFile(wantPath, got, 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(wantPath)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, want) {
				t.Errorf("output differs from %s\n--- got ---\n%s", wantPath, got)
			}
		})
	}
}

// TestTwoHundredIdenticalFailuresBecomeOneTask states the headline fact of
// the milestone in code, so that regenerating the fixture cannot hide a
// change to it.
func TestTwoHundredIdenticalFailuresBecomeOneTask(t *testing.T) {
	path := filepath.Join("fixtures", "two-hundred-identical", "input.jsonl")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := bytes.Split(bytes.TrimSuffix(raw, []byte("\n")), []byte("\n"))
	if len(lines) != 200 {
		t.Fatalf("fixture has %d lines, want 200", len(lines))
	}
	for i, l := range lines {
		if !bytes.Equal(l, lines[0]) {
			t.Fatalf("line %d differs from line 1", i+1)
		}
	}

	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	rep, err := replay.Run(context.Background(), f)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Outcomes) != 200 {
		t.Fatalf("%d outcomes, want 200", len(rep.Outcomes))
	}
	if rep.Outcomes[0].Action != "opened" {
		t.Errorf("first outcome is %q, want opened", rep.Outcomes[0].Action)
	}
	for i, o := range rep.Outcomes[1:] {
		if o.Action != "added" || o.Task != 1 || o.Occurrences != i+2 {
			t.Fatalf("outcome %d is %+v, want added to task 1 with %d occurrences", i+2, o, i+2)
		}
	}
	if len(rep.Tasks) != 1 {
		t.Fatalf("%d tasks, want 1", len(rep.Tasks))
	}
	if got := rep.Tasks[0]; got.Occurrences != 200 || !got.Open {
		t.Errorf("task is %+v, want open with 200 occurrences", got)
	}
}

func replayFile(t *testing.T, path string) []byte {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	rep, err := replay.Run(context.Background(), f)
	if err != nil {
		t.Fatal(err)
	}
	out, err := rep.JSON()
	if err != nil {
		t.Fatal(err)
	}
	return out
}
