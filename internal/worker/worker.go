// Package worker is the loop that leases jobs from Muster, runs them through
// the sub-tools, and tells Muster how each one ended.
//
// One job is worked on at a time, by one goroutine. That keeps the order of
// everything Bellbuoy does a function of the order of its input, and makes
// the open-or-add-to rule safe without a lock in Plainforge.
package worker

import (
	"context"
	"time"

	"github.com/ClearedStack/bellbuoy/internal/codes"
	"github.com/ClearedStack/bellbuoy/internal/muster"
	"github.com/ClearedStack/bellbuoy/internal/route"
	"github.com/ClearedStack/bellbuoy/internal/sanitize"
	"github.com/ClearedStack/bellbuoy/internal/spool"
	"github.com/ClearedStack/bellbuoy/internal/work"
)

// Defaults for the fields of Worker left at zero.
const (
	DefaultHeartbeat = 15 * time.Second
	DefaultIdle      = 2 * time.Second
	DefaultPerTool   = 20 * time.Second
	DefaultMaxHeld   = 256
)

// Worker is the lease loop.
type Worker struct {
	Queue muster.Queue
	Tools []work.Tool
	Spool *spool.Spool

	// Now is the clock. Nil means time.Now.
	Now func() time.Time
	// Log receives one line per event worth recording. Lines hold job
	// references, lanes, kinds, sub-tool names, public codes and fixed
	// reasons, and nothing else. Nil means no logging.
	Log func(line string)
	// Heartbeat is how often the leases of staged jobs are kept alive.
	Heartbeat time.Duration
	// Idle is how long Run sleeps when there is nothing to do.
	Idle time.Duration
	// PerTool is how long one sub-tool may take on one job.
	PerTool time.Duration
	// MaxHeld is how many jobs may be staged at once. At the limit no new
	// job is leased, which bounds both memory and disk.
	MaxHeld int

	lastBeat time.Time
}

func (w *Worker) now() time.Time {
	if w.Now != nil {
		return w.Now()
	}
	return time.Now()
}

func (w *Worker) logf(parts ...string) {
	if w.Log == nil {
		return
	}
	line := "bellbuoy"
	for _, p := range parts {
		line += " " + p
	}
	w.Log(line)
}

func orDefault[T comparable](v, def T) T {
	var zero T
	if v == zero {
		return def
	}
	return v
}

// Run works until ctx is done. Jobs in hand when it stops are staged in the
// spool, not lost.
func (w *Worker) Run(ctx context.Context) {
	idle := orDefault(w.Idle, DefaultIdle)
	for ctx.Err() == nil {
		if w.Step(ctx) {
			continue
		}
		t := time.NewTimer(idle)
		select {
		case <-ctx.Done():
		case <-t.C:
		}
		t.Stop()
	}
}

// Drain calls Step until there is nothing left to do right now, and returns
// how many jobs are still staged, waiting for a later retry.
func (w *Worker) Drain(ctx context.Context) int {
	for ctx.Err() == nil && w.Step(ctx) {
	}
	return w.Spool.Len()
}

// Step does one round and reports whether it found anything to do:
//
//  1. keep the leases of staged jobs alive, if a heartbeat is due;
//  2. try again every staged job whose retry time has come, oldest first;
//  3. lease one new job, taking from urgent before normal and from normal
//     before backfill, and work on it.
func (w *Worker) Step(ctx context.Context) bool {
	now := w.now()
	if now.Sub(w.lastBeat) >= orDefault(w.Heartbeat, DefaultHeartbeat) {
		w.lastBeat = now
		for _, id := range w.Spool.Unfinished() {
			if err := w.Queue.Heartbeat(ctx, id); err != nil {
				w.logf("job="+ref(id), "heartbeat=missed", "code="+string(codes.From(err).Code))
			}
		}
	}

	busy := false
	for _, id := range w.Spool.Due(now) {
		if ctx.Err() != nil {
			return busy
		}
		w.resume(ctx, id)
		busy = true
	}

	if w.Spool.Len() >= orDefault(w.MaxHeld, DefaultMaxHeld) {
		return busy
	}
	for _, lane := range work.Lanes() {
		job, ok, err := w.Queue.Lease(ctx, lane)
		if err != nil {
			w.logf("lease=missed", "lane="+lane, "code="+string(codes.From(err).Code))
			return busy
		}
		if !ok {
			continue
		}
		if w.Spool.Has(job.ID) {
			// Muster has handed back a job that is already staged here.
			// Carry on from the record instead of starting again.
			w.resume(ctx, job.ID)
		} else {
			w.advance(ctx, &spool.Record{Job: job}, false)
		}
		return true
	}
	return busy
}

func (w *Worker) resume(ctx context.Context, jobID string) {
	rec, err := w.Spool.Get(jobID)
	if err != nil {
		w.logf("job="+ref(jobID), "spool=unreadable", "code="+string(codes.From(err).Code))
		return
	}
	w.advance(ctx, rec, true)
}

// advance carries one job as far as it will go: through the sub-tools that
// still have work, then to Muster with the verdict.
func (w *Worker) advance(ctx context.Context, rec *spool.Record, staged bool) {
	progress := false
	if !rec.Finished {
		if err := rec.Job.Check(); err != nil {
			e := codes.From(err)
			rec.Fault = &spool.Fault{Code: string(e.Code), Reason: e.Reason}
		} else {
			done := map[string]bool{}
			for _, name := range rec.Done {
				done[name] = true
			}
			out := route.Run(ctx, w.Tools, rec.Job, &rec.State, done, orDefault(w.PerTool, DefaultPerTool))
			rec.Done = rec.Done[:0]
			for _, tool := range w.Tools {
				if done[tool.Name()] {
					rec.Done = append(rec.Done, tool.Name())
				}
			}
			if out.Fault != nil && rec.Fault == nil {
				rec.Fault = &spool.Fault{Code: string(out.Fault.Code), Reason: out.Fault.Reason}
			}
			if out.Waiting {
				w.stage(rec, out.Progress, "waiting", string(codes.Timeout))
				return
			}
		}
		rec.Finished = true
		progress = true
	}

	verdict, err := w.answer(ctx, rec)
	if e := codes.From(err); e != nil {
		if e.Code == codes.Timeout {
			w.stage(rec, progress, "answer-waiting", string(codes.Timeout))
			return
		}
		// Muster refused the answer. Asking again would get the same
		// refusal, so the record is let go.
		w.logf(describe(rec.Job), "result=answer-refused", "code="+string(e.Code))
	} else {
		code := ""
		if rec.Fault != nil {
			code = rec.Fault.Code
		}
		w.logf(describe(rec.Job), "result="+verdict, "code="+code)
	}
	if staged {
		if err := w.Spool.Delete(rec.Job.ID); err != nil {
			w.logf(describe(rec.Job), "spool=stuck", "code="+string(codes.From(err).Code))
		}
	}
}

// answer tells Muster how the job ended: complete, or fail for broken data,
// or needs_subtool for data no rule covers.
func (w *Worker) answer(ctx context.Context, rec *spool.Record) (string, error) {
	switch {
	case rec.Fault == nil:
		return muster.VerdictComplete, w.Queue.Complete(ctx, rec.Job.ID)
	case rec.Fault.Code == string(codes.NeedsSubtool):
		return muster.VerdictNeedsSubtool, w.Queue.NeedsSubtool(ctx, rec.Job.ID, rec.Fault.Reason)
	default:
		return muster.VerdictFail, w.Queue.Fail(ctx, rec.Job.ID, codes.Code(rec.Fault.Code))
	}
}

// stage writes the record to the spool with its next retry time.
func (w *Worker) stage(rec *spool.Record, progress bool, result, code string) {
	if progress {
		rec.Attempts = 1
	} else {
		rec.Attempts++
	}
	rec.RetryAt = w.now().Add(spool.Backoff(rec.Attempts)).Unix()
	if err := w.Spool.Put(rec); err != nil {
		// Nothing can be staged. The lease will run out and Muster will
		// hand the job out again.
		w.logf(describe(rec.Job), "spool=unwritable", "code="+string(codes.From(err).Code))
		return
	}
	w.logf(describe(rec.Job), "result="+result, "code="+code)
}

// ref returns a job reference for a log line, or a placeholder when the
// reference is not shaped like one and so cannot be trusted in a log.
func ref(id string) string {
	if sanitize.IsRef(id) {
		return id
	}
	return "malformed"
}

func describe(job work.Job) string {
	lane, kind := job.Lane, job.Kind
	if !work.IsLane(lane) {
		lane = "malformed"
	}
	if !sanitize.IsKind(kind) {
		kind = "malformed"
	}
	return "job=" + ref(job.ID) + " lane=" + lane + " kind=" + kind
}
