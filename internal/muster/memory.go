package muster

import (
	"context"
	"encoding/json"
	"strconv"
	"sync"

	"github.com/ClearedStack/bellbuoy/internal/codes"
	"github.com/ClearedStack/bellbuoy/internal/work"
)

// The three ways a step ends.
const (
	VerdictComplete     = "complete"
	VerdictFail         = "fail"
	VerdictNeedsSubtool = "needs_subtool"
)

// Ack is the record of how one step ended.
type Ack struct {
	Job     string          `json:"job"`
	Verdict string          `json:"verdict"`
	Code    string          `json:"code,omitempty"`
	State   string          `json:"state,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
}

// Memory is an in-memory stand-in for Muster. It is safe for use by several
// goroutines.
type Memory struct {
	mu       sync.Mutex
	lanes    map[string][]work.Job
	leased   map[string]work.Job // by lease token
	attempts map[string]int      // by step key
	order    []string            // lane of each step handed out, in order
	acks     []Ack
	events   []Event
	beats    map[string]int // by job reference
	fault    error
	feedback string
	fed      int
}

// NewMemory returns an empty stand-in.
func NewMemory() *Memory {
	return &Memory{
		lanes:    map[string][]work.Job{},
		leased:   map[string]work.Job{},
		attempts: map[string]int{},
		beats:    map[string]int{},
	}
}

var (
	_ Queue     = (*Memory)(nil)
	_ Publisher = (*Memory)(nil)
)

// Add puts a job at the back of its lane.
func (m *Memory) Add(job work.Job) { m.AddTo(job.Lane, job) }

// AddTo puts a job at the back of the named lane, whatever lane the job
// itself claims. It is for showing what happens when the two disagree.
func (m *Memory) AddTo(lane string, job work.Job) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lanes[lane] = append(m.lanes[lane], job)
}

// SetFault makes every later call fail with err. A nil err clears it.
func (m *Memory) SetFault(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.fault = err
}

// SetFeedback makes the stand-in behave like the platform around it: each
// step that is failed or paused comes back as a new job reporting that this
// worker failed. An empty worker turns it off.
func (m *Memory) SetFeedback(worker string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.feedback = worker
}

// Expire ends every lease on a job without an answer and puts the job back
// at the front of its lane, as Muster does when heartbeats stop.
func (m *Memory) Expire(jobID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for lease, job := range m.leased {
		if job.ID == jobID {
			delete(m.leased, lease)
			job.Lease = ""
			m.lanes[job.Lane] = append([]work.Job{job}, m.lanes[job.Lane]...)
			return true
		}
	}
	return false
}

// Acks returns how each step ended, in order.
func (m *Memory) Acks() []Ack {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]Ack(nil), m.acks...)
}

// Events returns every published event, in order.
func (m *Memory) Events() []Event {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]Event(nil), m.events...)
}

// Leased returns how many steps are currently out on lease.
func (m *Memory) Leased() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.leased)
}

// Waiting returns how many jobs are still queued.
func (m *Memory) Waiting() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, q := range m.lanes {
		n += len(q)
	}
	return n
}

// Beats returns how many heartbeats a job has had, over all its leases.
func (m *Memory) Beats(jobID string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.beats[jobID]
}

// LeaseOrder returns the lane of each step handed out, in order.
func (m *Memory) LeaseOrder() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.order...)
}

// Lease hands out the oldest job of the first lane that has one: urgent,
// then normal, then backfill. The lease token is the job reference, a dot,
// and the number of times the job has been handed out, with "incident:" in
// front for an incident, as Muster writes it.
func (m *Memory) Lease(context.Context) (work.Job, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fault != nil {
		return work.Job{}, false, m.fault
	}
	for _, lane := range work.Lanes() {
		q := m.lanes[lane]
		if len(q) == 0 {
			continue
		}
		job := q[0]
		m.lanes[lane] = q[1:]
		m.attempts[job.Key()]++
		job.Attempt = m.attempts[job.Key()]
		job.Lease = job.ID + "." + strconv.Itoa(job.Attempt)
		if job.Kind == work.KindIncident {
			job.Lease = "incident:" + job.Lease
		}
		m.leased[job.Lease] = job
		m.order = append(m.order, lane)
		return job, true, nil
	}
	return work.Job{}, false, nil
}

func (m *Memory) Heartbeat(_ context.Context, lease string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fault != nil {
		return m.fault
	}
	job, ok := m.leased[lease]
	if !ok {
		return ErrLeaseNotHeld
	}
	m.beats[job.ID]++
	return nil
}

func (m *Memory) Complete(_ context.Context, lease, state string, result json.RawMessage) error {
	return m.end(lease, Ack{Verdict: VerdictComplete, State: state, Result: append(json.RawMessage(nil), result...)})
}

func (m *Memory) Fail(_ context.Context, lease string, code codes.Code) error {
	if code != codes.Timeout && code != codes.Parse {
		return codes.Broken("a step can be failed only with E_TIMEOUT or E_PARSE")
	}
	return m.end(lease, Ack{Verdict: VerdictFail, Code: string(code)})
}

func (m *Memory) NeedsSubtool(_ context.Context, lease string) error {
	return m.end(lease, Ack{Verdict: VerdictNeedsSubtool, Code: string(codes.NeedsSubtool)})
}

func (m *Memory) end(lease string, ack Ack) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fault != nil {
		return m.fault
	}
	job, ok := m.leased[lease]
	if !ok {
		return ErrLeaseNotHeld
	}
	if job.Kind == work.KindIncident {
		// As in Muster's contract: an incident step changes no state,
		// takes no result, and cannot be paused.
		switch {
		case ack.Verdict == VerdictNeedsSubtool:
			return ErrMoveNotAllowed
		case ack.Verdict == VerdictComplete && ack.State != job.State:
			return ErrMoveNotAllowed
		case ack.Verdict == VerdictComplete && len(ack.Result) > 0:
			return codes.Broken("an incident step takes no result")
		}
	}
	if job.Kind == work.KindProposal {
		// As in Muster's contract: Bellbuoy moves an approved proposal to
		// building, and a proposal step cannot be paused.
		switch {
		case ack.Verdict == VerdictNeedsSubtool:
			return ErrMoveNotAllowed
		case ack.Verdict == VerdictComplete && (job.State != "approved" || ack.State != "building"):
			return ErrMoveNotAllowed
		}
	}
	delete(m.leased, lease)
	ack.Job = job.ID
	m.acks = append(m.acks, ack)
	if m.feedback != "" && ack.Verdict != VerdictComplete {
		m.fed++
		kind := work.KindError
		if ack.Verdict == VerdictNeedsSubtool {
			kind = work.KindNeedsSubtool
		}
		payload, _ := json.Marshal(map[string]string{"worker": m.feedback, "code": ack.Code, "pattern": "step ended as " + ack.Verdict})
		back := work.Job{ID: "feedback-" + strconv.Itoa(m.fed), Lane: work.LaneNormal, Kind: kind, Payload: payload}
		m.lanes[back.Lane] = append(m.lanes[back.Lane], back)
	}
	return nil
}

// Publish keeps one event for the live stream.
func (m *Memory) Publish(_ context.Context, event Event) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fault != nil {
		return m.fault
	}
	m.events = append(m.events, event)
	return nil
}
