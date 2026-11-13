// Package githubmirror is the github-mirror sub-tool. It asks Plainforge to
// bring a repository's public mirror up to date when the repository changes.
//
// Bellbuoy holds no credential for the public host and pushes nothing there
// itself. Only two kinds of content may be mirrored: source and tasks.
package githubmirror

import (
	"github.com/ClearedStack/bellbuoy/internal/ownership"
	"github.com/ClearedStack/bellbuoy/internal/plainforge"
	"github.com/ClearedStack/bellbuoy/internal/work"
)

// Name is this sub-tool's name in the registry.
const Name = "github-mirror"

// Tool is the github-mirror sub-tool.
type Tool struct {
	Forge  plainforge.Forge
	Owners *ownership.Map
}

var _ work.Tool = (*Tool)(nil)

// Name returns the registry name.
func (*Tool) Name() string { return Name }

// Gate is false: nothing later reads what this sub-tool does.
func (*Tool) Gate() bool { return false }

// Match is the matching rule. github-mirror takes every mirror job, and any
// job for which internal-git has just written to a repository that has a
// public mirror. A write to a repository without a mirror is not matched:
// there is nothing to bring up to date.
func (t *Tool) Match(job work.Job, st work.State) bool {
	if job.Kind == work.KindMirror {
		return true
	}
	if st.Task != nil && t.Owners.Mirrored(st.Task.Repo) {
		return true
	}
	return st.Change != nil && t.Owners.Mirrored(st.Change.Repo)
}
