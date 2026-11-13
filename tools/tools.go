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
	Queue  muster.Queue
	Forge  plainforge.Forge
	Owners *ownership.Map
	// Sender is nil when no webhook is configured.
	Sender webhookalert.Sender
}

// All returns the four sub-tools in registry order.
func All(d Deps) []work.Tool {
	return []work.Tool{
		&internalgit.Tool{Forge: d.Forge, Owners: d.Owners},
		&githubmirror.Tool{Forge: d.Forge, Owners: d.Owners},
		&webhookalert.Tool{Sender: d.Sender},
		&portalsse.Tool{Queue: d.Queue},
	}
}
