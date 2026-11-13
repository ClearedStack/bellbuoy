package muster

import (
	"context"
	"encoding/json"
	"strconv"
	"sync"

	"github.com/ClearedStack/bellbuoy/internal/codes"
	"github.com/ClearedStack/bellbuoy/internal/work"
)

// The three ways a job ends.
const (
	VerdictComplete     = "complete"
	VerdictFail         = "fail"
	VerdictNeedsSubtool = "needs_subtool"
)

// Ack is the record of how one job ended.
type Ack struct {
	Job     string `json:"job"`
	Verdict string `json:"verdict"`
	Code    string `json:"code,omitempty"`
	Reason  string `json:"reason,omitempty"`
}

// Memory is an in-memory stand-in for Muster. It is safe for use by several
// goroutines.
type Memory struct {
	mu       sync.Mutex
	lanes    map[string][]work.Job
	leased   map[string]work.Job
	order    []string // lanes asked for, in order
	acks     []Ack
	events   []Event
	beats    map[string]int
	fault    error
	feedback string
	fed      int
}

// NewMemory returns an empty stand-in.
func NewMemory() *Memory {
	return &Memory{lanes: map[string][]work.Job{}, leased: map[string]work.Job{}, beats: map[string]int{}}
}

var _ Queue = (*Memory)(nil)

// Add puts a job at the back of its lane.
func (m *Memory) Add(job work.Job) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lanes[job.Lane] = append(m.lanes[job.Lane], job)
}

// SetFault makes every later call fail with err. A nil err clears it.
func (m *Memory) SetFault(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.fault = err
}

// SetFeedback makes the stand-in behave like the platform around it: each
// job that is failed or paused comes back as a new job reporting that this
// worker failed. An empty worker turns it off.
func (m *Memory) SetFeedback(worker string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.feedback = worker
}

// Expire ends a lease without an answer and puts the job back at the front
// of its lane, as Muster does when heartbeats stop.
func (m *Memory) Expire(jobID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	job, ok := m.leased[jobID]
	if !ok {
		return false
	}
	delete(m.leased, jobID)
	m.lanes[job.Lane] = append([]work.Job{job}, m.lanes[job.Lane]...)
	return true
}

// Acks returns how each job ended, in order.
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

// Leased returns the references of the jobs currently out on lease.
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

// Beats returns how many heartbeats a job has had.
func (m *Memory) Beats(jobID string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.beats[jobID]
}

// LeaseOrder returns the lane of each job handed out, in order.
func (m *Memory) LeaseOrder() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.order...)
}

func (m *Memory) Lease(_ context.Context, lane string) (work.Job, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fault != nil {
		return work.Job{}, false, m.fault
	}
	q := m.lanes[lane]
	if len(q) == 0 {
		return work.Job{}, false, nil
	}
	job := q[0]
	m.lanes[lane] = q[1:]
	m.leased[job.ID] = job
	m.order = append(m.order, lane)
	return job, true, nil
}

func (m *Memory) Heartbeat(_ context.Context, jobID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fault != nil {
		return m.fault
	}
	if _, ok := m.leased[jobID]; !ok {
		return codes.Broken("the lease is not held")
	}
	m.beats[jobID]++
	return nil
}

func (m *Memory) Complete(_ context.Context, jobID string) error {
	return m.end(Ack{Job: jobID, Verdict: VerdictComplete})
}

func (m *Memory) Fail(_ context.Context, jobID string, code codes.Code) error {
	return m.end(Ack{Job: jobID, Verdict: VerdictFail, Code: string(code)})
}

func (m *Memory) NeedsSubtool(_ context.Context, jobID string, reason string) error {
	return m.end(Ack{Job: jobID, Verdict: VerdictNeedsSubtool, Code: string(codes.NeedsSubtool), Reason: reason})
}

func (m *Memory) end(ack Ack) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fault != nil {
		return m.fault
	}
	if _, ok := m.leased[ack.Job]; !ok {
		return codes.Broken("the lease is not held")
	}
	delete(m.leased, ack.Job)
	m.acks = append(m.acks, ack)
	if m.feedback != "" && ack.Verdict != VerdictComplete {
		m.fed++
		kind := work.KindError
		if ack.Verdict == VerdictNeedsSubtool {
			kind = work.KindNeedsSubtool
		}
		payload, _ := json.Marshal(map[string]string{"worker": m.feedback, "code": ack.Code, "pattern": ack.Reason})
		job := work.Job{ID: "feedback-" + strconv.Itoa(m.fed), Lane: work.LaneNormal, Kind: kind, Payload: payload}
		m.lanes[job.Lane] = append(m.lanes[job.Lane], job)
	}
	return nil
}

func (m *Memory) Publish(_ context.Context, event Event) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fault != nil {
		return m.fault
	}
	m.events = append(m.events, event)
	return nil
}
