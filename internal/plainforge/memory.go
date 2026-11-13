package plainforge

import (
	"context"
	"sync"
)

// Memory is an in-memory task store. It is safe for use by many goroutines.
//
// Like a plain task tracker, it does not stop two open tasks from carrying
// the same fingerprint. Keeping them to one is Bellbuoy's job, and the tests
// would not prove anything if the stand-in did it instead.
type Memory struct {
	mu    sync.Mutex
	tasks []Task
	fail  []error
}

// NewMemory returns an empty store.
func NewMemory() *Memory { return &Memory{} }

// FailNext makes the next call to the store return err and change nothing.
// Calling it several times queues several failures.
func (m *Memory) FailNext(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.fail = append(m.fail, err)
}

// begin locks the store and returns a queued failure or a context error, if
// there is one. The caller must unlock.
func (m *Memory) begin(ctx context.Context) error {
	m.mu.Lock()
	if err := ctx.Err(); err != nil {
		return ErrTimeout
	}
	if len(m.fail) > 0 {
		err := m.fail[0]
		m.fail = m.fail[1:]
		return err
	}
	return nil
}

func (m *Memory) FindOpen(ctx context.Context, fingerprint string) (Task, bool, error) {
	err := m.begin(ctx)
	defer m.mu.Unlock()
	if err != nil {
		return Task{}, false, err
	}
	for _, t := range m.tasks {
		if t.Open && t.Fingerprint == fingerprint {
			return t, true, nil
		}
	}
	return Task{}, false, nil
}

func (m *Memory) Open(ctx context.Context, n NewTask) (Task, error) {
	err := m.begin(ctx)
	defer m.mu.Unlock()
	if err != nil {
		return Task{}, err
	}
	t := Task{
		ID:          len(m.tasks) + 1,
		Title:       n.Title,
		Body:        n.Body,
		Fingerprint: n.Fingerprint,
		Occurrences: 1,
		Open:        true,
	}
	m.tasks = append(m.tasks, t)
	return t, nil
}

func (m *Memory) AddOccurrence(ctx context.Context, id int) (Task, error) {
	err := m.begin(ctx)
	defer m.mu.Unlock()
	if err != nil {
		return Task{}, err
	}
	if id < 1 || id > len(m.tasks) || !m.tasks[id-1].Open {
		return Task{}, ErrBadResponse
	}
	m.tasks[id-1].Occurrences++
	return m.tasks[id-1], nil
}

// Close marks a task as closed. It reports whether the task was open.
func (m *Memory) Close(id int) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if id < 1 || id > len(m.tasks) || !m.tasks[id-1].Open {
		return false
	}
	m.tasks[id-1].Open = false
	return true
}

// All returns a copy of every task, open and closed, in the order opened.
func (m *Memory) All() []Task {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Task, len(m.tasks))
	copy(out, m.tasks)
	return out
}
