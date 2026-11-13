package tools

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// readRegistry reads the small part of YAML that registry.yaml uses: one
// top-level "tools:" key holding a list of flat maps.
func readRegistry(t *testing.T) []map[string]string {
	t.Helper()
	f, err := os.Open("registry.yaml")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var entries []map[string]string
	inList := false
	sc := bufio.NewScanner(f)
	for n := 1; sc.Scan(); n++ {
		line := sc.Text()
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if line == "tools:" {
			inList = true
			continue
		}
		if !inList {
			t.Fatalf("registry.yaml line %d: expected \"tools:\"", n)
		}
		var body string
		switch {
		case strings.HasPrefix(line, "  - "):
			entries = append(entries, map[string]string{})
			body = line[4:]
		case strings.HasPrefix(line, "    ") && len(entries) > 0:
			body = line[4:]
		default:
			t.Fatalf("registry.yaml line %d: unexpected layout", n)
		}
		key, value, ok := strings.Cut(body, ": ")
		if !ok || key == "" || value == "" {
			t.Fatalf("registry.yaml line %d: expected \"key: value\"", n)
		}
		entry := entries[len(entries)-1]
		if _, dup := entry[key]; dup {
			t.Fatalf("registry.yaml line %d: %q given twice", n, key)
		}
		entry[key] = value
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return entries
}

// The registry file, the compiled-in list and component.json must name the
// same sub-tools in the same order, and every one must have its three parts.
func TestRegistryIsInStep(t *testing.T) {
	entries := readRegistry(t)

	var listed []string
	for _, e := range entries {
		listed = append(listed, e["name"])
	}

	var built []string
	for _, tool := range All(Deps{}) {
		built = append(built, tool.Name())
	}
	if !reflect.DeepEqual(listed, built) {
		t.Fatalf("registry.yaml lists %v, the program builds %v", listed, built)
	}

	raw, err := os.ReadFile("../component.json")
	if err != nil {
		t.Fatal(err)
	}
	var component struct {
		SubTools []string `json:"subTools"`
	}
	if err := json.Unmarshal(raw, &component); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(listed, component.SubTools) {
		t.Fatalf("registry.yaml lists %v, component.json lists %v", listed, component.SubTools)
	}

	for _, e := range entries {
		name := e["name"]
		for _, key := range []string{"name", "folder", "rule", "logic", "fixtures", "target"} {
			if e[key] == "" {
				t.Fatalf("%s: registry.yaml has no %q", name, key)
			}
		}
		if e["folder"] != "tools/"+name {
			t.Errorf("%s: folder is %q, want tools/%s", name, e["folder"], name)
		}
		folder := filepath.Join("..", filepath.FromSlash(e["folder"]))
		for _, part := range []string{e["rule"], e["logic"]} {
			if info, err := os.Stat(filepath.Join(folder, part)); err != nil || info.IsDir() {
				t.Errorf("%s: %s is missing", name, part)
			}
		}
		fixtures, _ := filepath.Glob(filepath.Join(folder, e["fixtures"], "*.json"))
		if len(fixtures) == 0 {
			t.Errorf("%s: no fixtures", name)
		}
	}

	// No sub-tool folder may exist without being listed.
	dirs, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, d := range dirs {
		if d.IsDir() {
			count++
		}
	}
	if count != len(entries) {
		t.Fatalf("tools/ holds %d folders, registry.yaml lists %d", count, len(entries))
	}
}

// internal-git is the only gate and must be first: the others read what it
// records.
func TestOnlyTheFirstToolIsAGate(t *testing.T) {
	for i, tool := range All(Deps{}) {
		if tool.Gate() != (i == 0) {
			t.Errorf("%s: Gate() = %v at position %d", tool.Name(), tool.Gate(), i)
		}
	}
}
