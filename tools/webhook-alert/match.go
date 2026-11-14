// Package webhookalert is the webhook-alert sub-tool. It sends the owner a
// short alert, at a configured webhook, for the few things worth being
// interrupted for.
package webhookalert

import (
	"context"

	"github.com/ClearedStack/bellbuoy/internal/work"
)

// Name is this sub-tool's name in the registry.
const Name = "webhook-alert"

// TierP0 is the attention tier that always alerts.
const TierP0 = "P0"

// Sender delivers one alert body to the owner's webhook. It reports trouble
// with the public codes: E_TIMEOUT when the endpoint did not answer in time
// or is not ready, E_PARSE when its answer was not the one expected.
type Sender interface {
	Send(ctx context.Context, body []byte) error
}

// Tool is the webhook-alert sub-tool. Sender is nil when no webhook is
// configured.
type Tool struct {
	Sender Sender
}

var _ work.Tool = (*Tool)(nil)

// Name returns the registry name.
func (*Tool) Name() string { return Name }

// Gate is false: nothing later reads what this sub-tool does.
func (*Tool) Gate() bool { return false }

// Match is the matching rule. An alert goes out for exactly three things:
//
//   - a failure whose fingerprint has no open task, so it is new;
//   - a crash or a dead letter in tier P0, new or not;
//   - regulatory drift in the urgent lane.
//
// A repeat of a known failure is counted on its task and alerts nobody.
func (*Tool) Match(job work.Job, st work.State) bool {
	if job.Kind == work.KindDrift {
		return job.Lane == work.LaneUrgent
	}
	if !work.IsFailure(job.Kind) || st.Task == nil {
		return false
	}
	if st.Task.Created {
		return true
	}
	return (job.Kind == work.KindCrash || job.Kind == work.KindDeadLetter) && st.Task.Tier == TierP0
}
