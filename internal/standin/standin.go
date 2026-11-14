// Package standin gathers the in-memory stand-ins for everything Bellbuoy
// reaches outside through: Muster, Plainforge and the owner's webhook. With
// them Bellbuoy runs and is tested alone.
package standin

import (
	"context"
	"encoding/json"
	"sync"

	"github.com/ClearedStack/bellbuoy/internal/muster"
	"github.com/ClearedStack/bellbuoy/internal/ownership"
	"github.com/ClearedStack/bellbuoy/internal/plainforge"
)

// Recorder is a stand-in for the owner's webhook. It keeps what it is sent.
type Recorder struct {
	mu    sync.Mutex
	sent  []json.RawMessage
	fault error
}

// Send keeps the body, or fails if a fault is set.
func (r *Recorder) Send(_ context.Context, body []byte) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fault != nil {
		return r.fault
	}
	r.sent = append(r.sent, append(json.RawMessage(nil), body...))
	return nil
}

// SetFault makes every later Send fail with err. A nil err clears it.
func (r *Recorder) SetFault(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.fault = err
}

// Sent returns every body received, in order.
func (r *Recorder) Sent() []json.RawMessage {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]json.RawMessage(nil), r.sent...)
}

// Set is one of each stand-in, with the ownership map.
type Set struct {
	Forge  *plainforge.Memory
	Queue  *muster.Memory
	Owners *ownership.Map
	Alerts *Recorder
}

// New returns fresh stand-ins and the ownership map at the given path.
func New(ownersPath string) (Set, error) {
	owners, err := ownership.Load(ownersPath)
	if err != nil {
		return Set{}, err
	}
	return Set{Forge: plainforge.NewMemory(), Queue: muster.NewMemory(), Owners: owners, Alerts: &Recorder{}}, nil
}

// SetFault makes every later call to any stand-in fail with err. A nil err
// clears it.
func (s Set) SetFault(err error) {
	s.Forge.SetFault(err)
	s.Queue.SetFault(err)
	s.Alerts.SetFault(err)
}

// Effects is everything the stand-ins hold after a run.
type Effects struct {
	Tasks   []plainforge.Task          `json:"tasks,omitempty"`
	Changes []plainforge.Change        `json:"changes,omitempty"`
	Mirrors []plainforge.MirrorRequest `json:"mirrors,omitempty"`
	Events  []muster.Event             `json:"events,omitempty"`
	Alerts  []json.RawMessage          `json:"alerts,omitempty"`
}

// Collect reads the effects out of the stand-ins.
func (s Set) Collect() Effects {
	return Effects{
		Tasks:   s.Forge.Tasks(),
		Changes: s.Forge.Changes(),
		Mirrors: s.Forge.Mirrors(),
		Events:  s.Queue.Events(),
		Alerts:  s.Alerts.Sent(),
	}
}
