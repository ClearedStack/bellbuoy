// Package internalgit is the internal-git sub-tool: it turns routed items
// into work in Plainforge.
//
// Only one rule is built so far: a fingerprinted error becomes one task, and
// the same error seen again adds to that task. Approved edits and paused
// needs_subtool items are named by the specification and have no rule here
// yet, so they are paused, not guessed at.
package internalgit

import (
	"encoding/json"

	"github.com/clearedstack/bellbuoy/internal/errcode"
)

// KindError is the kind of item the error-task rule covers.
const KindError = "error"

// MaxItemBytes is the largest item accepted.
const MaxItemBytes = 16 << 10

// Failure is one reported failure: which worker failed, with which public
// error code, in which recurring way.
type Failure struct {
	Worker  string
	Code    string
	Pattern string
}

// Match is the matching rule. It reads one item and decides whether the
// error-task rule covers it.
//
//   - Broken data (not one JSON object, a missing or non-string field) gives
//     E_PARSE.
//   - Valid data that no rule covers (another kind, an extra field) gives
//     E_NEEDS_SUBTOOL.
//
// The worker, code and pattern values are checked later, when the
// fingerprint is made.
func Match(raw []byte) (Failure, *errcode.Error) {
	if len(raw) > MaxItemBytes {
		return Failure{}, errcode.New(errcode.Parse, errcode.ReasonItemTooLarge)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return Failure{}, errcode.New(errcode.Parse, errcode.ReasonNotJSONObject)
	}
	kind, ok := stringField(fields, "kind")
	if !ok {
		return Failure{}, errcode.New(errcode.Parse, errcode.ReasonKindMissing)
	}
	if kind != KindError {
		return Failure{}, errcode.New(errcode.NeedsSubtool, errcode.ReasonKindNoRule)
	}
	var f Failure
	var okW, okC, okP bool
	f.Worker, okW = stringField(fields, "worker")
	f.Code, okC = stringField(fields, "code")
	f.Pattern, okP = stringField(fields, "pattern")
	if !okW || !okC || !okP {
		return Failure{}, errcode.New(errcode.Parse, errcode.ReasonFieldMissing)
	}
	if len(fields) != 4 {
		return Failure{}, errcode.New(errcode.NeedsSubtool, errcode.ReasonFieldUnfamiliar)
	}
	return f, nil
}

func stringField(fields map[string]json.RawMessage, name string) (string, bool) {
	raw, ok := fields[name]
	if !ok {
		return "", false
	}
	var s *string
	if err := json.Unmarshal(raw, &s); err != nil || s == nil {
		return "", false
	}
	return *s, true
}
