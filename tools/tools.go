// Package tools lists the sub-tools in the order the router tries them. The
// same list is written down in registry.yaml, and a test keeps the two, and
// component.json, in step.
package tools

import (
	"github.com/ClearedStack/bellbuoy/internal/muster"
	"github.com/ClearedStack/bellbuoy/internal/ownership"
	"github.com/ClearedStack/bellbuoy/internal/plainforge"
	"github.com/ClearedStack/bellbuoy/internal/work"
	githubmirror "github.com/ClearedStack/bellbuoy/tools/github-mirror"
	internalgit "github.com/ClearedStack/bellbuoy/tools/internal-git"
	portalsse "github.com/ClearedStack/bellbuoy/tools/portal-sse"
	webhookalert "github.com/ClearedStack/bellbuoy/tools/webhook-alert"
)

// Deps is everything the sub-tools reach outside through.
type Deps struct {
	Forge  plainforge.Forge
	Owners *ownership.Map
	// Sender is nil when no webhook is configured.
	Sender webhookalert.Sender
	// Stream is nil when there is nowhere to publish live events.
	Stream muster.Publisher
}

// All returns the four sub-tools in registry order.
func All(d Deps) []work.Tool {
	return []work.Tool{
		&internalgit.Tool{Forge: d.Forge, Owners: d.Owners},
		&githubmirror.Tool{Forge: d.Forge, Owners: d.Owners},
		&webhookalert.Tool{Sender: d.Sender},
		&portalsse.Tool{Stream: d.Stream},
	}
}

// Only returns the sub-tools with the given names, in registry order.
func Only(all []work.Tool, names ...string) []work.Tool {
	keep := map[string]bool{}
	for _, name := range names {
		keep[name] = true
	}
	var out []work.Tool
	for _, tool := range all {
		if keep[tool.Name()] {
			out = append(out, tool)
		}
	}
	return out
}
