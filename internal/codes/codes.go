// Package codes holds the three public error codes and the only error type
// Bellbuoy lets out of the process.
//
// A reason is always a constant written in the source. The constructors take
// an unexported string type, so a caller in another package can pass a string
// literal and nothing else: text that arrived from outside (a host name, an
// address, a credential) cannot be put into an error by accident.
package codes

import (
	"context"
	"errors"
	"net"
)

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

// Lookup reports whether s is exactly one of the three public codes.
func Lookup(s string) (Code, bool) {
	switch Code(s) {
	case Timeout, Parse, NeedsSubtool:
		return Code(s), true
	}
	return "", false
}

// fixed is the type of a reason. It is unexported on purpose: see the package
// comment.
type fixed string

// Error is a public code with a fixed reason.
type Error struct {
	Code   Code   `json:"code"`
	Reason string `json:"reason"`
}

func (e *Error) Error() string { return string(e.Code) + ": " + e.Reason }

// TimedOut reports that something outside took too long.
func TimedOut(reason fixed) *Error { return &Error{Code: Timeout, Reason: string(reason)} }

// Broken reports that the data was broken.
func Broken(reason fixed) *Error { return &Error{Code: Parse, Reason: string(reason)} }

// Unfamiliar reports that the data was valid and no rule covers it.
func Unfamiliar(reason fixed) *Error { return &Error{Code: NeedsSubtool, Reason: string(reason)} }

// From turns any error into a public one. The text of a foreign error is
// never kept, because it may name a machine or an address.
func From(err error) *Error {
	if err == nil {
		return nil
	}
	var e *Error
	if errors.As(err, &e) && e != nil {
		return e
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return TimedOut("the outside call took too long")
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return TimedOut("the outside call took too long")
	}
	return Broken("the outside answer was not understood")
}
