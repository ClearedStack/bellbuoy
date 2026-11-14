package plainforge

import (
	"context"
	"strconv"
	"sync"

	"github.com/ClearedStack/bellbuoy/internal/codes"
)

// Memory is an in-memory stand-in for Plainforge. It is safe for use by
// several goroutines.
type Memory struct {
	mu      sync.Mutex
	tasks   []Task
	next    map[string]int
	changes []Change
	mirrors []MirrorRequest
	fault   error
}

// NewMemory returns an empty stand-in.
func NewMemory() *Memory { return &Memory{next: map[string]int{}} }

var _ Forge = (*Memory)(nil)

// SetFault makes every later call fail with err. A nil err clears it.
func (m *Memory) SetFault(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.fault = err
}

// Seed puts existing tasks into the store, as they are.
func (m *Memory) Seed(tasks []Task) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, t := range tasks {
		m.tasks = append(m.tasks, t)
		m.next[t.Repo]++
	}
}

// snapshot is everything a Memory holds that outlives a call.
type snapshot struct {
	Tasks   []Task   `json:"tasks"`
	Changes []Change `json:"changes"`
}

func (m *Memory) snapshot() snapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	return snapshot{Tasks: append([]Task{}, m.tasks...), Changes: append([]Change{}, m.changes...)}
}

// restore replaces what the Memory holds. Task numbers carry on from the
// number of tasks each repository has.
func (m *Memory) restore(s snapshot) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.tasks = append([]Task(nil), s.Tasks...)
	m.changes = append([]Change(nil), s.Changes...)
	m.next = map[string]int{}
	for _, t := range m.tasks {
		m.next[t.Repo]++
	}
}

// CloseTask marks a task as closed and reports whether it was found.
func (m *Memory) CloseTask(repo, id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.tasks {
		if m.tasks[i].Repo == repo && m.tasks[i].ID == id {
			m.tasks[i].Open = false
			return true
		}
	}
	return false
}

// Tasks returns a copy of every task, in the order they were opened.
func (m *Memory) Tasks() []Task {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]Task(nil), m.tasks...)
}

// Changes returns a copy of every proposed change, in order.
func (m *Memory) Changes() []Change {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]Change(nil), m.changes...)
}

// Mirrors returns a copy of every mirror request, in order.
func (m *Memory) Mirrors() []MirrorRequest {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]MirrorRequest(nil), m.mirrors...)
}

func (m *Memory) FindOpenTask(_ context.Context, repo, key string) (Task, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fault != nil {
		return Task{}, false, m.fault
	}
	for _, t := range m.tasks {
		if t.Open && t.Repo == repo && t.Key == key {
			return t, true, nil
		}
	}
	return Task{}, false, nil
}

func (m *Memory) OpenTask(_ context.Context, repo string, draft TaskDraft) (Task, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fault != nil {
		return Task{}, m.fault
	}
	m.next[repo]++
	t := Task{
		ID:          repo + "#" + strconv.Itoa(m.next[repo]),
		Repo:        repo,
		Key:         draft.Key,
		Title:       draft.Title,
		Body:        draft.Body,
		Occurrences: 1,
		Open:        true,
	}
	m.tasks = append(m.tasks, t)
	return t, nil
}

func (m *Memory) AddOccurrence(_ context.Context, repo, taskID string) (Task, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fault != nil {
		return Task{}, m.fault
	}
	for i := range m.tasks {
		if m.tasks[i].Repo == repo && m.tasks[i].ID == taskID && m.tasks[i].Open {
			m.tasks[i].Occurrences++
			return m.tasks[i], nil
		}
	}
	return Task{}, codes.Broken("the task to add to is not open")
}

func (m *Memory) ProposeChange(_ context.Context, change Change) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fault != nil {
		return false, m.fault
	}
	for _, c := range m.changes {
		if c.Repo == change.Repo && c.Branch == change.Branch {
			if c == change {
				return false, nil
			}
			return false, codes.Broken("the branch already holds a different change")
		}
	}
	m.changes = append(m.changes, change)
	return true, nil
}

func (m *Memory) SyncMirror(_ context.Context, repo string, content []string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fault != nil {
		return m.fault
	}
	m.mirrors = append(m.mirrors, MirrorRequest{Repo: repo, Content: append([]string(nil), content...)})
	return nil
}
