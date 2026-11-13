package githubmirror

import (
	"context"
	"sort"

	"github.com/ClearedStack/bellbuoy/internal/codes"
	"github.com/ClearedStack/bellbuoy/internal/sanitize"
	"github.com/ClearedStack/bellbuoy/internal/work"
)

// The only kinds of content that may leave for the public mirror.
const (
	ContentSource = "source"
	ContentTask   = "task"
)

// mirror is the payload of a mirror job: a repository changed.
type mirror struct {
	Repo    string   `json:"repo"`
	Content []string `json:"content"`
}

// Run asks Plainforge to bring one repository's mirror up to date.
func (t *Tool) Run(ctx context.Context, job work.Job, st *work.State) error {
	repo, content, err := t.plan(job, *st)
	if err != nil {
		return err
	}
	return t.Forge.SyncMirror(ctx, repo, content)
}

// plan works out which repository to mirror and which kinds of content. It
// looks at nothing outside the process.
func (t *Tool) plan(job work.Job, st work.State) (repo string, content []string, err error) {
	if job.Kind != work.KindMirror {
		// internal-git has just written. A task is task content. A proposed
		// change is source.
		if st.Task != nil {
			return st.Task.Repo, []string{ContentTask}, nil
		}
		return st.Change.Repo, []string{ContentSource}, nil
	}

	var m mirror
	if err := work.Decode(job.Payload, &m); err != nil {
		return "", nil, err
	}
	if !sanitize.IsRepo(m.Repo) {
		return "", nil, codes.Broken("repository name is missing or malformed")
	}
	if len(m.Content) == 0 {
		return "", nil, codes.Broken("mirror job does not say what content changed")
	}
	seen := map[string]bool{}
	for _, c := range m.Content {
		switch {
		case c == ContentSource || c == ContentTask:
			seen[c] = true
		case sanitize.IsKind(c):
			// Images, secrets, captures and anything else not on the list
			// stay inside. The whole job is paused: sending part of it
			// would be a guess.
			return "", nil, codes.Unfamiliar("this kind of content may not be mirrored")
		default:
			return "", nil, codes.Broken("mirror job names malformed content")
		}
	}
	if !t.Owners.Mirrored(m.Repo) {
		return "", nil, codes.Unfamiliar("this repository has no public mirror")
	}
	for c := range seen {
		content = append(content, c)
	}
	sort.Strings(content)
	return m.Repo, content, nil
}
