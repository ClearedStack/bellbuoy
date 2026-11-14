// Package work holds the types shared by the router and the sub-tools: the
// job leased from Muster, what a sub-tool leaves behind for the next one, and
// the shape every sub-tool has.
package work

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"reflect"
	"strings"

	"github.com/ClearedStack/bellbuoy/internal/codes"
	"github.com/ClearedStack/bellbuoy/internal/sanitize"
)

// The three lanes, in the order they are drained.
const (
	LaneUrgent   = "urgent"
	LaneNormal   = "normal"
	LaneBackfill = "backfill"
)

// Lanes returns the lanes in draining order: urgent, normal, backfill.
func Lanes() []string { return []string{LaneUrgent, LaneNormal, LaneBackfill} }

// IsLane reports whether s names a lane.
func IsLane(s string) bool { return s == LaneUrgent || s == LaneNormal || s == LaneBackfill }

// The kinds of job a rule exists for. Any other kind is paused.
const (
	KindError        = "error"         // a worker reported a failure
	KindCrash        = "crash"         // a worker stopped
	KindDeadLetter   = "dead_letter"   // an item ran out of attempts
	KindNeedsSubtool = "needs_subtool" // a worker paused an item for want of a rule
	KindEdit         = "edit"          // an owner approved a Portal edit
	KindStatus       = "status"        // an edit moved on
	KindDrift        = "drift"         // a public rule changed
	KindMirror       = "mirror"        // a repository changed
	KindIncident     = "incident"      // Muster holds or has paused an item
	KindProposal     = "proposal"      // Muster approved an edit proposal
)

// IsFailure reports whether a job of this kind describes a failure that is
// fingerprinted and turned into a task.
func IsFailure(kind string) bool {
	switch kind {
	case KindError, KindCrash, KindDeadLetter, KindNeedsSubtool, KindIncident:
		return true
	}
	return false
}

// MaxPayload is the largest job payload accepted, in bytes.
const MaxPayload = 512 << 10

// Job is one step leased from Muster.
//
// ID names the item or proposal the step belongs to. Muster holds at most
// one step for each, so Kind and ID together name the step. Lease names one
// handing-out of it and changes each time the step is handed out again.
type Job struct {
	ID   string `json:"id"`
	Lane string `json:"lane"`
	Kind string `json:"kind"`
	// Lease is the token every later call about this step carries.
	Lease string `json:"lease,omitempty"`
	// State is the state the item was in when the step was leased.
	State string `json:"state,omitempty"`
	// Attempt counts how many times the step has been handed out.
	Attempt int `json:"attempt,omitempty"`
	// Payload is everything else the lease carried, as one JSON object.
	Payload json.RawMessage `json:"payload"`
}

// Key names the step a job is one handing-out of.
func (j Job) Key() string { return j.Kind + "/" + j.ID }

// Check reports whether the job's envelope is well formed. It says nothing
// about whether a rule covers the job.
func (j Job) Check() error {
	switch {
	case !sanitize.IsRef(j.ID):
		return codes.Broken("job reference is missing or malformed")
	case !IsLane(j.Lane):
		return codes.Broken("job lane is not urgent, normal or backfill")
	case !sanitize.IsKind(j.Kind):
		return codes.Broken("job kind is missing or malformed")
	case j.Lease != "" && !sanitize.IsLease(j.Lease):
		return codes.Broken("job lease is malformed")
	case j.State != "" && !sanitize.IsKind(j.State):
		return codes.Broken("job state is malformed")
	case j.Attempt < 0:
		return codes.Broken("job attempt count is malformed")
	case len(j.Payload) > MaxPayload:
		return codes.Broken("job payload is too large")
	}
	return nil
}

// TaskResult records the task a job was turned into.
type TaskResult struct {
	Repo        string `json:"repo"`
	ID          string `json:"id"`
	Created     bool   `json:"created"`
	Occurrences int    `json:"occurrences"`
	Worker      string `json:"worker,omitempty"`
	Code        string `json:"code,omitempty"`
	Fingerprint string `json:"fingerprint,omitempty"`
	Tier        string `json:"tier,omitempty"`
	Edit        string `json:"edit,omitempty"`
}

// ChangeResult records the change an approved edit was turned into.
type ChangeResult struct {
	Repo    string `json:"repo"`
	Branch  string `json:"branch"`
	Edit    string `json:"edit"`
	Created bool   `json:"created"`
}

// State is what the sub-tools that have run so far leave for the ones that
// follow. Only internal-git writes to it.
type State struct {
	Task   *TaskResult   `json:"task,omitempty"`
	Change *ChangeResult `json:"change,omitempty"`
}

// Tool is one sub-tool: a matching rule and the logic behind it.
type Tool interface {
	// Name is the sub-tool's name in the registry.
	Name() string
	// Gate reports whether later sub-tools read what this one leaves in the
	// state. While a gate is waiting on something outside, the rest wait too.
	Gate() bool
	// Match is the matching rule. It must not look outside the process.
	Match(job Job, st State) bool
	// Run does the work. It returns nil or an error carrying a public code.
	Run(ctx context.Context, job Job, st *State) error
}

// Decode reads a job payload into v, a pointer to a struct whose fields carry
// json tags. The payload must be one JSON object. A field of the wrong type is
// E_PARSE. A field the struct does not have is E_NEEDS_SUBTOOL: the data is
// valid, and no rule says what to do with it.
func Decode(raw json.RawMessage, v any) error {
	if len(bytes.TrimSpace(raw)) == 0 {
		return codes.Broken("job payload is missing")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	var fields map[string]json.RawMessage
	if err := dec.Decode(&fields); err != nil || fields == nil {
		return codes.Broken("job payload is not a JSON object")
	}
	if _, err := dec.Token(); err != io.EOF {
		return codes.Broken("job payload has text after the JSON object")
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return codes.Broken("job payload has a field of the wrong type")
	}
	known := map[string]bool{}
	t := reflect.TypeOf(v).Elem()
	for i := 0; i < t.NumField(); i++ {
		name, _, _ := strings.Cut(t.Field(i).Tag.Get("json"), ",")
		known[name] = true
	}
	for name := range fields {
		if !known[name] {
			return codes.Unfamiliar("job payload has a field no rule covers")
		}
	}
	return nil
}
