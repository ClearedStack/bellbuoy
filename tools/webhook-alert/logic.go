package webhookalert

import (
	"context"
	"encoding/json"

	"github.com/ClearedStack/bellbuoy/internal/codes"
	"github.com/ClearedStack/bellbuoy/internal/sanitize"
	"github.com/ClearedStack/bellbuoy/internal/work"
)

// Alert is the body sent to the webhook. It has no free text: every field is
// a registry name, a public code, a hash, a count or an opaque reference, so
// an alert cannot name a machine, an address or a credential.
type Alert struct {
	Source      string `json:"source"`
	Kind        string `json:"kind"`
	Lane        string `json:"lane"`
	Tier        string `json:"tier,omitempty"`
	New         bool   `json:"new"`
	Worker      string `json:"worker,omitempty"`
	Code        string `json:"code,omitempty"`
	Fingerprint string `json:"fingerprint,omitempty"`
	Occurrences int    `json:"occurrences,omitempty"`
	Task        string `json:"task,omitempty"`
	Ref         string `json:"ref,omitempty"`
}

// drift is the payload of a drift job: a public rule changed. Ref is an
// opaque reference to the rule.
type drift struct {
	Ref string `json:"ref"`
}

// Run builds the alert for one job and sends it.
func (t *Tool) Run(ctx context.Context, job work.Job, st *work.State) error {
	alert, err := plan(job, *st)
	if err != nil {
		return err
	}
	if t.Sender == nil {
		return codes.Unfamiliar("no webhook is configured for owner alerts")
	}
	body, err := json.Marshal(alert)
	if err != nil {
		return codes.Broken("alert could not be written out")
	}
	return t.Sender.Send(ctx, body)
}

// plan builds the alert. It looks at nothing outside the process.
func plan(job work.Job, st work.State) (Alert, error) {
	alert := Alert{Source: "bellbuoy", Kind: job.Kind, Lane: job.Lane}
	if job.Kind == work.KindDrift {
		var d drift
		if err := work.Decode(job.Payload, &d); err != nil {
			return Alert{}, err
		}
		if !sanitize.IsRef(d.Ref) {
			return Alert{}, codes.Broken("drift reference is missing or malformed")
		}
		alert.New = true
		alert.Ref = d.Ref
		return alert, nil
	}
	task := st.Task
	alert.Tier = task.Tier
	alert.New = task.Created
	alert.Worker = task.Worker
	alert.Code = task.Code
	alert.Fingerprint = task.Fingerprint
	alert.Occurrences = task.Occurrences
	alert.Task = task.ID
	return alert, nil
}
