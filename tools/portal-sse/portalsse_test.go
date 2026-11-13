package portalsse

import (
	"testing"

	"github.com/ClearedStack/bellbuoy/internal/fixture"
	"github.com/ClearedStack/bellbuoy/internal/work"
)

func TestFixtures(t *testing.T) {
	fixture.Run(t, "fixtures", "../../testdata/ownership_map.json", func(d fixture.Deps) work.Tool {
		return &Tool{Queue: d.Queue}
	})
}
