package internalgit

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/ClearedStack/bellbuoy/internal/codes"
	"github.com/ClearedStack/bellbuoy/internal/fingerprint"
	"github.com/ClearedStack/bellbuoy/internal/plainforge"
	"github.com/ClearedStack/bellbuoy/internal/sanitize"
	"github.com/ClearedStack/bellbuoy/internal/tasks"
	"github.com/ClearedStack/bellbuoy/internal/work"
)

// The three classes of Portal edit.
const (
	ClassData   = "DATA"
	ClassConfig = "CONFIG"
	ClassIssue  = "ISSUE"
)

const (
	maxContent = 256 << 10 // bytes of file content in a DATA or CONFIG edit
	maxSummary = 4000      // bytes of text in an ISSUE edit
)

// failure is the payload of an error, crash, dead_letter or needs_subtool
// job.
type failure struct {
	Worker  string `json:"worker"`
	Code    string `json:"code"`
	Pattern string `json:"pattern"`
	Item    string `json:"item"`
	Tier    string `json:"tier"`
}

// incidentLease is what Muster's lease of an incident step carries besides
// the fields every lease has.
type incidentLease struct {
	Incident json.RawMessage `json:"incident"`
}

// incident says that a worker stopped on an item: the item, the worker, the
// code it gave (E_PARSE for a held item, E_NEEDS_SUBTOOL for a paused one)
// and the state the item was in when the worker stopped.
type incident struct {
	ID     string `json:"id"`
	Worker string `json:"worker"`
	Code   string `json:"code"`
	State  string `json:"state"`
}

// edit is the payload of an edit job: a Portal edit an owner has approved.
type edit struct {
	Edit     string  `json:"edit"`
	Class    string  `json:"class"`
	Target   string  `json:"target"`
	Approval string  `json:"approval"`
	Content  *string `json:"content"`
	Summary  *string `json:"summary"`
}

// Run does the work for one job. Everything is checked before anything is
// written, so a job that is refused leaves the repository untouched.
func (t *Tool) Run(ctx context.Context, job work.Job, st *work.State) error {
	switch job.Kind {
	case work.KindEdit:
		return t.runEdit(ctx, job, st)
	case work.KindIncident:
		return t.runIncident(ctx, job, st)
	}
	return t.runFailure(ctx, job, st)
}

// runIncident files the task for an item Muster holds or has paused. Muster
// gives no failure message, so the pattern is made from what it does give:
// whether the item was paused or held, and the state it is in. Items that
// the same worker left the same way in the same state share one task.
func (t *Tool) runIncident(ctx context.Context, job work.Job, st *work.State) error {
	var lease incidentLease
	if err := work.Decode(job.Payload, &lease); err != nil {
		return err
	}
	var in incident
	if err := work.Decode(lease.Incident, &in); err != nil {
		return err
	}
	if !sanitize.IsItem(job.ID) || in.ID != job.ID {
		return codes.Broken("item reference is malformed or does not match the step")
	}
	if !sanitize.IsKind(in.State) {
		return codes.Broken("item state is missing or malformed")
	}
	var verb string
	switch in.Code {
	case string(codes.Parse):
		verb = "held in "
	case string(codes.NeedsSubtool):
		verb = "paused in "
	case string(codes.Timeout):
		return codes.Unfamiliar("no rule covers an incident with this code")
	}
	return t.file(ctx, st, failure{
		Worker:  in.Worker,
		Code:    in.Code,
		Pattern: verb + strings.ReplaceAll(in.State, "_", " "),
		Item:    job.ID,
	}, work.KindError)
}

func (t *Tool) runFailure(ctx context.Context, job work.Job, st *work.State) error {
	var f failure
	if err := work.Decode(job.Payload, &f); err != nil {
		return err
	}
	return t.file(ctx, st, f, job.Kind)
}

// file checks a failure and opens or adds to its task.
func (t *Tool) file(ctx context.Context, st *work.State, f failure, kind string) error {
	if !sanitize.IsWorker(f.Worker) {
		return codes.Broken("worker name is missing or malformed")
	}
	code, err := failureCode(kind, f.Code)
	if err != nil {
		return err
	}
	if f.Item != "" && !sanitize.IsItem(f.Item) {
		return codes.Broken("item reference is malformed")
	}
	if f.Tier != "" && !sanitize.IsTier(f.Tier) {
		return codes.Broken("attention tier is malformed")
	}
	repo, ok := t.Owners.RepoForWorker(f.Worker)
	if !ok {
		return codes.Unfamiliar("no repository is recorded as owning this worker")
	}

	fp := fingerprint.Of(f.Worker, code, f.Pattern)
	body := []string{
		"Worker: " + f.Worker,
		"Error code: " + string(code),
		"Fingerprint: " + fp,
		"Failure pattern: " + sanitize.Pattern(f.Pattern),
	}
	if f.Item != "" {
		body = append(body, "First seen on item: "+f.Item)
	}
	body = append(body, "", "Opened by bellbuoy. Repeats of this failure are counted on this task.")
	draft := plainforge.TaskDraft{
		Key:   fp,
		Title: f.Worker + ": " + string(code) + " [" + fingerprint.Short(fp) + "]",
		Body:  strings.Join(body, "\n"),
	}

	task, created, err := tasks.OpenOrAdd(ctx, t.Forge, repo, draft)
	if err != nil {
		return err
	}
	st.Task = &work.TaskResult{
		Repo:        repo,
		ID:          task.ID,
		Created:     created,
		Occurrences: task.Occurrences,
		Worker:      f.Worker,
		Code:        string(code),
		Fingerprint: fp,
		Tier:        f.Tier,
	}
	return nil
}

// failureCode checks the error code a failure carries. Only the three public
// codes are known. A code of the right shape that is not one of them is
// valid and unfamiliar. A needs_subtool job may leave the code out.
func failureCode(kind, raw string) (codes.Code, error) {
	if kind == work.KindNeedsSubtool {
		if raw == "" || raw == string(codes.NeedsSubtool) {
			return codes.NeedsSubtool, nil
		}
		return "", codes.Broken("a needs_subtool job carries a different error code")
	}
	if code, ok := codes.Lookup(raw); ok {
		return code, nil
	}
	if sanitize.IsCodeShaped(raw) {
		return "", codes.Unfamiliar("error code is not one of the three public codes")
	}
	return "", codes.Broken("error code is missing or malformed")
}

func (t *Tool) runEdit(ctx context.Context, job work.Job, st *work.State) error {
	var e edit
	if err := work.Decode(job.Payload, &e); err != nil {
		return err
	}
	if !sanitize.IsRef(e.Edit) {
		return codes.Broken("edit reference is missing or malformed")
	}
	if !sanitize.IsRef(e.Approval) {
		return codes.Broken("edit carries no owner approval")
	}
	if !sanitize.IsPath(e.Target) {
		return codes.Broken("edit target is missing or malformed")
	}
	switch e.Class {
	case ClassData, ClassConfig:
		if e.Content == nil || e.Summary != nil {
			return codes.Broken("a DATA or CONFIG edit must carry content and nothing else")
		}
		if !sanitize.IsText(*e.Content, maxContent) {
			return codes.Broken("edit content is too large or is not plain text")
		}
	case ClassIssue:
		if e.Summary == nil || e.Content != nil {
			return codes.Broken("an ISSUE edit must carry a summary and nothing else")
		}
		if strings.TrimSpace(*e.Summary) == "" || !sanitize.IsText(*e.Summary, maxSummary) {
			return codes.Broken("edit summary is empty, too large or is not plain text")
		}
	default:
		if sanitize.IsLetters(e.Class, 16) && strings.ToUpper(e.Class) == e.Class {
			return codes.Unfamiliar("no rule covers this class of edit")
		}
		return codes.Broken("edit class is missing or malformed")
	}
	repo, ok := t.Owners.RepoForPath(e.Target)
	if !ok {
		return codes.Unfamiliar("no repository is recorded as owning this edit target")
	}

	title := "Portal edit " + e.Edit + ": " + e.Target

	if e.Class == ClassIssue {
		draft := plainforge.TaskDraft{
			Key:   "edit-" + e.Edit,
			Title: title,
			Body: strings.Join([]string{
				strings.TrimSpace(*e.Summary),
				"",
				"Target: " + e.Target,
				"Approval: " + e.Approval,
				"",
				"Opened by bellbuoy from an owner-approved Portal edit.",
			}, "\n"),
		}
		task, created, err := tasks.OpenOnce(ctx, t.Forge, repo, draft)
		if err != nil {
			return err
		}
		st.Task = &work.TaskResult{Repo: repo, ID: task.ID, Created: created, Occurrences: task.Occurrences, Edit: e.Edit}
		return nil
	}

	change := plainforge.Change{
		Repo:    repo,
		Branch:  "edit/" + e.Edit,
		Path:    e.Target,
		Content: *e.Content,
		Title:   title,
		Message: title + "\n\nClass: " + e.Class + "\nApproval: " + e.Approval,
	}
	created, err := t.Forge.ProposeChange(ctx, change)
	if err != nil {
		return err
	}
	st.Change = &work.ChangeResult{Repo: repo, Branch: change.Branch, Edit: e.Edit, Created: created}
	return nil
}
