// Package muster is the small interface Bellbuoy needs from Muster, the
// queue it leases work from, and an in-memory stand-in for it.
//
// Muster itself is a separate component and is not built here. Bellbuoy
// never talks to the database behind Muster.
package muster

import (
	"context"

	"github.com/ClearedStack/bellbuoy/internal/codes"
	"github.com/ClearedStack/bellbuoy/internal/work"
)

// The two streams a rule exists for.
const (
	// TopicPill carries the state of one Portal edit.
	TopicPill = "pill"
	// TopicOperations carries failures by fingerprint.
	TopicOperations = "operations"
)

// Event is one update for the live stream. It has no free text: every field
// is a name from the registry, a public code, a hash, a count, or an opaque
// reference.
type Event struct {
	Topic       string `json:"topic"`
	Edit        string `json:"edit,omitempty"`
	State       string `json:"state,omitempty"`
	Worker      string `json:"worker,omitempty"`
	Code        string `json:"code,omitempty"`
	Fingerprint string `json:"fingerprint,omitempty"`
	Occurrences int    `json:"occurrences,omitempty"`
}

// Queue is everything Bellbuoy asks of Muster.
//
// An implementation reports trouble with the public codes: E_TIMEOUT when
// Muster did not answer in time, E_PARSE when its answer made no sense.
type Queue interface {
	// Lease takes the next job from one lane. ok is false when the lane is
	// empty.
	Lease(ctx context.Context, lane string) (job work.Job, ok bool, err error)
	// Heartbeat keeps a lease alive.
	Heartbeat(ctx context.Context, jobID string) error
	// Complete reports a job as done.
	Complete(ctx context.Context, jobID string) error
	// Fail reports a job as failed, with a public code.
	Fail(ctx context.Context, jobID string, code codes.Code) error
	// NeedsSubtool pauses a job until a rule exists for it.
	NeedsSubtool(ctx context.Context, jobID string, reason string) error
	// Publish hands one event to the live stream.
	Publish(ctx context.Context, event Event) error
}
