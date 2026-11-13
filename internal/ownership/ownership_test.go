package ownership

import (
	"testing"

	"github.com/ClearedStack/bellbuoy/internal/codes"
)

func load(t *testing.T) *Map {
	t.Helper()
	m, err := Load("../../testdata/ownership_map.json")
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestWorkers(t *testing.T) {
	m := load(t)
	if r, ok := m.RepoForWorker("muster"); !ok || r != "muster" {
		t.Fatalf("muster -> %q %v", r, ok)
	}
	if _, ok := m.RepoForWorker("nobody"); ok {
		t.Fatal("an unlisted worker has no owner")
	}
}

func TestPaths(t *testing.T) {
	m := load(t)
	cases := []struct {
		target, repo string
		ok           bool
	}{
		{"platform/registry.json", "clearedstack.com", true},
		{"platform/workers/bellbuoy/notes.json", "bellbuoy", true}, // longest prefix wins
		{"platform/workers/muster/notes.json", "clearedstack.com", true},
		{"deploy/values.json", "shipkeeper", true}, // exact file
		{"deploy/values.json.bak", "", false},      // not a prefix match
		{"deploy/other.json", "", false},
		{"platform", "", false},
		{"elsewhere/file.json", "", false},
	}
	for _, c := range cases {
		repo, ok := m.RepoForPath(c.target)
		if repo != c.repo || ok != c.ok {
			t.Errorf("RepoForPath(%q) = %q %v, want %q %v", c.target, repo, ok, c.repo, c.ok)
		}
	}
}

func TestMirrors(t *testing.T) {
	m := load(t)
	if !m.Mirrored("muster") || m.Mirrored("shipkeeper") || m.Mirrored("unlisted") {
		t.Fatal("mirror flags are wrong")
	}
	if !m.Known("shipkeeper") || m.Known("unlisted") {
		t.Fatal("Known is wrong")
	}
}

func TestParseRefusesBadMaps(t *testing.T) {
	bad := []string{
		``,
		`[]`,
		`{"repos":{"a":{"mirror":true}}} extra`,
		`{"repos":{"a":{"mirror":true}},"surprise":1}`,
		`{"repos":{"A":{"mirror":true}}}`,
		`{"repos":{"a":{"mirror":"yes"}}}`,
		`{"repos":{"a":{}},"workers":{"Bad Name":"a"}}`,
		`{"repos":{"a":{}},"workers":{"w":"missing"}}`,
		`{"repos":{"a":{}},"paths":[{"prefix":"../x/","repo":"a"}]}`,
		`{"repos":{"a":{}},"paths":[{"prefix":"/x/","repo":"a"}]}`,
		`{"repos":{"a":{}},"paths":[{"prefix":"x/","repo":"missing"}]}`,
		`{"repos":{"a":{}},"paths":[{"prefix":"x/","repo":"a"},{"prefix":"x/","repo":"a"}]}`,
	}
	for _, s := range bad {
		if _, err := Parse([]byte(s)); err == nil || codes.From(err).Code != codes.Parse {
			t.Errorf("Parse(%s) = %v, want E_PARSE", s, err)
		}
	}
}

func TestLoadMissingFileNamesNoPath(t *testing.T) {
	_, err := Load("/nonexistent/place/ownership_map.json")
	if err == nil || codes.From(err).Code != codes.Parse {
		t.Fatalf("err = %v", err)
	}
	if got := err.Error(); got != "E_PARSE: ownership map cannot be read" {
		t.Fatalf("error text must not name the path: %q", got)
	}
}
