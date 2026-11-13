// Package fingerprint computes the repeatable fingerprint that Bellbuoy uses
// to recognise the same failure again.
//
// The fingerprint is made from exactly three things: the worker name, the
// public error code, and the failure pattern. Nothing else goes in: no time,
// no item identifier, no host. The same three values give the same
// fingerprint on any machine.
package fingerprint

import (
	"crypto/sha256"
	"encoding/hex"

	"github.com/clearedstack/bellbuoy/internal/errcode"
)

// version is hashed in front of the three values. Changing how a fingerprint
// is made means changing this line, which changes every fingerprint and fails
// the saved examples.
const version = "bellbuoy-fingerprint-v1"

// MaxNameLen is the longest worker name or failure pattern accepted.
const MaxNameLen = 64

// Of returns the fingerprint of a failure as 64 lowercase hexadecimal
// characters.
//
// worker and pattern must each be a plain name: lowercase letters, digits and
// single hyphens, starting with a letter, ending with a letter or digit, at
// most MaxNameLen characters. code must be one of the three public codes.
//
// A value that is broken gives E_PARSE. A code that is well formed but is not
// one of the three gives E_NEEDS_SUBTOOL. Of never repairs a value.
func Of(worker, code, pattern string) (string, *errcode.Error) {
	if !plainName(worker) {
		return "", errcode.New(errcode.Parse, errcode.ReasonWorkerMalformed)
	}
	if !errcode.Known(code) {
		if codeShaped(code) {
			return "", errcode.New(errcode.NeedsSubtool, errcode.ReasonCodeUnfamiliar)
		}
		return "", errcode.New(errcode.Parse, errcode.ReasonCodeMalformed)
	}
	if !plainName(pattern) {
		return "", errcode.New(errcode.Parse, errcode.ReasonPatternMalformed)
	}
	// None of the three values can contain a newline, so joining them with
	// newlines cannot make two different failures hash the same bytes.
	sum := sha256.Sum256([]byte(version + "\n" + worker + "\n" + code + "\n" + pattern))
	return hex.EncodeToString(sum[:]), nil
}

// plainName reports whether s is lowercase letters, digits and single
// hyphens, starting with a letter and ending with a letter or digit.
func plainName(s string) bool {
	if len(s) == 0 || len(s) > MaxNameLen {
		return false
	}
	if s[0] < 'a' || s[0] > 'z' {
		return false
	}
	prevHyphen := false
	for i := 1; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
			prevHyphen = false
		case c == '-':
			if prevHyphen {
				return false
			}
			prevHyphen = true
		default:
			return false
		}
	}
	return !prevHyphen
}

// codeShaped reports whether s looks like an error code (E_ followed by
// capital letters, digits and underscores) without being a known one.
func codeShaped(s string) bool {
	if len(s) < 3 || len(s) > MaxNameLen || s[0] != 'E' || s[1] != '_' {
		return false
	}
	for i := 2; i < len(s); i++ {
		c := s[i]
		if !(c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_') {
			return false
		}
	}
	return true
}
