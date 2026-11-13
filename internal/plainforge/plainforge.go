// Package plainforge is the small interface Bellbuoy needs from Plainforge,
// the cluster-private forge, and an in-memory stand-in for it.
//
// Plainforge itself is a separate component and is not built here.
package plainforge

import "context"

// Task is one piece of work in a repository. The specification also calls
// this an issue or a ticket.
type Task struct {
	ID          string `json:"id"`
	Repo        string `json:"repo"`
	Key         string `json:"key"`
	Title       string `json:"title"`
	Body        string `json:"body"`
	Occurrences int    `json:"occurrences"`
	Open        bool   `json:"open"`
}

// TaskDraft is what Bellbuoy supplies to open a task. Key is how the task is
// found again: a fingerprint for a failure, an edit reference for an edit.
type TaskDraft struct {
	Key   string `json:"key"`
	Title string `json:"title"`
	Body  string `json:"body"`
}

// Change is one file replaced on its own branch, with a request to merge it.
type Change struct {
	Repo    string `json:"repo"`
	Branch  string `json:"branch"`
	Path    string `json:"path"`
	Content string `json:"content"`
	Title   string `json:"title"`
	Message string `json:"message"`
}

// MirrorRequest asks for a repository's public mirror to be brought up to
// date, for the named kinds of content only.
type MirrorRequest struct {
	Repo    string   `json:"repo"`
	Content []string `json:"content"`
}

// Forge is everything Bellbuoy asks of Plainforge.
//
// An implementation reports trouble with the public codes: E_TIMEOUT when
// Plainforge did not answer in time, E_PARSE when its answer made no sense.
type Forge interface {
	// FindOpenTask returns the open task in repo with this key, if any.
	FindOpenTask(ctx context.Context, repo, key string) (Task, bool, error)
	// OpenTask opens a task with an occurrence count of one.
	OpenTask(ctx context.Context, repo string, draft TaskDraft) (Task, error)
	// AddOccurrence adds one to a task's occurrence count.
	AddOccurrence(ctx context.Context, repo, taskID string) (Task, error)
	// ProposeChange commits one file on a branch and opens a request to
	// merge it. Asking again with the same change does nothing and reports
	// created as false. It never writes to the default branch.
	ProposeChange(ctx context.Context, change Change) (created bool, err error)
	// SyncMirror asks Plainforge to push the named kinds of content to the
	// repository's public mirror.
	SyncMirror(ctx context.Context, repo string, content []string) error
}
