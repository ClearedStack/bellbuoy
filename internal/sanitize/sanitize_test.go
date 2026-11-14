package sanitize

import (
	"strings"
	"testing"
)

func TestPattern(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", ""},
		{"   ", ""},
		{"Queue is FULL", "queue is full"},
		{"dial tcp 10.0.0.7:5432: i/o timeout", "dial tcp # timeout"},
		{"lookup store-0.internal.example on 10.0.0.2:53: no such host", "lookup # on # no such host"},
		{"pod worker-7f9c4 restarted 3 times", "pod # restarted # times"},
		{"password=not-a-real-one rejected", "# rejected"},
		{"(timeout), again!", "timeout again"},
		{"... --- ...", "#"},
		{"réseau lent", "# lent"},
		{"see https://example.invalid/a/b?c=d now", "see # now"},
		{strings.Repeat("a", 25) + " ok", "# ok"},
		{strings.Repeat("a", 24) + " ok", strings.Repeat("a", 24) + " ok"},
	}
	for _, c := range cases {
		if got := Pattern(c.in); got != c.want {
			t.Errorf("Pattern(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestPatternIsBounded(t *testing.T) {
	got := Pattern(strings.Repeat("word ", 5000))
	if n := len(strings.Fields(got)); n != 32 {
		t.Fatalf("kept %d words, want 32", n)
	}
}

// Whatever goes in, only lower-case letters, single spaces and the
// placeholder come out.
func TestPatternAlphabet(t *testing.T) {
	in := "Node k3s-a1 at 192.0.2.44 (fe80::1) said: token=abc123 \x00\x1b[31m /var/spool/cs ünï"
	for _, r := range Pattern(in) {
		if !(r >= 'a' && r <= 'z') && r != ' ' && r != '#' {
			t.Fatalf("unexpected character %q in %q", r, Pattern(in))
		}
	}
}

func TestShapes(t *testing.T) {
	type check struct {
		name string
		fn   func(string) bool
		good []string
		bad  []string
	}
	checks := []check{
		{"IsWorker", IsWorker, []string{"bellbuoy", "muster", "a", "cs-10"}, []string{"", "Muster", "1st", "a_b", "a.b", strings.Repeat("a", 33)}},
		{"IsRepo", IsRepo, []string{"bellbuoy", "clearedstack.com", "0day"}, []string{"", "a..b", "A", "a/b", ".a", "-a"}},
		{"IsRef", IsRef, []string{"ed-0001", "A.b_c-9", "7"}, []string{"", "-a", "a b", "a/b", strings.Repeat("a", 65)}},
		{"IsItem", IsItem, []string{"PA-0042", "PA-9999", "PA-0000", "PA-10000", "PA-123456789"}, []string{"PA-42", "pa-0042", "PA-00420", "PA-00a2", "", "PA-", "PA-1234567890", "PA-01234", "PA-0042 "}},
		{"IsLease", IsLease, []string{"PA-0042.1", "a", "x_y:z-9"}, []string{"", "a b", "a/b", "a\nb", strings.Repeat("a", 129)}},
		{"IsTier", IsTier, []string{"P0", "P1"}, []string{"P", "p0", "P10", "Q0"}},
		{"IsCodeShaped", IsCodeShaped, []string{"E_TIMEOUT", "E_OUT_OF_SPACE"}, []string{"E_", "TIMEOUT", "E_timeout", "E_1", "E__A"}},
		{"IsKind", IsKind, []string{"error", "dead_letter", "item-lifecycle"}, []string{"", "Error", "_x", "a b", "a1"}},
		{"IsPath", IsPath, []string{"a", "platform/registry.json", "a/b-c/d_e.f"}, []string{"", "/a", "a/", "a//b", "../a", "a/../b", "a/./b", "a\\b", "a b", "a/b?c"}},
	}
	for _, c := range checks {
		for _, s := range c.good {
			if !c.fn(s) {
				t.Errorf("%s(%q) = false, want true", c.name, s)
			}
		}
		for _, s := range c.bad {
			if c.fn(s) {
				t.Errorf("%s(%q) = true, want false", c.name, s)
			}
		}
	}
}

func TestIsText(t *testing.T) {
	if !IsText("line one\nline two\ttabbed\r\n", 100) {
		t.Error("plain text refused")
	}
	for _, s := range []string{"nul\x00", "escape\x1b[0m", "bad\xff", "del\x7f"} {
		if IsText(s, 100) {
			t.Errorf("IsText(%q) = true", s)
		}
	}
	if IsText("too long", 3) {
		t.Error("length limit ignored")
	}
}

func TestIsLetters(t *testing.T) {
	if !IsLetters("Approved", 16) || IsLetters("", 16) || IsLetters("a1", 16) || IsLetters("abc", 2) {
		t.Error("IsLetters is wrong")
	}
}
