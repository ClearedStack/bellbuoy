// Package ownership reads ownership_map.json, the file that says which
// repository owns the fix for a worker's failures or for an edited file.
//
// The file has three parts:
//
//	{
//	  "repos":   {"muster": {"mirror": true}},
//	  "workers": {"muster": "muster"},
//	  "paths":   [{"prefix": "platform/", "repo": "clearedstack.com"}]
//	}
//
// "repos" lists every repository Bellbuoy may write to and whether it has a
// public mirror. "workers" maps a worker's name to the repository that owns
// it. "paths" maps the target of a Portal edit to a repository: a prefix
// ending in "/" covers everything under it, any other prefix covers that one
// file, and the longest matching prefix wins.
package ownership

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/ClearedStack/bellbuoy/internal/codes"
	"github.com/ClearedStack/bellbuoy/internal/sanitize"
)

// maxFile is the largest ownership map accepted, in bytes.
const maxFile = 1 << 20

type file struct {
	Repos   map[string]repo   `json:"repos"`
	Workers map[string]string `json:"workers"`
	Paths   []pathRule        `json:"paths"`
}

type repo struct {
	Mirror bool `json:"mirror"`
}

type pathRule struct {
	Prefix string `json:"prefix"`
	Repo   string `json:"repo"`
}

// Map is a checked ownership map.
type Map struct {
	repos   map[string]repo
	workers map[string]string
	paths   []pathRule // longest prefix first
}

// Load reads and checks the file at path.
func Load(path string) (*Map, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, codes.Broken("ownership map cannot be read")
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxFile+1))
	if err != nil || len(data) > maxFile {
		return nil, codes.Broken("ownership map cannot be read")
	}
	return Parse(data)
}

// Parse checks an ownership map. Anything out of place is E_PARSE: a map that
// is partly understood would send work to the wrong repository.
func Parse(data []byte) (*Map, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var f file
	if err := dec.Decode(&f); err != nil {
		return nil, codes.Broken("ownership map is not in the expected form")
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, codes.Broken("ownership map has text after its end")
	}
	for name := range f.Repos {
		if !sanitize.IsRepo(name) {
			return nil, codes.Broken("ownership map names a malformed repository")
		}
	}
	for worker, name := range f.Workers {
		if !sanitize.IsWorker(worker) {
			return nil, codes.Broken("ownership map names a malformed worker")
		}
		if _, ok := f.Repos[name]; !ok {
			return nil, codes.Broken("ownership map gives a worker to a repository it does not list")
		}
	}
	seen := map[string]bool{}
	for _, p := range f.Paths {
		if !sanitize.IsPath(strings.TrimSuffix(p.Prefix, "/")) {
			return nil, codes.Broken("ownership map has a malformed path prefix")
		}
		if seen[p.Prefix] {
			return nil, codes.Broken("ownership map lists a path prefix twice")
		}
		seen[p.Prefix] = true
		if _, ok := f.Repos[p.Repo]; !ok {
			return nil, codes.Broken("ownership map gives a path to a repository it does not list")
		}
	}
	m := &Map{repos: f.Repos, workers: f.Workers, paths: append([]pathRule(nil), f.Paths...)}
	sort.SliceStable(m.paths, func(i, j int) bool {
		if len(m.paths[i].Prefix) != len(m.paths[j].Prefix) {
			return len(m.paths[i].Prefix) > len(m.paths[j].Prefix)
		}
		return m.paths[i].Prefix < m.paths[j].Prefix
	})
	return m, nil
}

// RepoForWorker returns the repository that owns a worker.
func (m *Map) RepoForWorker(worker string) (string, bool) {
	name, ok := m.workers[worker]
	return name, ok
}

// RepoForPath returns the repository that owns the file an edit targets.
func (m *Map) RepoForPath(target string) (string, bool) {
	for _, p := range m.paths {
		if target == p.Prefix || (strings.HasSuffix(p.Prefix, "/") && strings.HasPrefix(target, p.Prefix)) {
			return p.Repo, true
		}
	}
	return "", false
}

// Known reports whether the map lists the repository.
func (m *Map) Known(name string) bool {
	_, ok := m.repos[name]
	return ok
}

// Mirrored reports whether the repository has a public mirror.
func (m *Map) Mirrored(name string) bool { return m.repos[name].Mirror }
