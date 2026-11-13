package internalgit

import (
	"context"
	"errors"
	"sync"

	"github.com/clearedstack/bellbuoy/internal/errcode"
	"github.com/clearedstack/bellbuoy/internal/fingerprint"
	"github.com/clearedstack/bellbuoy/internal/plainforge"
)

// What happened to one item.
const (
	// ActionOpened: no open task had this fingerprint, so one was opened.
	ActionOpened = "opened"
	// ActionAdded: an open task had this fingerprint; its count went up by one.
	ActionAdded = "added"
	// ActionPaused: no rule covers the item (E_NEEDS_SUBTOOL). Nothing was written.
	ActionPaused = "paused"
	// ActionRefused: the item was broken (E_PARSE). Nothing was written.
	ActionRefused = "refused"
	// ActionNotDelivered: the task store took too long (E_TIMEOUT).
	ActionNotDelivered = "not_delivered"
)

// Outcome is the exact result of handling one item.
type Outcome struct {
	Action      string         `json:"action"`
	Task        int            `json:"task,omitempty"`
	Occurrences int            `json:"occurrences,omitempty"`
	Fingerprint string         `json:"fingerprint,omitempty"`
	Code        errcode.Code   `json:"code,omitempty"`
	Reason      errcode.Reason `json:"reason,omitempty"`
}

// Router applies the open-or-add-to rule against one task store.
type Router struct {
	// mu makes "look for an open task, then open one or add to it" a single
	// step inside this process, so two identical failures arriving together
	// cannot both open a task.
	mu    sync.Mutex
	tasks plainforge.Tasks
}

// NewRouter returns a Router that writes to tasks.
func NewRouter(tasks plainforge.Tasks) *Router {
	return &Router{tasks: tasks}
}

// Handle deals with one item and reports what happened. It never returns
// text taken from the item or from the store.
func (r *Router) Handle(ctx context.Context, raw []byte) Outcome {
	f, perr := Match(raw)
	if perr != nil {
		return failed(perr)
	}
	fp, perr := fingerprint.Of(f.Worker, f.Code, f.Pattern)
	if perr != nil {
		return failed(perr)
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	existing, found, err := r.tasks.FindOpen(ctx, fp)
	if err != nil {
		return failed(storeError(err))
	}
	if found {
		t, err := r.tasks.AddOccurrence(ctx, existing.ID)
		if err != nil {
			return failed(storeError(err))
		}
		return Outcome{Action: ActionAdded, Task: t.ID, Occurrences: t.Occurrences, Fingerprint: fp}
	}
	t, err := r.tasks.Open(ctx, plainforge.NewTask{
		Title:       f.Worker + ": " + f.Code + " " + f.Pattern,
		Body:        "Worker: " + f.Worker + "\nError code: " + f.Code + "\nFailure pattern: " + f.Pattern + "\nFingerprint: " + fp + "\n",
		Fingerprint: fp,
	})
	if err != nil {
		return failed(storeError(err))
	}
	return Outcome{Action: ActionOpened, Task: t.ID, Occurrences: t.Occurrences, Fingerprint: fp}
}

func failed(e *errcode.Error) Outcome {
	action := ActionRefused
	switch e.Code {
	case errcode.NeedsSubtool:
		action = ActionPaused
	case errcode.Timeout:
		action = ActionNotDelivered
	}
	return Outcome{Action: action, Code: e.Code, Reason: e.Reason}
}

// storeError turns a task-store error into a public one. The store's own
// text is dropped on purpose.
func storeError(err error) *errcode.Error {
	if errors.Is(err, plainforge.ErrTimeout) || errors.Is(err, context.DeadlineExceeded) {
		return errcode.New(errcode.Timeout, errcode.ReasonStoreTimeout)
	}
	return errcode.New(errcode.Parse, errcode.ReasonStoreBadResponse)
}
