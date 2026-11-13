package route

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/ClearedStack/bellbuoy/internal/codes"
	"github.com/ClearedStack/bellbuoy/internal/work"
)

// fake is a sub-tool whose behaviour is set by the test.
type fake struct {
	name  string
	gate  bool
	match func(work.State) bool
	err   error
	runs  int
	mark  bool // set st.Task on success
}

func (f *fake) Name() string { return f.name }
func (f *fake) Gate() bool   { return f.gate }
func (f *fake) Match(_ work.Job, st work.State) bool {
	if f.match == nil {
		return true
	}
	return f.match(st)
}
func (f *fake) Run(_ context.Context, _ work.Job, st *work.State) error {
	f.runs++
	if f.err != nil {
		return f.err
	}
	if f.mark {
		st.Task = &work.TaskResult{ID: "t"}
	}
	return nil
}

func never(work.State) bool      { return false }
func hasTask(st work.State) bool { return st.Task != nil }

func keys(m map[string]bool) []string {
	var out []string
	for _, k := range []string{"a", "b", "c"} {
		if m[k] {
			out = append(out, k)
		}
	}
	return out
}

func run(tools []work.Tool, done map[string]bool) (Outcome, work.State) {
	var st work.State
	out := Run(context.Background(), tools, work.Job{ID: "j"}, &st, done, 0)
	return out, st
}

func TestAllMatchingToolsRunInOrder(t *testing.T) {
	a, b, c := &fake{name: "a", gate: true, mark: true}, &fake{name: "b", match: hasTask}, &fake{name: "c", match: never}
	done := map[string]bool{}
	out, _ := run([]work.Tool{a, b, c}, done)
	if out.Waiting || out.Fault != nil || !out.Progress {
		t.Fatalf("outcome = %+v", out)
	}
	if a.runs != 1 || b.runs != 1 || c.runs != 0 {
		t.Fatalf("runs = %d %d %d", a.runs, b.runs, c.runs)
	}
	if !reflect.DeepEqual(keys(done), []string{"a", "b"}) {
		t.Fatalf("done = %v", keys(done))
	}
}

func TestNoMatchIsNeedsSubtool(t *testing.T) {
	a := &fake{name: "a", match: never}
	out, _ := run([]work.Tool{a}, map[string]bool{})
	if out.Fault == nil || out.Fault.Code != codes.NeedsSubtool || out.Waiting {
		t.Fatalf("outcome = %+v", out)
	}
	if a.runs != 0 {
		t.Fatal("a sub-tool ran without matching")
	}
}

func TestTimeoutInAGateHoldsTheRest(t *testing.T) {
	a, b := &fake{name: "a", gate: true, err: codes.TimedOut("slow")}, &fake{name: "b"}
	done := map[string]bool{}
	out, _ := run([]work.Tool{a, b}, done)
	if !out.Waiting || out.Fault != nil || out.Progress {
		t.Fatalf("outcome = %+v", out)
	}
	if b.runs != 0 || len(done) != 0 {
		t.Fatal("a later sub-tool ran while the gate was waiting")
	}
}

func TestTimeoutElsewhereDoesNotHoldTheRest(t *testing.T) {
	a, b, c := &fake{name: "a", gate: true}, &fake{name: "b", err: codes.TimedOut("slow")}, &fake{name: "c"}
	done := map[string]bool{}
	out, _ := run([]work.Tool{a, b, c}, done)
	if !out.Waiting || out.Fault != nil || !out.Progress {
		t.Fatalf("outcome = %+v", out)
	}
	if c.runs != 1 || !reflect.DeepEqual(keys(done), []string{"a", "c"}) {
		t.Fatalf("done = %v", keys(done))
	}

	// The next run tries only the one that was waiting.
	b.err = nil
	out, _ = run([]work.Tool{a, b, c}, done)
	if out.Waiting || out.Fault != nil {
		t.Fatalf("second outcome = %+v", out)
	}
	if a.runs != 1 || b.runs != 2 || c.runs != 1 {
		t.Fatalf("runs = %d %d %d", a.runs, b.runs, c.runs)
	}
}

func TestFaultInAGateStopsEverything(t *testing.T) {
	a, b := &fake{name: "a", gate: true, err: codes.Broken("bad")}, &fake{name: "b"}
	done := map[string]bool{}
	out, _ := run([]work.Tool{a, b}, done)
	if out.Fault == nil || out.Fault.Code != codes.Parse || out.Waiting {
		t.Fatalf("outcome = %+v", out)
	}
	if b.runs != 0 {
		t.Fatal("a later sub-tool ran after the gate refused the job")
	}
}

func TestFirstFaultIsKeptAndTheRestStillRun(t *testing.T) {
	a := &fake{name: "a", gate: true}
	b := &fake{name: "b", err: codes.Broken("first")}
	c := &fake{name: "c", err: codes.Unfamiliar("second")}
	done := map[string]bool{}
	out, _ := run([]work.Tool{a, b, c}, done)
	if out.Fault == nil || out.Fault.Reason != "first" {
		t.Fatalf("fault = %+v", out.Fault)
	}
	if c.runs != 1 || !reflect.DeepEqual(keys(done), []string{"a", "b", "c"}) {
		t.Fatalf("c.runs = %d, done = %v", c.runs, keys(done))
	}
}

// An error that carries no public code must not leak its text.
func TestForeignErrorsBecomePublicCodes(t *testing.T) {
	a := &fake{name: "a", err: errors.New("connect to store-0.internal.example failed")}
	out, _ := run([]work.Tool{a}, map[string]bool{})
	if out.Fault == nil || out.Fault.Code != codes.Parse || out.Fault.Reason != "the outside answer was not understood" {
		t.Fatalf("fault = %+v", out.Fault)
	}
}

type slow struct{ fake }

func (s *slow) Run(ctx context.Context, _ work.Job, _ *work.State) error {
	<-ctx.Done()
	return ctx.Err()
}

func TestPerToolLimitGivesATimeout(t *testing.T) {
	s := &slow{fake{name: "a"}}
	var st work.State
	out := Run(context.Background(), []work.Tool{s}, work.Job{ID: "j"}, &st, map[string]bool{}, 20*time.Millisecond)
	if !out.Waiting || out.Fault != nil {
		t.Fatalf("outcome = %+v", out)
	}
}
