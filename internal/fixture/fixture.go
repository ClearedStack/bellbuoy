// Package fixture runs the saved examples that every sub-tool ships with.
//
// A fixture is one JSON file: a job, the state left by earlier sub-tools,
// and the exact result expected. The result covers whether the matching rule
// matched, the error if any, the state afterwards, and everything the
// sub-tool did to the stand-ins for Muster, Plainforge and the webhook. The
// comparison is byte for byte, so any change in behaviour fails a test.
//
// Run "go test ./tools/... -update" to rewrite the expected results after a
// deliberate change, then read the difference before committing it.
package fixture

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"

	"github.com/ClearedStack/bellbuoy/internal/codes"
	"github.com/ClearedStack/bellbuoy/internal/plainforge"
	"github.com/ClearedStack/bellbuoy/internal/standin"
	"github.com/ClearedStack/bellbuoy/internal/work"
)

var update = flag.Bool("update", false, "rewrite the expected results in fixture files")

// Deps are the stand-ins a sub-tool is built on for a fixture run.
type Deps = standin.Set

// NewDeps returns fresh stand-ins and the ownership map at the given path.
func NewDeps(ownersPath string) (Deps, error) { return standin.New(ownersPath) }

// Result is the exact outcome a fixture expects.
type Result struct {
	Match   bool            `json:"match"`
	Error   *codes.Error    `json:"error,omitempty"`
	State   work.State      `json:"state"`
	Effects standin.Effects `json:"effects"`
}

// Case is one fixture file.
type Case struct {
	// About says in plain words what the example shows.
	About string `json:"about"`
	// Job is the saved input.
	Job work.Job `json:"job"`
	// Repeat delivers the same job this many times. Zero means once.
	Repeat int `json:"repeat,omitempty"`
	// Fault, when set to E_TIMEOUT or E_PARSE, makes every call outside
	// fail that way.
	Fault string `json:"fault,omitempty"`
	// Seed is the tasks already in Plainforge before the run.
	Seed []plainforge.Task `json:"seed,omitempty"`
	// State is what earlier sub-tools left behind.
	State work.State `json:"state"`
	// Want is the exact result expected.
	Want json.RawMessage `json:"want"`
}

// Encode writes v as indented JSON without escaping <, > or &.
func Encode(v any) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		panic(err)
	}
	return buf.Bytes()
}

// Play runs one case against a sub-tool built on fresh stand-ins.
func Play(c Case, ownersPath string, build func(Deps) work.Tool) (Result, error) {
	deps, err := NewDeps(ownersPath)
	if err != nil {
		return Result{}, err
	}
	deps.Forge.Seed(c.Seed)
	switch c.Fault {
	case "":
	case string(codes.Timeout):
		deps.SetFault(codes.TimedOut("the outside call took too long"))
	case string(codes.Parse):
		deps.SetFault(codes.Broken("the outside answer was not understood"))
	default:
		return Result{}, codes.Broken("fixture names a fault that does not exist")
	}
	tool := build(deps)
	st := c.State
	res := Result{Match: tool.Match(c.Job, st)}
	if res.Match {
		repeat := c.Repeat
		if repeat < 1 {
			repeat = 1
		}
		for i := 0; i < repeat; i++ {
			if err := tool.Run(context.Background(), c.Job, &st); err != nil {
				res.Error = codes.From(err)
				break
			}
		}
	}
	res.State = st
	res.Effects = deps.Collect()
	return res, nil
}

// Run plays every fixture in dir against the sub-tool and compares the
// result with the one saved in the file, byte for byte.
func Run(t *testing.T, dir, ownersPath string, build func(Deps) work.Tool) {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatalf("no fixtures in %s", dir)
	}
	for _, file := range files {
		t.Run(filepath.Base(file), func(t *testing.T) {
			raw, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			var c Case
			dec := json.NewDecoder(bytes.NewReader(raw))
			dec.DisallowUnknownFields()
			if err := dec.Decode(&c); err != nil {
				t.Fatalf("fixture is not readable: %v", err)
			}
			if c.About == "" {
				t.Fatal("fixture does not say what it shows")
			}
			res, err := Play(c, ownersPath, build)
			if err != nil {
				t.Fatal(err)
			}
			got := Encode(res)
			if *update {
				c.Want = json.RawMessage(bytes.TrimSpace(got))
				if err := os.WriteFile(file, Encode(c), 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			var want bytes.Buffer
			if err := json.Indent(&want, c.Want, "", "  "); err != nil {
				t.Fatalf("fixture has no readable expected result: %v", err)
			}
			want.WriteByte('\n')
			if !bytes.Equal(got, want.Bytes()) {
				t.Fatalf("result differs from the saved example\n--- got\n%s--- want\n%s", got, want.Bytes())
			}
		})
	}
}
