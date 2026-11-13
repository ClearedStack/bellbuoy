// Package plainforge is the small interface Bellbuoy uses to reach
// Plainforge, and an in-memory stand-in for it.
//
// Plainforge does not exist yet. This package is not Plainforge and does not
// try to be: it holds only what the first milestone needs, which is tasks
// that can be found by fingerprint, opened, and counted.
package plainforge

import (
	"context"
	"errors"
)

// Task is one piece of work in the repository that owns the fix.
type Task struct {
	ID          int    `json:"id"`
	Title       string `json:"title"`
	Body        string `json:"body"`
	Fingerprint string `json:"fingerprint"`
	Occurrences int    `json:"occurrences"`
	Open        bool   `json:"open"`
}

// NewTask is what Bellbuoy supplies when it opens a task.
type NewTask struct {
	Title       string
	Body        string
	Fingerprint string
}

// Tasks is the task store of one repository.
type Tasks interface {
	// FindOpen returns the open task with this fingerprint, if there is one.
	FindOpen(ctx context.Context, fingerprint string) (Task, bool, error)
	// Open creates a task. It starts open with one occurrence.
	Open(ctx context.Context, t NewTask) (Task, error)
	// AddOccurrence adds one to the occurrence count of an open task.
	AddOccurrence(ctx context.Context, id int) (Task, error)
}

var (
	// ErrTimeout means the store took too long.
	ErrTimeout = errors.New("plainforge: timeout")
	// ErrBadResponse means the store answered with something unexpected.
	ErrBadResponse = errors.New("plainforge: unexpected response")
)
