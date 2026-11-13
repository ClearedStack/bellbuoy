// Package sanitize reduces text from outside to forms that are safe to
// repeat, and checks the shape of every identifier Bellbuoy accepts.
package sanitize

import (
	"strings"
	"unicode/utf8"
)

const (
	// Placeholder stands for any run of words that are not plain letters.
	Placeholder = "#"

	maxPatternBytes = 4096
	maxPatternWords = 32
	maxWordLen      = 24

	trimSet = ".,;:!?()[]{}<>\"'`"
)

// Pattern reduces a failure message to its repeatable shape.
//
// The text is cut to 4096 bytes and split on white space. Punctuation is
// trimmed from both ends of each word. A word made only of the letters a to z
// (in either case, at most 24 of them) is kept in lower case. Every other
// word, which covers numbers, addresses, host names, paths, identifiers and
// key=value pairs, becomes "#", and neighbouring "#" collapse into one. At
// most 32 words are kept, joined by single spaces.
func Pattern(s string) string {
	if len(s) > maxPatternBytes {
		s = s[:maxPatternBytes]
	}
	out := make([]string, 0, 8)
	for _, tok := range strings.Fields(s) {
		tok = strings.Trim(tok, trimSet)
		if tok == "" {
			continue
		}
		w := word(tok)
		if w == Placeholder && len(out) > 0 && out[len(out)-1] == Placeholder {
			continue
		}
		out = append(out, w)
		if len(out) == maxPatternWords {
			break
		}
	}
	return strings.Join(out, " ")
}

func word(tok string) string {
	if len(tok) > maxWordLen {
		return Placeholder
	}
	b := []byte(tok)
	for i, c := range b {
		switch {
		case c >= 'a' && c <= 'z':
		case c >= 'A' && c <= 'Z':
			b[i] = c + ('a' - 'A')
		default:
			return Placeholder
		}
	}
	return string(b)
}

func lower(c byte) bool { return c >= 'a' && c <= 'z' }
func upper(c byte) bool { return c >= 'A' && c <= 'Z' }
func digit(c byte) bool { return c >= '0' && c <= '9' }

// IsWorker reports whether s has the shape of a registry name: a lower-case
// letter, then up to 31 lower-case letters, digits or hyphens.
func IsWorker(s string) bool {
	if len(s) < 1 || len(s) > 32 || !lower(s[0]) {
		return false
	}
	for i := 1; i < len(s); i++ {
		if c := s[i]; !lower(c) && !digit(c) && c != '-' {
			return false
		}
	}
	return true
}

// IsRepo reports whether s has the shape of a repository name.
func IsRepo(s string) bool {
	if len(s) < 1 || len(s) > 64 || (!lower(s[0]) && !digit(s[0])) {
		return false
	}
	for i := 1; i < len(s); i++ {
		if c := s[i]; !lower(c) && !digit(c) && c != '-' && c != '.' {
			return false
		}
	}
	return !strings.Contains(s, "..")
}

// IsRef reports whether s is an opaque reference: a letter or digit, then up
// to 63 letters, digits, dots, underscores or hyphens.
func IsRef(s string) bool {
	if len(s) < 1 || len(s) > 64 || (!lower(s[0]) && !upper(s[0]) && !digit(s[0])) {
		return false
	}
	for i := 1; i < len(s); i++ {
		if c := s[i]; !lower(c) && !upper(c) && !digit(c) && c != '.' && c != '_' && c != '-' {
			return false
		}
	}
	return true
}

// IsItem reports whether s is a pipeline item reference, PA- and four digits.
func IsItem(s string) bool {
	if len(s) != 7 || s[:3] != "PA-" {
		return false
	}
	for i := 3; i < 7; i++ {
		if !digit(s[i]) {
			return false
		}
	}
	return true
}

// IsTier reports whether s is an attention tier, P and one digit.
func IsTier(s string) bool { return len(s) == 2 && s[0] == 'P' && digit(s[1]) }

// IsCodeShaped reports whether s looks like an error code (E_ and capitals),
// whether or not it is one of the three public ones.
func IsCodeShaped(s string) bool {
	if len(s) < 3 || len(s) > 32 || s[:2] != "E_" || !upper(s[2]) {
		return false
	}
	for i := 3; i < len(s); i++ {
		if c := s[i]; !upper(c) && c != '_' {
			return false
		}
	}
	return true
}

// IsKind reports whether s is shaped like a job kind or stream name: lower
// case letters, underscores and hyphens, starting with a letter.
func IsKind(s string) bool {
	if len(s) < 1 || len(s) > 32 || !lower(s[0]) {
		return false
	}
	for i := 1; i < len(s); i++ {
		if c := s[i]; !lower(c) && c != '_' && c != '-' {
			return false
		}
	}
	return true
}

// IsLetters reports whether s is 1 to max letters and nothing else.
func IsLetters(s string, max int) bool {
	if len(s) < 1 || len(s) > max {
		return false
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; !lower(c) && !upper(c) {
			return false
		}
	}
	return true
}

// IsPath reports whether s is a plain relative file path: segments of
// letters, digits, dots, underscores and hyphens joined by single slashes,
// with no "." or ".." segment.
func IsPath(s string) bool {
	if len(s) < 1 || len(s) > 256 {
		return false
	}
	for _, seg := range strings.Split(s, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return false
		}
		for i := 0; i < len(seg); i++ {
			if c := seg[i]; !lower(c) && !upper(c) && !digit(c) && c != '.' && c != '_' && c != '-' {
				return false
			}
		}
	}
	return true
}

// IsText reports whether s is valid UTF-8 of at most max bytes with no
// control characters other than tab, line feed and carriage return.
func IsText(s string, max int) bool {
	if len(s) > max || !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if (r < 0x20 && r != '\t' && r != '\n' && r != '\r') || r == 0x7f {
			return false
		}
	}
	return true
}
