// Package portalsse is the portal-sse sub-tool. It feeds the live update
// stream that the public Portal shows, by handing events to Muster.
//
// Bellbuoy does not serve the stream itself. Muster carries each event to the
// component that does.
package portalsse

import (
	"github.com/ClearedStack/bellbuoy/internal/muster"
	"github.com/ClearedStack/bellbuoy/internal/work"
)

// Name is this sub-tool's name in the registry.
const Name = "portal-sse"

// Tool is the portal-sse sub-tool.
type Tool struct {
	Queue muster.Queue
}

var _ work.Tool = (*Tool)(nil)

// Name returns the registry name.
func (*Tool) Name() string { return Name }

// Gate is false: nothing later reads what this sub-tool does.
func (*Tool) Gate() bool { return false }

// Match is the matching rule. portal-sse takes:
//
//   - every status job, which reports that an edit moved on;
//   - an edit job once internal-git has turned it into a task or a change;
//   - a failure once internal-git has counted it on a task.
func (*Tool) Match(job work.Job, st work.State) bool {
	switch {
	case job.Kind == work.KindStatus:
		return true
	case job.Kind == work.KindEdit:
		return st.Task != nil || st.Change != nil
	case work.IsFailure(job.Kind):
		return st.Task != nil
	}
	return false
}
