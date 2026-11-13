package codes

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestLookup(t *testing.T) {
	for _, s := range []string{"E_TIMEOUT", "E_PARSE", "E_NEEDS_SUBTOOL"} {
		if c, ok := Lookup(s); !ok || string(c) != s {
			t.Errorf("Lookup(%q) failed", s)
		}
	}
	for _, s := range []string{"", "e_timeout", "E_OTHER", "E_TIMEOUT "} {
		if _, ok := Lookup(s); ok {
			t.Errorf("Lookup(%q) succeeded", s)
		}
	}
}

func TestFrom(t *testing.T) {
	if From(nil) != nil {
		t.Fatal("nil must stay nil")
	}
	own := Unfamiliar("no rule")
	if got := From(fmt.Errorf("wrapped: %w", own)); got != own {
		t.Fatal("a public error must be found through wrapping")
	}
	if got := From(context.DeadlineExceeded); got.Code != Timeout {
		t.Fatalf("deadline: %v", got)
	}
	if got := From(context.Canceled); got.Code != Timeout {
		t.Fatalf("canceled: %v", got)
	}
}

// The text of a foreign error may name a machine. None of it may survive.
func TestFromDropsForeignText(t *testing.T) {
	foreign := errors.New("dial tcp 10.0.0.7:5432: connect: connection refused (host store-0.internal.example, password hunter2)")
	got := From(foreign)
	if got.Code != Parse {
		t.Fatalf("code = %s", got.Code)
	}
	for _, leak := range []string{"10.0.0.7", "store-0", "hunter2", "5432", "dial"} {
		if strings.Contains(got.Error(), leak) {
			t.Fatalf("error text leaks %q: %s", leak, got.Error())
		}
	}
}

func TestErrorText(t *testing.T) {
	if got := TimedOut("too slow").Error(); got != "E_TIMEOUT: too slow" {
		t.Fatal(got)
	}
	if got := Broken("bad").Error(); got != "E_PARSE: bad" {
		t.Fatal(got)
	}
}
