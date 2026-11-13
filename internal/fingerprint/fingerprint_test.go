package fingerprint

import (
	"encoding/json"
	"flag"
	"os"
	"strings"
	"testing"

	"github.com/clearedstack/bellbuoy/internal/errcode"
)

var update = flag.Bool("update", false, "rewrite the fingerprints saved in testdata/vectors.json")

type vector struct {
	Worker      string `json:"worker"`
	Code        string `json:"code"`
	Pattern     string `json:"pattern"`
	Fingerprint string `json:"fingerprint,omitempty"`
	Error       string `json:"error,omitempty"`
}

// TestVectors checks every saved example: the exact fingerprint for good
// values, the exact public error for bad ones.
func TestVectors(t *testing.T) {
	const path = "testdata/vectors.json"
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var vs []vector
	if err := json.Unmarshal(raw, &vs); err != nil {
		t.Fatal(err)
	}
	if len(vs) == 0 {
		t.Fatal("no vectors")
	}
	for i := range vs {
		v := &vs[i]
		fp, perr := Of(v.Worker, v.Code, v.Pattern)
		gotErr := ""
		if perr != nil {
			gotErr = perr.Error()
		}
		if *update {
			v.Fingerprint, v.Error = fp, gotErr
			continue
		}
		if fp != v.Fingerprint || gotErr != v.Error {
			t.Errorf("vector %d (%q, %q, %q): got (%q, %q), want (%q, %q)",
				i, v.Worker, v.Code, v.Pattern, fp, gotErr, v.Fingerprint, v.Error)
		}
	}
	if *update {
		out, err := json.MarshalIndent(vs, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, append(out, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// One fingerprint is written out here as well as in the vectors file, so
// that regenerating the file cannot hide a change to the function.
func TestPinned(t *testing.T) {
	const want = "3d0bab2ff68c8f670dc17fa80d7e7dbafde711599967c295949787ffa8d06db5"
	got, perr := Of("keelworks", "E_TIMEOUT", "build-step-slow")
	if perr != nil || got != want {
		t.Fatalf("got %q, %v; want %q", got, perr, want)
	}
}

func TestShapeAndRepeatability(t *testing.T) {
	a, _ := Of("keelworks", "E_TIMEOUT", "build-step-slow")
	b, _ := Of("keelworks", "E_TIMEOUT", "build-step-slow")
	if a != b {
		t.Fatal("same input gave different fingerprints")
	}
	if len(a) != 64 || strings.Trim(a, "0123456789abcdef") != "" {
		t.Fatalf("fingerprint %q is not 64 lowercase hex characters", a)
	}
}

// Each of the three values must change the fingerprint, and moving text
// from one value to its neighbour must too.
func TestEveryPartCounts(t *testing.T) {
	inputs := [][3]string{
		{"keelworks", "E_TIMEOUT", "build-step-slow"},
		{"shipkeeper", "E_TIMEOUT", "build-step-slow"},
		{"keelworks", "E_PARSE", "build-step-slow"},
		{"keelworks", "E_NEEDS_SUBTOOL", "build-step-slow"},
		{"keelworks", "E_TIMEOUT", "build-step-slower"},
		{"keelworks-build", "E_TIMEOUT", "step-slow"},
		{"keelworks", "E_TIMEOUT", "build-step-slow-keelworks"},
	}
	seen := map[string][3]string{}
	for _, in := range inputs {
		fp, perr := Of(in[0], in[1], in[2])
		if perr != nil {
			t.Fatalf("%v: %v", in, perr)
		}
		if other, dup := seen[fp]; dup {
			t.Fatalf("%v and %v share a fingerprint", in, other)
		}
		seen[fp] = in
	}
}

func TestLengthLimit(t *testing.T) {
	ok := "a" + strings.Repeat("b", MaxNameLen-1)
	if _, perr := Of(ok, "E_PARSE", ok); perr != nil {
		t.Fatalf("names of %d characters refused: %v", MaxNameLen, perr)
	}
	long := ok + "b"
	if _, perr := Of(long, "E_PARSE", "x"); perr == nil || perr.Code != errcode.Parse {
		t.Fatalf("long worker: %v", perr)
	}
	if _, perr := Of("x", "E_PARSE", long); perr == nil || perr.Code != errcode.Parse {
		t.Fatalf("long pattern: %v", perr)
	}
}
