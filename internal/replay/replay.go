// Package replay runs a list of saved items, one JSON item per line, through
// the internal-git sub-tool against a fresh in-memory task store, and reports
// exactly what happened. The fixture tests and cmd/replay both use it.
package replay

import (
	"bufio"
	"context"
	"encoding/json"
	"io"

	"github.com/clearedstack/bellbuoy/internal/plainforge"
	internalgit "github.com/clearedstack/bellbuoy/tools/internal-git"
)

// Report is the full result of a replay: one outcome per input line, in
// order, and every task left in the store.
type Report struct {
	Outcomes []internalgit.Outcome `json:"outcomes"`
	Tasks    []plainforge.Task     `json:"tasks"`
}

// Run reads items from in, one per line, until the end. Every line is an
// item, including an empty one.
func Run(ctx context.Context, in io.Reader) (Report, error) {
	store := plainforge.NewMemory()
	router := internalgit.NewRouter(store)
	rep := Report{Outcomes: []internalgit.Outcome{}}

	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		rep.Outcomes = append(rep.Outcomes, router.Handle(ctx, sc.Bytes()))
	}
	if err := sc.Err(); err != nil {
		return Report{}, err
	}
	rep.Tasks = store.All()
	return rep, nil
}

// JSON returns the report as indented JSON ending in a newline. This is the
// exact form saved in the fixtures.
func (r Report) JSON() ([]byte, error) {
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}
