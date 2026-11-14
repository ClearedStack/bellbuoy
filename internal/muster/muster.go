// Package muster is the small interface Bellbuoy needs from Muster, the
// queue it leases work from, with a client for the real one and an in-memory
// stand-in.
//
// Muster itself is a separate component and is not built here. Bellbuoy
// never talks to the database behind Muster.
package muster

import (
	"context"
	"encoding/json"

	"github.com/ClearedStack/bellbuoy/internal/codes"
	"github.com/ClearedStack/bellbuoy/internal/work"
)

// Contract is the version of the Muster contract this package speaks.
const Contract = "v1"

// Two refusals a worker has to tell apart from the rest. Both carry E_PARSE.
var (
	// ErrLeaseNotHeld: the lease is unknown, has ended, or is someone
	// else's. The same call will never be acknowledged under this lease.
	ErrLeaseNotHeld = codes.Broken("muster says the lease is not held")
	// ErrMoveNotAllowed: Muster's tables do not allow what was asked, such
	// as pausing a step that is not an item's own.
	ErrMoveNotAllowed = codes.Broken("muster does not allow that move")
)

// Queue is the five worker calls of Muster's contract v1.
//
// An implementation reports trouble with the public codes: E_TIMEOUT when
// Muster did not answer in time or asked to be tried again, E_PARSE when it
// refused the call or its answer made no sense, E_NEEDS_SUBTOOL when it does
// not know the contract version.
type Queue interface {
	// Lease takes the next step Muster has for this worker. Muster offers
	// urgent work first, then normal, then backfill. ok is false when there
	// is nothing to do.
	Lease(ctx context.Context) (job work.Job, ok bool, err error)
	// Heartbeat keeps a lease alive.
	Heartbeat(ctx context.Context, lease string) error
	// Complete reports a step as done. state is the state the item is to be
	// in afterwards, and result is a JSON object describing what was done.
	Complete(ctx context.Context, lease, state string, result json.RawMessage) error
	// Fail reports a step as failed, with E_TIMEOUT or E_PARSE.
	Fail(ctx context.Context, lease string, code codes.Code) error
	// NeedsSubtool pauses a step until a rule exists for it.
	NeedsSubtool(ctx context.Context, lease string) error
}

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

// Publisher hands events to the live stream. Muster's contract v1 has no
// call for this, so only the in-memory stand-in implements it.
type Publisher interface {
	Publish(ctx context.Context, event Event) error
}
