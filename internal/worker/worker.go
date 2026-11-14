// Package worker is the loop that leases steps from Muster, runs them through
// the sub-tools, and tells Muster how each one ended.
//
// One step is worked on at a time. That keeps the order of everything
// Bellbuoy does a function of the order of its input, and makes the
// open-or-add-to rule safe without a lock in Plainforge. The only other
// goroutine is the one that keeps the lease of the step in hand alive.
package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"sync"
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
	DefaultHeartbeat = 20 * time.Second
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
	// Heartbeat is how often a held lease is kept alive.
	Heartbeat time.Duration
	// Idle is how long Run waits when Muster has nothing to lease.
	Idle time.Duration
	// PerTool is how long one sub-tool may take on one job.
	PerTool time.Duration
	// MaxHeld is how many steps may be staged at once. At the limit no new
	// step is leased, which bounds both memory and disk.
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

// Run works until ctx is done. Steps in hand when it stops are staged in the
// spool, not lost. When Muster has nothing to lease, Run waits and asks
// again.
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
// how many steps are still staged, waiting for a later retry.
func (w *Worker) Drain(ctx context.Context) int {
	for ctx.Err() == nil && w.Step(ctx) {
	}
	return w.Spool.Len()
}

// Step does one round and reports whether it found anything to do:
//
//  1. keep the leases of staged steps alive, if a heartbeat is due;
//  2. try again every staged step whose retry time has come, oldest first;
//  3. lease one new step and work on it. Muster offers urgent work before
//     normal and normal before backfill.
func (w *Worker) Step(ctx context.Context) bool {
	now := w.now()
	if now.Sub(w.lastBeat) >= orDefault(w.Heartbeat, DefaultHeartbeat) {
		w.lastBeat = now
		for _, lease := range w.Spool.Leases() {
			w.beat(ctx, lease)
		}
	}

	busy := false
	for _, key := range w.Spool.Due(now) {
		if ctx.Err() != nil {
			return busy
		}
		if rec := w.load(key); rec != nil {
			w.advance(ctx, rec)
		}
		busy = true
	}

	if w.Spool.Len() >= orDefault(w.MaxHeld, DefaultMaxHeld) {
		return busy
	}
	job, ok, err := w.Queue.Lease(ctx)
	if err != nil {
		w.logf("lease=missed", "code="+string(codes.From(err).Code))
		return busy
	}
	if !ok {
		return busy
	}
	rec := &spool.Record{Job: job}
	if w.Spool.Has(job.Key()) {
		staged := w.load(job.Key())
		if staged == nil {
			return true
		}
		if staged.Job.State == job.State && bytes.Equal(staged.Job.Payload, job.Payload) {
			// Muster has handed out a step that is already staged here,
			// under a new lease. Carry on from the record instead of
			// starting again, and answer with the lease that is now the
			// live one.
			rec = staged
			rec.Job.Lease = job.Lease
			rec.Job.Attempt = job.Attempt
		} else {
			// Same item, different step: Muster replaces an incident
			// that was not dealt with by a newer one. The staged record
			// is about something that no longer exists. Start again,
			// keeping its place in the spool.
			rec.Seq = staged.Seq
		}
	}
	w.advance(ctx, rec)
	return true
}

func (w *Worker) load(key string) *spool.Record {
	rec, err := w.Spool.Get(key)
	if err != nil {
		w.logf("spool=unreadable", "code="+string(codes.From(err).Code))
		return nil
	}
	return rec
}

func (w *Worker) beat(ctx context.Context, lease string) {
	if err := w.Queue.Heartbeat(ctx, lease); err != nil {
		w.logf("heartbeat=missed", "code="+string(codes.From(err).Code))
	}
}

// keepAlive sends a heartbeat for one lease at every interval until the
// returned function is called.
func (w *Worker) keepAlive(ctx context.Context, lease string) (stop func()) {
	if lease == "" {
		return func() {}
	}
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		t := time.NewTicker(orDefault(w.Heartbeat, DefaultHeartbeat))
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-t.C:
				w.beat(ctx, lease)
			}
		}
	}()
	return func() {
		close(done)
		wg.Wait()
	}
}

// advance carries one step as far as it will go: through the sub-tools that
// still have work, into the spool, and then to Muster with the answer. The
// record leaves the spool only when Muster has acknowledged the answer.
func (w *Worker) advance(ctx context.Context, rec *spool.Record) {
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
			stop := w.keepAlive(ctx, rec.Job.Lease)
			out := route.Run(ctx, w.Tools, rec.Job, &rec.State, done, orDefault(w.PerTool, DefaultPerTool))
			stop()
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
		// The result goes to disk before Muster hears of it, so a stop
		// between the two cannot lose it or make the work happen twice.
		rec.Attempts = 0
		rec.RetryAt = w.now().Unix()
		if err := w.Spool.Put(rec); err != nil {
			w.logf(describe(rec.Job), "spool=unwritable", "code="+string(codes.From(err).Code))
		}
	}

	verdict, err := w.answer(ctx, rec)
	if verdict == muster.VerdictNeedsSubtool && errors.Is(err, muster.ErrMoveNotAllowed) &&
		rec.State.Task == nil && rec.State.Change == nil {
		// Muster does not let this step be paused, and nothing was written
		// for it, so there is no result to keep. Let the lease run out.
		// Muster will offer the step again, and it will be looked at
		// afresh, by which time a rule or a setting may cover it.
		w.logf(describe(rec.Job), "result=pause-refused", "code="+rec.Fault.Code, "reason="+quote(rec.Fault.Reason))
		if err := w.Spool.Delete(rec.Job.Key()); err != nil {
			w.logf(describe(rec.Job), "spool=stuck", "code="+string(codes.From(err).Code))
		}
		return
	}
	if e := codes.From(err); e != nil {
		// Not acknowledged, so the record stays. A refusal is kept too: if
		// the lease has run out, Muster will hand the step out again and
		// the answer goes back under the new lease.
		result := "answer-refused"
		if e.Code == codes.Timeout {
			result = "answer-waiting"
		}
		w.stage(rec, progress, result, string(e.Code))
		return
	}
	parts := []string{describe(rec.Job), "result=" + verdict}
	if rec.Fault != nil {
		parts = append(parts, "code="+rec.Fault.Code, "reason="+quote(rec.Fault.Reason))
	}
	w.logf(parts...)
	if err := w.Spool.Delete(rec.Job.Key()); err != nil {
		w.logf(describe(rec.Job), "spool=stuck", "code="+string(codes.From(err).Code))
	}
}

// answer tells Muster how the step ended: complete, with the item left in
// the state it was leased in; or fail, for broken data; or needs_subtool, for
// data no rule covers.
//
// A completed step carries what was filed as its result, except an incident:
// Muster's contract says an incident step takes no result.
func (w *Worker) answer(ctx context.Context, rec *spool.Record) (string, error) {
	switch {
	case rec.Fault == nil:
		var result json.RawMessage
		if rec.Job.Kind != work.KindIncident {
			var err error
			if result, err = json.Marshal(rec.State); err != nil {
				return muster.VerdictComplete, codes.Broken("result could not be written out")
			}
		}
		return muster.VerdictComplete, w.Queue.Complete(ctx, rec.Job.Lease, rec.Job.State, result)
	case rec.Fault.Code == string(codes.NeedsSubtool):
		return muster.VerdictNeedsSubtool, w.Queue.NeedsSubtool(ctx, rec.Job.Lease)
	default:
		return muster.VerdictFail, w.Queue.Fail(ctx, rec.Job.Lease, codes.Parse)
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
		// hand the step out again.
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

// quote wraps a fixed reason for a log line. Reasons are constants in the
// source and hold no quotation marks.
func quote(reason string) string { return `"` + reason + `"` }
