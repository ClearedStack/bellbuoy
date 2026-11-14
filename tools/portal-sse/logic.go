package portalsse

import (
	"context"

	"github.com/ClearedStack/bellbuoy/internal/codes"
	"github.com/ClearedStack/bellbuoy/internal/muster"
	"github.com/ClearedStack/bellbuoy/internal/sanitize"
	"github.com/ClearedStack/bellbuoy/internal/work"
)

// The four states an edit pill moves through, in order.
const (
	StateProposed = "Proposed"
	StateApproved = "Approved"
	StateBuilding = "Building"
	StateLive     = "Live"
)

// status is the payload of a status job.
type status struct {
	Topic string `json:"topic"`
	Edit  string `json:"edit"`
	State string `json:"state"`
}

// Run builds the event for one job and hands it to Muster.
func (t *Tool) Run(ctx context.Context, job work.Job, st *work.State) error {
	event, err := plan(job, *st)
	if err != nil {
		return err
	}
	if t.Stream == nil {
		return codes.Unfamiliar("no live stream is configured")
	}
	return t.Stream.Publish(ctx, event)
}

// plan builds the event. It looks at nothing outside the process, and it
// copies no free text: an event is made only of fields that have been
// checked for shape or that Bellbuoy worked out itself.
func plan(job work.Job, st work.State) (muster.Event, error) {
	switch {
	case job.Kind == work.KindStatus:
		return planStatus(job)
	case job.Kind == work.KindEdit:
		// internal-git has committed an approved edit.
		edit := ""
		if st.Change != nil {
			edit = st.Change.Edit
		} else {
			edit = st.Task.Edit
		}
		return muster.Event{Topic: muster.TopicPill, Edit: edit, State: StateApproved}, nil
	default:
		task := st.Task
		return muster.Event{
			Topic:       muster.TopicOperations,
			Worker:      task.Worker,
			Code:        task.Code,
			Fingerprint: task.Fingerprint,
			Occurrences: task.Occurrences,
		}, nil
	}
}

func planStatus(job work.Job) (muster.Event, error) {
	var s status
	if err := work.Decode(job.Payload, &s); err != nil {
		return muster.Event{}, err
	}
	if s.Topic != muster.TopicPill {
		if sanitize.IsKind(s.Topic) {
			return muster.Event{}, codes.Unfamiliar("no rule covers this stream")
		}
		return muster.Event{}, codes.Broken("stream name is missing or malformed")
	}
	if !sanitize.IsRef(s.Edit) {
		return muster.Event{}, codes.Broken("edit reference is missing or malformed")
	}
	switch s.State {
	case StateProposed, StateApproved, StateBuilding, StateLive:
	default:
		if sanitize.IsLetters(s.State, 16) {
			return muster.Event{}, codes.Unfamiliar("no rule covers this edit state")
		}
		return muster.Event{}, codes.Broken("edit state is missing or malformed")
	}
	return muster.Event{Topic: muster.TopicPill, Edit: s.Edit, State: s.State}, nil
}
