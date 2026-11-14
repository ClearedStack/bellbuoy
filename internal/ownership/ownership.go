// Package ownership reads ownership_map.json, the file that says which
// repository owns the fix for a worker's failures or for an edited element
// or file.
//
// It accepts both Muster's contract v1 form:
//
//	{
//	  "elements": {
//	    "catalog.sample-calculator.description": {
//	      "repository": "clearedstack.com",
//	      "file": "src/data/sample-catalog.json",
//	      "kind": "DATA"
//	    }
//	  }
//	}
//
// and the extended cluster form with explicit "repos", "workers" and "paths":
//
//	{
//	  "repos":    {"muster": {"mirror": true}},
//	  "workers":  {"muster": "muster"},
//	  "paths":    [{"prefix": "platform/", "repo": "clearedstack.com"}],
//	  "elements": {...}
//	}
//
// When a map supplies only "elements", the repositories and worker-to-repo
// mappings default to the ClearedStack naming registry.
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

// DefaultPath is where the ConfigMap mounts ownership_map.json in the cluster.
const DefaultPath = "/etc/bellbuoy/ownership_map.json"

// DefaultJSON is the cluster ownership map shipped in deploy/bellbuoy.yaml.
// A test in cmd/bellbuoy keeps the two identical.
const DefaultJSON = `{
  "repos": {
    "clearedstack.com": { "mirror": true },
    "muster":           { "mirror": true },
    "logbook":          { "mirror": true },
    "plainforge":       { "mirror": true },
    "lazarette":        { "mirror": true },
    "keelworks":        { "mirror": true },
    "shipkeeper":       { "mirror": true },
    "trawl":            { "mirror": true },
    "sextant":          { "mirror": true },
    "assay":            { "mirror": true },
    "countersign":      { "mirror": true },
    "waybill":          { "mirror": true },
    "moldloft":         { "mirror": true },
    "shakedown":        { "mirror": true },
    "chartroom":        { "mirror": true },
    "tidegauge":        { "mirror": true },
    "bellbuoy":         { "mirror": true }
  },
  "workers": {
    "conn":        "clearedstack.com",
    "muster":      "muster",
    "logbook":     "logbook",
    "plainforge":  "plainforge",
    "lazarette":   "lazarette",
    "keelworks":   "keelworks",
    "shipkeeper":  "shipkeeper",
    "trawl":       "trawl",
    "sextant":     "sextant",
    "assay":       "assay",
    "countersign": "countersign",
    "waybill":     "waybill",
    "moldloft":    "moldloft",
    "shakedown":   "shakedown",
    "chartroom":   "chartroom",
    "tidegauge":   "tidegauge",
    "bellbuoy":    "bellbuoy"
  },
  "paths": [
    { "prefix": "src/",    "repo": "clearedstack.com" },
    { "prefix": "deploy/", "repo": "clearedstack.com" }
  ],
  "elements": {
    "command.hero.mission":        { "repository": "clearedstack.com", "file": "src/data/site.ts",          "kind": "DATA" },
    "portal.home.hero":            { "repository": "clearedstack.com", "file": "src/app/guides/page.tsx",   "kind": "CONFIG" },
    "command.attention.p0":        { "repository": "shipkeeper",       "file": "app.yaml",                  "kind": "ISSUE" },
    "command.attention.p1":        { "repository": "clearedstack.com", "file": "deploy/muster/app.yaml",    "kind": "ISSUE" },
    "command.attention.p2":        { "repository": "countersign",      "file": "app.yaml",                  "kind": "ISSUE" },
    "decide.sandbox.test":         { "repository": "clearedstack.com", "file": "src/app/guides/page.tsx",   "kind": "ISSUE" },
    "app.conn.manifest":           { "repository": "clearedstack.com", "file": "app.yaml",                  "kind": "CONFIG" },
    "app.muster.manifest":         { "repository": "clearedstack.com", "file": "deploy/muster/app.yaml",    "kind": "CONFIG" },
    "app.logbook.manifest":        { "repository": "clearedstack.com", "file": "deploy/logbook/app.yaml",   "kind": "CONFIG" },
    "app.plainforge.manifest":     { "repository": "plainforge",       "file": "app.yaml",                  "kind": "CONFIG" },
    "app.lazarette.manifest":      { "repository": "lazarette",        "file": "app.yaml",                  "kind": "CONFIG" },
    "app.keelworks.manifest":      { "repository": "keelworks",        "file": "app.yaml",                  "kind": "CONFIG" },
    "app.shipkeeper.manifest":     { "repository": "shipkeeper",       "file": "app.yaml",                  "kind": "CONFIG" },
    "worker.trawl.manifest":       { "repository": "trawl",            "file": "app.yaml",                  "kind": "CONFIG" },
    "worker.sextant.manifest":     { "repository": "sextant",          "file": "app.yaml",                  "kind": "CONFIG" },
    "worker.assay.manifest":       { "repository": "assay",            "file": "app.yaml",                  "kind": "CONFIG" },
    "worker.countersign.manifest": { "repository": "countersign",      "file": "app.yaml",                  "kind": "CONFIG" },
    "worker.waybill.manifest":     { "repository": "waybill",          "file": "app.yaml",                  "kind": "CONFIG" },
    "worker.moldloft.manifest":    { "repository": "moldloft",         "file": "app.yaml",                  "kind": "CONFIG" },
    "worker.shakedown.manifest":   { "repository": "shakedown",        "file": "app.yaml",                  "kind": "CONFIG" },
    "worker.chartroom.manifest":   { "repository": "chartroom",        "file": "app.yaml",                  "kind": "CONFIG" },
    "worker.tidegauge.manifest":   { "repository": "tidegauge",        "file": "app.yaml",                  "kind": "CONFIG" },
    "worker.bellbuoy.manifest":    { "repository": "bellbuoy",         "file": "app.yaml",                  "kind": "CONFIG" },
    "worker.trawl.role":           { "repository": "trawl",            "file": "app.yaml",                  "kind": "CONFIG" },
    "worker.sextant.role":         { "repository": "sextant",          "file": "app.yaml",                  "kind": "CONFIG" },
    "worker.assay.role":           { "repository": "assay",            "file": "app.yaml",                  "kind": "CONFIG" },
    "worker.countersign.role":     { "repository": "countersign",      "file": "app.yaml",                  "kind": "CONFIG" },
    "worker.waybill.role":         { "repository": "waybill",          "file": "app.yaml",                  "kind": "CONFIG" },
    "worker.moldloft.role":        { "repository": "moldloft",         "file": "app.yaml",                  "kind": "CONFIG" },
    "worker.shakedown.role":       { "repository": "shakedown",        "file": "app.yaml",                  "kind": "CONFIG" },
    "worker.chartroom.role":       { "repository": "chartroom",        "file": "app.yaml",                  "kind": "CONFIG" },
    "worker.tidegauge.role":       { "repository": "tidegauge",        "file": "app.yaml",                  "kind": "CONFIG" },
    "worker.bellbuoy.role":        { "repository": "bellbuoy",         "file": "app.yaml",                  "kind": "CONFIG" },
    "pipeline.trawl":              { "repository": "trawl",            "file": "app.yaml",                  "kind": "ISSUE" },
    "pipeline.sextant":            { "repository": "sextant",          "file": "app.yaml",                  "kind": "ISSUE" },
    "pipeline.assay":              { "repository": "assay",            "file": "app.yaml",                  "kind": "ISSUE" },
    "pipeline.countersign":        { "repository": "countersign",      "file": "app.yaml",                  "kind": "ISSUE" },
    "pipeline.waybill":            { "repository": "waybill",          "file": "app.yaml",                  "kind": "ISSUE" },
    "pipeline.moldloft":           { "repository": "moldloft",         "file": "app.yaml",                  "kind": "ISSUE" },
    "pipeline.shakedown":          { "repository": "shakedown",        "file": "app.yaml",                  "kind": "ISSUE" },
    "pipeline.chartroom":          { "repository": "chartroom",        "file": "app.yaml",                  "kind": "ISSUE" },
    "pipeline.tidegauge":          { "repository": "tidegauge",        "file": "app.yaml",                  "kind": "ISSUE" },
    "pipeline.bellbuoy":           { "repository": "bellbuoy",         "file": "app.yaml",                  "kind": "ISSUE" }
  }
}`

// Default returns the built-in cluster ownership map.
func Default() (*Map, error) {
	return Parse([]byte(DefaultJSON))
}

// registryRepos lists the seventeen repositories in the naming registry.
var registryRepos = []string{
	"assay",
	"bellbuoy",
	"chartroom",
	"clearedstack.com",
	"countersign",
	"keelworks",
	"lazarette",
	"logbook",
	"moldloft",
	"muster",
	"plainforge",
	"sextant",
	"shakedown",
	"shipkeeper",
	"tidegauge",
	"trawl",
	"waybill",
}

// registryComponents lists the sixteen components in the naming registry
// whose failures map to their own repository.
var registryComponents = []string{
	"assay",
	"bellbuoy",
	"chartroom",
	"countersign",
	"keelworks",
	"lazarette",
	"logbook",
	"moldloft",
	"muster",
	"plainforge",
	"sextant",
	"shakedown",
	"shipkeeper",
	"tidegauge",
	"trawl",
	"waybill",
}

// Element describes one editable element in Muster's ownership_map.json.
type Element struct {
	Repository string `json:"repository"`
	File       string `json:"file"`
	Kind       string `json:"kind"`
}

type file struct {
	Repos    map[string]repo    `json:"repos"`
	Workers  map[string]string  `json:"workers"`
	Paths    []pathRule         `json:"paths"`
	Elements map[string]Element `json:"elements"`
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
	repos    map[string]repo
	workers  map[string]string
	paths    []pathRule // longest prefix first
	elements map[string]Element
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
	if f.Repos == nil && f.Workers == nil && f.Paths == nil && f.Elements != nil {
		f.Repos = make(map[string]repo, len(registryRepos))
		for _, name := range registryRepos {
			f.Repos[name] = repo{Mirror: true}
		}
		f.Workers = make(map[string]string, len(registryComponents))
		for _, name := range registryComponents {
			f.Workers[name] = name
		}
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
	for target, elem := range f.Elements {
		if !sanitize.IsTarget(target) {
			return nil, codes.Broken("ownership map names a malformed element target")
		}
		if _, ok := f.Repos[elem.Repository]; !ok {
			return nil, codes.Broken("ownership map gives an element to a repository it does not list")
		}
		if !sanitize.IsPath(elem.File) {
			return nil, codes.Broken("ownership map has a malformed element file")
		}
		switch elem.Kind {
		case "DATA", "CONFIG", "ISSUE":
		default:
			return nil, codes.Broken("ownership map has an unknown element edit kind")
		}
	}
	m := &Map{
		repos:    f.Repos,
		workers:  f.Workers,
		paths:    append([]pathRule(nil), f.Paths...),
		elements: f.Elements,
	}
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

// ElementForTarget returns the editable element entry for target, if any.
func (m *Map) ElementForTarget(target string) (Element, bool) {
	elem, ok := m.elements[target]
	return elem, ok
}

// Known reports whether the map lists the repository.
func (m *Map) Known(name string) bool {
	_, ok := m.repos[name]
	return ok
}

// Mirrored reports whether the repository has a public mirror.
func (m *Map) Mirrored(name string) bool { return m.repos[name].Mirror }
