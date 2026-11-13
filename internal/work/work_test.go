package work

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/ClearedStack/bellbuoy/internal/codes"
)

func code(err error) codes.Code {
	if e := codes.From(err); e != nil {
		return e.Code
	}
	return ""
}

func TestDecode(t *testing.T) {
	type payload struct {
		Name  string  `json:"name"`
		Count int     `json:"count,omitempty"`
		Note  *string `json:"note"`
	}
	cases := []struct {
		raw  string
		want codes.Code
	}{
		{`{"name":"a"}`, ""},
		{`{"name":"a","count":2,"note":"x"}`, ""},
		{` {"name":"a"} `, ""},
		{``, codes.Parse},
		{`   `, codes.Parse},
		{`null`, codes.Parse},
		{`[]`, codes.Parse},
		{`"text"`, codes.Parse},
		{`{"name":`, codes.Parse},
		{`{"name":"a"} {"name":"b"}`, codes.Parse},
		{`{"name":"a"} trailing`, codes.Parse},
		{`{"name":7}`, codes.Parse},
		{`{"name":"a","count":"two"}`, codes.Parse},
		{`{"name":"a","extra":1}`, codes.NeedsSubtool},
		{`{"Name":"a"}`, codes.NeedsSubtool},
		{`{"name":7,"extra":1}`, codes.Parse},
	}
	for _, c := range cases {
		var p payload
		if got := code(Decode(json.RawMessage(c.raw), &p)); got != c.want {
			t.Errorf("Decode(%q) = %q, want %q", c.raw, got, c.want)
		}
	}
}

func TestJobCheck(t *testing.T) {
	good := Job{ID: "job-1", Lane: LaneNormal, Kind: KindError, Payload: json.RawMessage(`{}`)}
	if err := good.Check(); err != nil {
		t.Fatal(err)
	}
	bad := []Job{
		{ID: "", Lane: LaneNormal, Kind: KindError},
		{ID: "has space", Lane: LaneNormal, Kind: KindError},
		{ID: "job-1", Lane: "soon", Kind: KindError},
		{ID: "job-1", Lane: LaneNormal, Kind: ""},
		{ID: "job-1", Lane: LaneNormal, Kind: "Error"},
		{ID: "job-1", Lane: LaneNormal, Kind: KindError, Payload: json.RawMessage(strings.Repeat(" ", MaxPayload+1))},
	}
	for _, j := range bad {
		if code(j.Check()) != codes.Parse {
			t.Errorf("Check(%+v) did not report E_PARSE", j.ID+"/"+j.Lane+"/"+j.Kind)
		}
	}
}

func TestLanesOrder(t *testing.T) {
	got := strings.Join(Lanes(), ",")
	if got != "urgent,normal,backfill" {
		t.Fatalf("lanes are drained in the wrong order: %s", got)
	}
}

func TestIsFailure(t *testing.T) {
	for _, k := range []string{KindError, KindCrash, KindDeadLetter, KindNeedsSubtool} {
		if !IsFailure(k) {
			t.Errorf("%s is a failure", k)
		}
	}
	for _, k := range []string{KindEdit, KindStatus, KindDrift, KindMirror, "other"} {
		if IsFailure(k) {
			t.Errorf("%s is not a failure", k)
		}
	}
}
