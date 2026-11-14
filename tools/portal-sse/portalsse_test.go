package portalsse

import (
	"context"
	"testing"

	"github.com/ClearedStack/bellbuoy/internal/codes"
	"github.com/ClearedStack/bellbuoy/internal/fixture"
	"github.com/ClearedStack/bellbuoy/internal/work"
)

func TestFixtures(t *testing.T) {
	fixture.Run(t, "fixtures", "../../testdata/ownership_map.json", func(d fixture.Deps) work.Tool {
		return &Tool{Stream: d.Queue}
	})
}

func TestNoStreamConfiguredPausesTheJob(t *testing.T) {
	tool := &Tool{}
	job := work.Job{ID: "job-1", Lane: work.LaneNormal, Kind: work.KindStatus, Payload: []byte(`{"topic":"pill","edit":"ed-0001","state":"Live"}`)}
	st := work.State{}
	err := tool.Run(context.Background(), job, &st)
	if e := codes.From(err); e == nil || e.Code != codes.NeedsSubtool {
		t.Fatalf("err = %v, want E_NEEDS_SUBTOOL", err)
	}
}
