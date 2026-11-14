// Package route runs one job through the sub-tools.
package route

import (
	"context"
	"time"

	"github.com/ClearedStack/bellbuoy/internal/codes"
	"github.com/ClearedStack/bellbuoy/internal/work"
)

// Outcome says how far a job got.
type Outcome struct {
	// Waiting is true when a call outside timed out. The job is not
	// finished and has to be tried again.
	Waiting bool
	// Fault is the first rule failure of this run, if any.
	Fault *codes.Error
	// Progress is true when at least one sub-tool finished in this run.
	Progress bool
}

// Run tries the sub-tools on a job in order. done names the sub-tools that
// finished with this job on an earlier run. They are skipped, and those that
// finish now are added.
//
// The rules:
//
//   - A sub-tool runs only if its matching rule says yes.
//   - If no sub-tool matches at all, the job is E_NEEDS_SUBTOOL. Nothing is
//     improvised for it.
//   - E_TIMEOUT from a sub-tool leaves it to be tried again. The other
//     sub-tools still run, unless the one waiting is a gate, in which case
//     the rest wait for it.
//   - Any other error is a fault. The first one is kept. The sub-tool is
//     done and is not tried again. After a fault in a gate nothing else runs.
//
// Each sub-tool gets at most perTool to do its work. Zero means no limit.
func Run(ctx context.Context, tools []work.Tool, job work.Job, st *work.State, done map[string]bool, perTool time.Duration) Outcome {
	var out Outcome
	matched := len(done) > 0
	for _, tool := range tools {
		if done[tool.Name()] || !tool.Match(job, *st) {
			continue
		}
		matched = true
		err := codes.From(runOne(ctx, tool, job, st, perTool))
		switch {
		case err == nil:
			done[tool.Name()] = true
			out.Progress = true
		case err.Code == codes.Timeout:
			out.Waiting = true
			if tool.Gate() {
				return out
			}
		default:
			done[tool.Name()] = true
			if out.Fault == nil {
				out.Fault = err
			}
			if tool.Gate() {
				return out
			}
		}
	}
	if !matched {
		out.Fault = codes.Unfamiliar("no rule covers this kind of job")
	}
	return out
}

func runOne(ctx context.Context, tool work.Tool, job work.Job, st *work.State, perTool time.Duration) error {
	if perTool > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, perTool)
		defer cancel()
	}
	return tool.Run(ctx, job, st)
}
