package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/ClearedStack/bellbuoy/internal/codes"
	"github.com/ClearedStack/bellbuoy/internal/fixture"
	"github.com/ClearedStack/bellbuoy/internal/muster"
	"github.com/ClearedStack/bellbuoy/internal/spool"
	"github.com/ClearedStack/bellbuoy/internal/work"
	"github.com/ClearedStack/bellbuoy/tools"
)

// rig is a worker on stand-ins, with a clock the test moves by hand.
type rig struct {
	t    *testing.T
	deps fixture.Deps
	w    *Worker
	dir  string
	now  time.Time
	logs []string
}

func newRig(t *testing.T) *rig {
	t.Helper()
	deps, err := fixture.NewDeps("../../testdata/ownership_map.json")
	if err != nil {
		t.Fatal(err)
	}
	r := &rig{t: t, deps: deps, dir: t.TempDir(), now: time.Unix(1_000_000, 0)}
	r.start()
	return r
}

// start builds a fresh worker on the same stand-ins and spool directory, as
// a restart of the process would.
func (r *rig) start() {
	sp, err := spool.Open(r.dir)
	if err != nil {
		r.t.Fatal(err)
	}
	r.w = &Worker{
		Queue: r.deps.Queue,
		Tools: tools.All(tools.Deps{Queue: r.deps.Queue, Forge: r.deps.Forge, Owners: r.deps.Owners, Sender: r.deps.Alerts}),
		Spool: sp,
		Now:   func() time.Time { return r.now },
		Log:   func(line string) { r.logs = append(r.logs, line) },
	}
}

func (r *rig) add(id, lane, kind, payload string) {
	r.deps.Queue.Add(work.Job{ID: id, Lane: lane, Kind: kind, Payload: json.RawMessage(payload)})
}

func (r *rig) drain() int { return r.w.Drain(context.Background()) }

func (r *rig) wait(d time.Duration) { r.now = r.now.Add(d) }

const failure = `{"worker":"muster","code":"E_TIMEOUT","pattern":"lease store did not answer"}`

var slow = codes.TimedOut("the outside call took too long")

func verdicts(acks []muster.Ack) []string {
	var out []string
	for _, a := range acks {
		out = append(out, a.Job+"="+a.Verdict)
	}
	return out
}

func TestOneNewFailureGoesEverywhereOnce(t *testing.T) {
	r := newRig(t)
	r.add("job-1", work.LaneNormal, work.KindError, failure)
	if held := r.drain(); held != 0 {
		t.Fatalf("%d jobs left staged", held)
	}
	fx := r.deps.Collect()
	if len(fx.Tasks) != 1 || fx.Tasks[0].Occurrences != 1 || fx.Tasks[0].Repo != "muster" {
		t.Fatalf("tasks = %+v", fx.Tasks)
	}
	if len(fx.Mirrors) != 1 || len(fx.Alerts) != 1 || len(fx.Events) != 1 {
		t.Fatalf("mirrors=%d alerts=%d events=%d", len(fx.Mirrors), len(fx.Alerts), len(fx.Events))
	}
	if got := verdicts(r.deps.Queue.Acks()); !reflect.DeepEqual(got, []string{"job-1=complete"}) {
		t.Fatalf("acks = %v", got)
	}
}

func TestTwoHundredIdenticalFailuresThroughTheLoop(t *testing.T) {
	r := newRig(t)
	for i := 0; i < 200; i++ {
		r.add(fmt.Sprintf("job-%03d", i), work.LaneNormal, work.KindError, failure)
	}
	r.drain()
	fx := r.deps.Collect()
	if len(fx.Tasks) != 1 || fx.Tasks[0].Occurrences != 200 {
		t.Fatalf("tasks = %+v", fx.Tasks)
	}
	if len(fx.Alerts) != 1 {
		t.Fatalf("the owner was alerted %d times, want once", len(fx.Alerts))
	}
	if len(fx.Events) != 200 || fx.Events[199].Occurrences != 200 {
		t.Fatalf("events = %d", len(fx.Events))
	}
	if len(r.deps.Queue.Acks()) != 200 || r.deps.Queue.Leased() != 0 {
		t.Fatal("not every job was answered")
	}
}

func TestLanesAreDrainedInOrder(t *testing.T) {
	r := newRig(t)
	for i := 0; i < 3; i++ {
		r.add(fmt.Sprintf("b-%d", i), work.LaneBackfill, work.KindError, failure)
		r.add(fmt.Sprintf("n-%d", i), work.LaneNormal, work.KindError, failure)
		r.add(fmt.Sprintf("u-%d", i), work.LaneUrgent, work.KindError, failure)
	}
	ctx := context.Background()
	// Work through all of urgent and one normal job, then have new urgent
	// work arrive. It must be taken before the normal lane is touched again.
	for i := 0; i < 4; i++ {
		r.w.Step(ctx)
	}
	r.add("u-late", work.LaneUrgent, work.KindError, failure)
	r.drain()
	want := []string{"urgent", "urgent", "urgent", "normal", "urgent", "normal", "normal", "backfill", "backfill", "backfill"}
	if got := r.deps.Queue.LeaseOrder(); !reflect.DeepEqual(got, want) {
		t.Fatalf("lease order = %v\nwant %v", got, want)
	}
}

func TestOneBadJobDoesNotStopTheRest(t *testing.T) {
	r := newRig(t)
	r.add("job-1", work.LaneNormal, work.KindError, failure)
	r.add("job-2", work.LaneNormal, "census", `{"count":1}`)                                 // no rule
	r.add("job-3", work.LaneNormal, work.KindError, `{"worker":"muster","code":"E_TIMEOUT"`) // broken
	r.add("job-4", work.LaneNormal, work.KindEdit, `{"edit":"ed-1","class":"LAYOUT","target":"platform/a.json","approval":"cs-1","content":"x"}`)
	r.add("job-5", work.LaneNormal, work.KindError, failure)
	r.drain()
	want := []muster.Ack{
		{Job: "job-1", Verdict: "complete"},
		{Job: "job-2", Verdict: "needs_subtool", Code: "E_NEEDS_SUBTOOL", Reason: "no rule covers this kind of job"},
		{Job: "job-3", Verdict: "fail", Code: "E_PARSE"},
		{Job: "job-4", Verdict: "needs_subtool", Code: "E_NEEDS_SUBTOOL", Reason: "no rule covers this class of edit"},
		{Job: "job-5", Verdict: "complete"},
	}
	if got := r.deps.Queue.Acks(); !reflect.DeepEqual(got, want) {
		t.Fatalf("acks = %+v", got)
	}
	fx := r.deps.Collect()
	if len(fx.Tasks) != 1 || fx.Tasks[0].Occurrences != 2 || len(fx.Changes) != 0 {
		t.Fatalf("the refused jobs left something behind: %+v", fx)
	}
}

func TestBadEnvelopeIsFailedWithEParse(t *testing.T) {
	r := newRig(t)
	r.deps.Queue.Add(work.Job{ID: "job-1", Lane: work.LaneNormal, Kind: "Not A Kind", Payload: json.RawMessage(failure)})
	r.deps.Queue.AddTo(work.LaneNormal, work.Job{ID: "job-2", Lane: "soon", Kind: work.KindError, Payload: json.RawMessage(failure)})
	r.drain()
	want := []muster.Ack{{Job: "job-1", Verdict: "fail", Code: "E_PARSE"}, {Job: "job-2", Verdict: "fail", Code: "E_PARSE"}}
	if got := r.deps.Queue.Acks(); !reflect.DeepEqual(got, want) {
		t.Fatalf("acks = %+v", got)
	}
	if len(r.deps.Forge.Tasks()) != 0 {
		t.Fatal("a job with a bad envelope reached a sub-tool")
	}
}

func TestWebhookTimeoutIsStagedAndRetriedUntilDelivered(t *testing.T) {
	r := newRig(t)
	r.deps.Alerts.SetFault(slow)
	r.add("job-1", work.LaneNormal, work.KindError, failure)
	if held := r.drain(); held != 1 {
		t.Fatalf("held = %d, want the job staged", held)
	}
	// The job is not answered yet, and the other sub-tools did not wait.
	if len(r.deps.Queue.Acks()) != 0 || r.deps.Queue.Leased() != 1 {
		t.Fatal("a job with an undelivered alert must stay leased")
	}
	fx := r.deps.Collect()
	if len(fx.Tasks) != 1 || len(fx.Mirrors) != 1 || len(fx.Events) != 1 || len(fx.Alerts) != 0 {
		t.Fatalf("effects after the first try: %+v", fx)
	}

	// Still down: retried on the schedule, lease kept alive, nothing lost.
	for i := 0; i < 5; i++ {
		r.wait(5 * time.Minute)
		r.drain()
	}
	if r.deps.Queue.Beats("job-1") == 0 {
		t.Fatal("the lease of a staged job was not kept alive")
	}
	if len(r.deps.Queue.Acks()) != 0 {
		t.Fatal("job answered before its alert was delivered")
	}

	// Back up: delivered once, answered, spool empty.
	r.deps.Alerts.SetFault(nil)
	r.wait(5 * time.Minute)
	if held := r.drain(); held != 0 {
		t.Fatalf("held = %d after recovery", held)
	}
	fx = r.deps.Collect()
	if len(fx.Alerts) != 1 {
		t.Fatalf("alerts = %d, want 1", len(fx.Alerts))
	}
	if len(fx.Tasks) != 1 || fx.Tasks[0].Occurrences != 1 || len(fx.Mirrors) != 1 || len(fx.Events) != 1 {
		t.Fatalf("a sub-tool that had finished was run again: %+v", fx)
	}
	if got := verdicts(r.deps.Queue.Acks()); !reflect.DeepEqual(got, []string{"job-1=complete"}) {
		t.Fatalf("acks = %v", got)
	}
}

func TestPlainforgeTimeoutHoldsEverythingBehindIt(t *testing.T) {
	r := newRig(t)
	r.deps.Forge.SetFault(slow)
	r.add("job-1", work.LaneNormal, work.KindError, failure)
	if held := r.drain(); held != 1 {
		t.Fatalf("held = %d", held)
	}
	fx := r.deps.Collect()
	if len(fx.Alerts) != 0 || len(fx.Events) != 0 {
		t.Fatalf("something was announced before the task existed: %+v", fx)
	}
	r.deps.Forge.SetFault(nil)
	r.wait(time.Second)
	if held := r.drain(); held != 0 {
		t.Fatalf("held = %d after recovery", held)
	}
	fx = r.deps.Collect()
	if len(fx.Tasks) != 1 || fx.Tasks[0].Occurrences != 1 || len(fx.Alerts) != 1 || len(fx.Events) != 1 || len(fx.Mirrors) != 1 {
		t.Fatalf("effects after recovery: %+v", fx)
	}
}

func TestRetriesFollowTheBackoffSchedule(t *testing.T) {
	r := newRig(t)
	calls := 0
	r.w.Tools = []work.Tool{&countingTool{calls: &calls}}
	r.add("job-1", work.LaneNormal, work.KindDrift, `{}`)
	r.drain()
	if calls != 1 {
		t.Fatalf("calls = %d", calls)
	}
	// After n tries without progress the next comes spool.Backoff(n) later,
	// and not a second sooner.
	for n := 1; n <= 10; n++ {
		delay := spool.Backoff(n)
		r.wait(delay - time.Second)
		r.drain()
		if calls != n {
			t.Fatalf("after %d tries: retried %v early", n, time.Second)
		}
		r.wait(time.Second)
		r.drain()
		if calls != n+1 {
			t.Fatalf("after %d tries: no retry %v later (calls = %d)", n, delay, calls)
		}
	}
}

type countingTool struct{ calls *int }

func (*countingTool) Name() string                    { return "counting" }
func (*countingTool) Gate() bool                      { return false }
func (*countingTool) Match(work.Job, work.State) bool { return true }
func (c *countingTool) Run(context.Context, work.Job, *work.State) error {
	*c.calls++
	return slow
}

func TestAnswerSurvivesMusterBeingDown(t *testing.T) {
	r := newRig(t)
	r.add("job-1", work.LaneNormal, work.KindError, failure)
	ctx := context.Background()
	// Muster goes down between handing out the job and hearing the answer.
	job, _, _ := r.deps.Queue.Lease(ctx, work.LaneNormal)
	r.deps.Queue.SetFault(slow)
	r.w.advance(ctx, &spool.Record{Job: job}, false)
	if r.w.Spool.Len() != 1 {
		t.Fatal("the answer was not staged")
	}
	fx := r.deps.Collect()
	if len(fx.Tasks) != 1 || len(fx.Alerts) != 1 {
		t.Fatalf("effects = %+v", fx)
	}
	// The event for the live stream also goes through Muster, so it waits.
	if len(fx.Events) != 0 {
		t.Fatal("an event got through a Muster that is down")
	}

	r.deps.Queue.SetFault(nil)
	r.wait(time.Minute)
	if held := r.drain(); held != 0 {
		t.Fatalf("held = %d", held)
	}
	fx = r.deps.Collect()
	if fx.Tasks[0].Occurrences != 1 || len(fx.Alerts) != 1 || len(fx.Events) != 1 {
		t.Fatalf("work was repeated while only the answer was outstanding: %+v", fx)
	}
	if got := verdicts(r.deps.Queue.Acks()); !reflect.DeepEqual(got, []string{"job-1=complete"}) {
		t.Fatalf("acks = %v", got)
	}
}

func TestStagedWorkSurvivesARestart(t *testing.T) {
	r := newRig(t)
	r.deps.Alerts.SetFault(slow)
	r.add("job-1", work.LaneNormal, work.KindError, failure)
	r.drain()

	r.start() // the process restarts and reads its spool
	if r.w.Spool.Len() != 1 {
		t.Fatal("the staged job did not survive the restart")
	}
	r.deps.Alerts.SetFault(nil)
	r.wait(time.Minute)
	if held := r.drain(); held != 0 {
		t.Fatalf("held = %d", held)
	}
	fx := r.deps.Collect()
	if len(fx.Tasks) != 1 || fx.Tasks[0].Occurrences != 1 || len(fx.Alerts) != 1 || len(fx.Events) != 1 {
		t.Fatalf("effects after restart: %+v", fx)
	}
	if len(r.deps.Queue.Acks()) != 1 {
		t.Fatal("job was not answered after the restart")
	}
}

func TestAJobHandedOutAgainCarriesOnFromItsRecord(t *testing.T) {
	r := newRig(t)
	r.deps.Alerts.SetFault(slow)
	r.add("job-1", work.LaneNormal, work.KindError, failure)
	r.drain()
	// The lease runs out and Muster hands the same job out again.
	if !r.deps.Queue.Expire("job-1") {
		t.Fatal("no lease to expire")
	}
	r.deps.Alerts.SetFault(nil)
	if held := r.drain(); held != 0 {
		t.Fatalf("held = %d", held)
	}
	fx := r.deps.Collect()
	if len(fx.Tasks) != 1 || fx.Tasks[0].Occurrences != 1 {
		t.Fatalf("the failure was counted twice: %+v", fx.Tasks)
	}
	if len(fx.Alerts) != 1 || len(fx.Events) != 1 {
		t.Fatalf("alerts=%d events=%d", len(fx.Alerts), len(fx.Events))
	}
}

func TestNoNewLeasesWhileTheSpoolIsFull(t *testing.T) {
	r := newRig(t)
	r.w.MaxHeld = 2
	r.deps.Alerts.SetFault(slow)
	for i := 0; i < 5; i++ {
		// A different worker each time, so each is a new fingerprint and
		// each wants an alert.
		w := []string{"muster", "keelworks", "bellbuoy", "shipkeeper", "muster"}[i]
		r.add(fmt.Sprintf("job-%d", i), work.LaneNormal, work.KindError,
			fmt.Sprintf(`{"worker":%q,"code":"E_PARSE","pattern":"case %s"}`, w, strings.Repeat("x", i+1)))
	}
	if held := r.drain(); held != 2 {
		t.Fatalf("held = %d, want 2", held)
	}
	if r.deps.Queue.Waiting() != 3 {
		t.Fatalf("%d jobs left in Muster, want 3", r.deps.Queue.Waiting())
	}
	r.deps.Alerts.SetFault(nil)
	r.wait(time.Minute)
	if held := r.drain(); held != 0 {
		t.Fatalf("held = %d after recovery", held)
	}
	if len(r.deps.Queue.Acks()) != 5 {
		t.Fatalf("acks = %d", len(r.deps.Queue.Acks()))
	}
}

// Bellbuoy's own refusals come back to it as failures of the worker
// "bellbuoy". That must settle into one task per kind of refusal and stop.
func TestOwnFailuresDoNotLoop(t *testing.T) {
	r := newRig(t)
	r.deps.Queue.SetFeedback("bellbuoy")
	for i := 0; i < 50; i++ {
		r.add(fmt.Sprintf("bad-%d", i), work.LaneNormal, work.KindError, `{"worker":`)
		r.add(fmt.Sprintf("odd-%d", i), work.LaneNormal, "census", `{}`)
	}
	ctx := context.Background()
	steps := 0
	for r.w.Step(ctx) {
		if steps++; steps > 1000 {
			t.Fatal("the queue never drained")
		}
	}
	if r.deps.Queue.Waiting() != 0 || r.deps.Queue.Leased() != 0 {
		t.Fatal("work left over")
	}
	// 100 refused jobs, and 100 reports of them, which all succeed.
	if got := len(r.deps.Queue.Acks()); got != 200 {
		t.Fatalf("acks = %d, want 200", got)
	}
	tasks := r.deps.Forge.Tasks()
	if len(tasks) != 2 {
		t.Fatalf("tasks = %d, want 2", len(tasks))
	}
	for _, task := range tasks {
		if task.Repo != "bellbuoy" || task.Occurrences != 50 {
			t.Fatalf("task = %+v", task)
		}
	}
	if got := len(r.deps.Alerts.Sent()); got != 2 {
		t.Fatalf("alerts = %d, want 2", got)
	}
}

// Whatever a job carries, a log line holds only checked references, lanes,
// kinds, public codes and fixed words.
func TestLogsHoldNothingFromOutside(t *testing.T) {
	r := newRig(t)
	r.add("job-1", work.LaneNormal, work.KindError,
		`{"worker":"muster","code":"E_TIMEOUT","pattern":"dial tcp 10.0.0.7:5432 on node-3 password=made-up-secret"}`)
	r.add("job-2", work.LaneNormal, work.KindError, `{"worker":"node-3.internal.example","code":"E_TIMEOUT","pattern":"x"}`)
	r.deps.Queue.Add(work.Job{ID: "id with 10.0.0.9 inside", Lane: work.LaneNormal, Kind: "kind on node-3", Payload: json.RawMessage(`{}`)})
	r.deps.Queue.AddTo(work.LaneNormal, work.Job{ID: "job-4", Lane: "10.0.0.8", Kind: work.KindError, Payload: json.RawMessage(`{}`)})
	r.deps.Alerts.SetFault(slow)
	r.drain()
	if len(r.logs) < 4 {
		t.Fatalf("only %d log lines", len(r.logs))
	}
	for _, line := range r.logs {
		for _, leak := range []string{"10.0.0", "node-3", "5432", "made-up-secret", "password", "internal.example", "dial"} {
			if strings.Contains(line, leak) {
				t.Fatalf("log line leaks %q: %s", leak, line)
			}
		}
	}
}

func TestRunStopsWhenAskedAndStagesWhatItHolds(t *testing.T) {
	r := newRig(t)
	r.w.Now = nil // real clock
	r.w.Idle = 5 * time.Millisecond
	r.deps.Alerts.SetFault(slow)
	r.add("job-1", work.LaneNormal, work.KindError, failure)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.w.Run(ctx); close(done) }()
	deadline := time.Now().Add(5 * time.Second)
	for r.w.Spool.Len() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop")
	}
	if r.w.Spool.Len() != 1 {
		t.Fatal("the job in hand was not staged")
	}
}

// The budget is 30 MiB for the whole process. The loop itself must stay far
// below that however many jobs pass through.
func TestHeapStaysSmall(t *testing.T) {
	r := newRig(t)
	r.w.Log = nil
	var before runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	for round := 0; round < 20; round++ {
		for i := 0; i < 500; i++ {
			r.add(fmt.Sprintf("job-%d-%d", round, i), work.LaneNormal, work.KindError,
				fmt.Sprintf(`{"worker":"muster","code":"E_TIMEOUT","pattern":"timeout after %ds on node-%d"}`, i, round))
		}
		r.drain()
	}
	var after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&after)
	if tasks := r.deps.Forge.Tasks(); len(tasks) != 1 || tasks[0].Occurrences != 10000 {
		t.Fatalf("tasks = %+v", tasks)
	}
	// The stand-ins keep every answer, event and mirror request for the
	// test to read, which is most of what is counted here.
	const limit = 8 << 20
	if grown := int64(after.HeapAlloc) - int64(before.HeapAlloc); grown > limit {
		t.Fatalf("heap grew by %d bytes over 10000 jobs, limit %d", grown, limit)
	}
}
