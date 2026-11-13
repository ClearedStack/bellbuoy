// Package internalgit is the internal-git sub-tool. It turns failures,
// paused items and owner-approved Portal edits into work in the Plainforge
// repository that owns the fix.
package internalgit

import (
	"github.com/ClearedStack/bellbuoy/internal/ownership"
	"github.com/ClearedStack/bellbuoy/internal/plainforge"
	"github.com/ClearedStack/bellbuoy/internal/work"
)

// Name is this sub-tool's name in the registry.
const Name = "internal-git"

// Tool is the internal-git sub-tool.
type Tool struct {
	Forge  plainforge.Forge
	Owners *ownership.Map
}

var _ work.Tool = (*Tool)(nil)

// Name returns the registry name.
func (*Tool) Name() string { return Name }

// Gate is true: the sub-tools after this one decide what to do from the task
// or change it records.
func (*Tool) Gate() bool { return true }

// Match is the matching rule. internal-git takes every failure (error,
// crash, dead_letter, needs_subtool) and every approved edit.
func (*Tool) Match(job work.Job, _ work.State) bool {
	return work.IsFailure(job.Kind) || job.Kind == work.KindEdit
}
