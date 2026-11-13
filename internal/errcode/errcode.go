// Package errcode holds the three public error codes of Bellbuoy and the
// only error type that may leave the process.
//
// A public error carries a code and a fixed reason chosen from the constants
// below. It never carries text taken from the input or from a dependency, so
// nothing that names a machine, an address or a credential can leak through it.
package errcode

// Code is one of the three public error codes.
type Code string

const (
	// Timeout: something outside took too long.
	Timeout Code = "E_TIMEOUT"
	// Parse: the data was broken.
	Parse Code = "E_PARSE"
	// NeedsSubtool: the data was valid and unfamiliar.
	NeedsSubtool Code = "E_NEEDS_SUBTOOL"
)

// Known reports whether s is exactly one of the three public codes.
func Known(s string) bool {
	switch Code(s) {
	case Timeout, Parse, NeedsSubtool:
		return true
	}
	return false
}

// Reason is a fixed, input-free explanation. Only the constants declared in
// this file are meant to be used; never build a Reason from other text.
type Reason string

const (
	ReasonItemTooLarge     Reason = "item is larger than the limit"
	ReasonNotJSONObject    Reason = "item is not a single JSON object"
	ReasonKindMissing      Reason = "kind is missing or is not a string"
	ReasonKindNoRule       Reason = "no rule covers this kind of item"
	ReasonFieldMissing     Reason = "a required field is missing or is not a string"
	ReasonFieldUnfamiliar  Reason = "item has a field no rule covers"
	ReasonWorkerMalformed  Reason = "worker name is malformed"
	ReasonCodeMalformed    Reason = "error code is malformed"
	ReasonCodeUnfamiliar   Reason = "error code is well formed and not one of the three public codes"
	ReasonPatternMalformed Reason = "failure pattern is malformed"
	ReasonStoreTimeout     Reason = "task store took too long"
	ReasonStoreBadResponse Reason = "task store gave an unexpected response"
)

// Error is the only error type Bellbuoy shows to the outside.
type Error struct {
	Code   Code
	Reason Reason
}

// New builds a public error.
func New(code Code, reason Reason) *Error {
	return &Error{Code: code, Reason: reason}
}

func (e *Error) Error() string {
	return string(e.Code) + ": " + string(e.Reason)
}
