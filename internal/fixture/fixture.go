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
	"sync"
	"testing"

	"github.com/ClearedStack/bellbuoy/internal/codes"
	"github.com/ClearedStack/bellbuoy/internal/muster"
	"github.com/ClearedStack/bellbuoy/internal/ownership"
	"github.com/ClearedStack/bellbuoy/internal/plainforge"
	"github.com/ClearedStack/bellbuoy/internal/work"
)

var update = flag.Bool("update", false, "rewrite the expected results in fixture files")

// Recorder is a stand-in for the owner's webhook. It keeps what it is sent.
type Recorder struct {
	mu    sync.Mutex
	sent  []json.RawMessage
	fault error
}

// Send keeps the body, or fails if a fault is set.
func (r *Recorder) Send(_ context.Context, body []byte) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fault != nil {
		return r.fault
	}
	r.sent = append(r.sent, append(json.RawMessage(nil), body...))
	return nil
}

// SetFault makes every later Send fail with err. A nil err clears it.
func (r *Recorder) SetFault(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.fault = err
}

// Sent returns every body received, in order.
func (r *Recorder) Sent() []json.RawMessage {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]json.RawMessage(nil), r.sent...)
}

// Deps are the stand-ins a sub-tool is built on for a fixture run.
type Deps struct {
	Forge  *plainforge.Memory
	Queue  *muster.Memory
	Owners *ownership.Map
	Alerts *Recorder
}

// NewDeps returns fresh stand-ins and the ownership map at the given path.
func NewDeps(ownersPath string) (Deps, error) {
	owners, err := ownership.Load(ownersPath)
	if err != nil {
		return Deps{}, err
	}
	return Deps{Forge: plainforge.NewMemory(), Queue: muster.NewMemory(), Owners: owners, Alerts: &Recorder{}}, nil
}

// Effects is everything the stand-ins hold after a run.
type Effects struct {
	Tasks   []plainforge.Task          `json:"tasks,omitempty"`
	Changes []plainforge.Change        `json:"changes,omitempty"`
	Mirrors []plainforge.MirrorRequest `json:"mirrors,omitempty"`
	Events  []muster.Event             `json:"events,omitempty"`
	Alerts  []json.RawMessage          `json:"alerts,omitempty"`
}

// Collect reads the effects out of the stand-ins.
func (d Deps) Collect() Effects {
	return Effects{
		Tasks:   d.Forge.Tasks(),
		Changes: d.Forge.Changes(),
		Mirrors: d.Forge.Mirrors(),
		Events:  d.Queue.Events(),
		Alerts:  d.Alerts.Sent(),
	}
}

// Result is the exact outcome a fixture expects.
type Result struct {
	Match   bool         `json:"match"`
	Error   *codes.Error `json:"error,omitempty"`
	State   work.State   `json:"state"`
	Effects Effects      `json:"effects"`
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
		fault := codes.TimedOut("the outside call took too long")
		deps.Forge.SetFault(fault)
		deps.Queue.SetFault(fault)
		deps.Alerts.SetFault(fault)
	case string(codes.Parse):
		fault := codes.Broken("the outside answer was not understood")
		deps.Forge.SetFault(fault)
		deps.Queue.SetFault(fault)
		deps.Alerts.SetFault(fault)
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
