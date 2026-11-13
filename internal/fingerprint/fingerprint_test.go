package fingerprint

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/ClearedStack/bellbuoy/internal/codes"
	"github.com/ClearedStack/bellbuoy/internal/sanitize"
)

type vector struct {
	Worker      string `json:"worker"`
	Code        string `json:"code"`
	Pattern     string `json:"pattern"`
	Sanitized   string `json:"sanitized"`
	Fingerprint string `json:"fingerprint"`
}

// The expected values in testdata/vectors.json were worked out by a separate
// implementation of the recipe, not by this package.
func TestVectors(t *testing.T) {
	raw, err := os.ReadFile("testdata/vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var vectors []vector
	if err := json.Unmarshal(raw, &vectors); err != nil {
		t.Fatal(err)
	}
	if len(vectors) == 0 {
		t.Fatal("no vectors")
	}
	for _, v := range vectors {
		if got := sanitize.Pattern(v.Pattern); got != v.Sanitized {
			t.Errorf("Pattern(%q) = %q, want %q", v.Pattern, got, v.Sanitized)
		}
		if got := Of(v.Worker, codes.Code(v.Code), v.Pattern); got != v.Fingerprint {
			t.Errorf("Of(%q, %q, %q) = %s, want %s", v.Worker, v.Code, v.Pattern, got, v.Fingerprint)
		}
	}
}

func TestSameShapeSameFingerprint(t *testing.T) {
	a := Of("muster", codes.Timeout, "dial tcp 10.0.0.7:5432: i/o timeout after 31s on node-3")
	b := Of("muster", codes.Timeout, "dial tcp 10.0.0.9:5432: i/o timeout after 12s on node-8")
	if a != b {
		t.Fatalf("failures that differ only in numbers and names must match: %s != %s", a, b)
	}
}

func TestEachPartCounts(t *testing.T) {
	base := Of("muster", codes.Timeout, "queue is full")
	for name, other := range map[string]string{
		"worker":  Of("keelworks", codes.Timeout, "queue is full"),
		"code":    Of("muster", codes.Parse, "queue is full"),
		"pattern": Of("muster", codes.Timeout, "queue is empty"),
	} {
		if other == base {
			t.Errorf("changing the %s did not change the fingerprint", name)
		}
	}
}

// Parts must not run together: moving a character from one part to the next
// has to give a different fingerprint.
func TestPartsDoNotRunTogether(t *testing.T) {
	if Of("ab", codes.Code("c"), "") == Of("a", codes.Code("bc"), "") {
		t.Fatal("parts ran together")
	}
}

func TestShort(t *testing.T) {
	fp := Of("muster", codes.Timeout, "queue is full")
	if len(fp) != 64 || len(Short(fp)) != 12 || Short(fp) != fp[:12] {
		t.Fatalf("unexpected lengths: %q %q", fp, Short(fp))
	}
	if Short("abc") != "abc" {
		t.Fatal("Short must not panic on short input")
	}
}
