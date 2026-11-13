// Package tasks holds the rule that keeps repeats of one failure on one task.
package tasks

import (
	"context"

	"github.com/ClearedStack/bellbuoy/internal/plainforge"
)

// OpenOrAdd is the open-or-add-to rule. If the repository has an open task
// with the draft's key, one is added to that task's occurrence count and
// created is false. Otherwise a task is opened and created is true.
//
// A closed task does not count: the same failure coming back after its task
// was closed opens a new one.
func OpenOrAdd(ctx context.Context, forge plainforge.Forge, repo string, draft plainforge.TaskDraft) (task plainforge.Task, created bool, err error) {
	existing, found, err := forge.FindOpenTask(ctx, repo, draft.Key)
	if err != nil {
		return plainforge.Task{}, false, err
	}
	if found {
		task, err = forge.AddOccurrence(ctx, repo, existing.ID)
		return task, false, err
	}
	task, err = forge.OpenTask(ctx, repo, draft)
	return task, err == nil, err
}

// OpenOnce opens a task unless an open one with the same key exists, in which
// case nothing changes. It is for work that must not be counted twice when
// the same job is delivered again.
func OpenOnce(ctx context.Context, forge plainforge.Forge, repo string, draft plainforge.TaskDraft) (task plainforge.Task, created bool, err error) {
	existing, found, err := forge.FindOpenTask(ctx, repo, draft.Key)
	if err != nil {
		return plainforge.Task{}, false, err
	}
	if found {
		return existing, false, nil
	}
	task, err = forge.OpenTask(ctx, repo, draft)
	return task, err == nil, err
}
