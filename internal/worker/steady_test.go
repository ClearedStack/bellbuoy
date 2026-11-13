package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"runtime"
	"testing"

	"github.com/ClearedStack/bellbuoy/internal/codes"
	"github.com/ClearedStack/bellbuoy/internal/muster"
	"github.com/ClearedStack/bellbuoy/internal/ownership"
	"github.com/ClearedStack/bellbuoy/internal/plainforge"
	"github.com/ClearedStack/bellbuoy/internal/spool"
	"github.com/ClearedStack/bellbuoy/internal/work"
	"github.com/ClearedStack/bellbuoy/tools"
)

// endless is a Muster that makes jobs up as they are asked for and keeps
// nothing but counts, the way the real one keeps its records elsewhere.
type endless struct {
	left     int
	made     int
	answered int
	events   int
}

func (q *endless) Lease(_ context.Context, lane string) (work.Job, bool, error) {
	if lane != work.LaneNormal || q.left == 0 {
		return work.Job{}, false, nil
	}
	q.left--
	q.made++
	n := q.made
	var kind, payload string
	switch n % 4 {
	case 0:
		kind = work.KindStatus
		payload = fmt.Sprintf(`{"topic":"pill","edit":"ed-%d","state":"Building"}`, n)
	case 1:
		kind = work.KindEdit
		payload = fmt.Sprintf(`{"edit":"ed-%d","class":"ISSUE","target":"platform/a.json","approval":"cs-%d","summary":"made-up note"}`, n%50, n)
	default:
		kind = work.KindError
		payload = fmt.Sprintf(`{"worker":"muster","code":"E_TIMEOUT","pattern":"timeout after %ds on node-%d (case %c)"}`, n, n%9, 'a'+rune(n%20))
	}
	return work.Job{ID: fmt.Sprintf("job-%d", n), Lane: lane, Kind: kind, Payload: json.RawMessage(payload)}, true, nil
}
func (q *endless) Heartbeat(context.Context, string) error            { return nil }
func (q *endless) Complete(context.Context, string) error             { q.answered++; return nil }
func (q *endless) Fail(context.Context, string, codes.Code) error     { q.answered++; return nil }
func (q *endless) NeedsSubtool(context.Context, string, string) error { q.answered++; return nil }
func (q *endless) Publish(context.Context, muster.Event) error        { q.events++; return nil }

// quietForge keeps tasks, as Plainforge does, and only counts the rest.
type quietForge struct {
	*plainforge.Memory
	mirrors int
}

func (f *quietForge) SyncMirror(context.Context, string, []string) error { f.mirrors++; return nil }

type countingSender struct{ sent int }

func (s *countingSender) Send(context.Context, []byte) error { s.sent++; return nil }

// The budget for the whole process is 30 MiB. With stand-ins that do not
// pile up results, a hundred thousand jobs must leave the heap where it was,
// and everything the Go runtime has taken from the system must fit the
// budget with room to spare.
func TestSteadyStateMemory(t *testing.T) {
	owners, err := ownership.Load("../../testdata/ownership_map.json")
	if err != nil {
		t.Fatal(err)
	}
	sp, err := spool.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	queue := &endless{}
	forge := &quietForge{Memory: plainforge.NewMemory()}
	sender := &countingSender{}
	w := &Worker{
		Queue: queue,
		Tools: tools.All(tools.Deps{Queue: queue, Forge: forge, Owners: owners, Sender: sender}),
		Spool: sp,
	}
	ctx := context.Background()

	queue.left = 5000 // warm up: open the tasks that will be counted on
	w.Drain(ctx)
	var before runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)

	queue.left = 100000
	w.Drain(ctx)
	var after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&after)

	if queue.answered != 105000 {
		t.Fatalf("answered %d jobs", queue.answered)
	}
	// Jobs 2 and 3 in every 4 are failures, in ten different shapes (the
	// case letter). Job 1 in every 4 is an edit, with 25 different
	// references. Everything else about them changes from job to job.
	if n := len(forge.Tasks()); n != 10+25 {
		t.Fatalf("tasks = %d, want 10 failures and 25 edits", n)
	}
	if sender.sent != 10 {
		t.Fatalf("alerts = %d, want one per new fingerprint", sender.sent)
	}

	const growthLimit = 1 << 20
	if grown := int64(after.HeapAlloc) - int64(before.HeapAlloc); grown > growthLimit {
		t.Errorf("heap grew by %d bytes over 100000 jobs, limit %d", grown, growthLimit)
	}
	const budget = 30 << 20
	if after.Sys > budget {
		t.Errorf("the runtime holds %d bytes from the system, budget %d", after.Sys, budget)
	}
	t.Logf("heap in use %d KiB, taken from the system %d KiB, after 105000 jobs", after.HeapAlloc>>10, after.Sys>>10)
}
