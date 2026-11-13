// Package tools holds the sub-tools of Bellbuoy, one folder each, and the
// registry that lists them.
package tools

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRegistryMatchesFolders checks that registry.yaml and the folders under
// tools/ agree, and that each listed sub-tool has its three parts and is one
// of the four names the specification gives.
//
// The standard library has no YAML reader, so this reads only the simple
// "key: value" lines the registry uses.
func TestRegistryMatchesFolders(t *testing.T) {
	allowed := map[string]bool{"internal-git": true, "github-mirror": true, "webhook-alert": true, "portal-sse": true}

	f, err := os.Open("registry.yaml")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	listed := map[string]map[string]string{}
	var cur map[string]string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") || line == "tools:" {
			continue
		}
		if strings.HasPrefix(line, "- ") {
			cur = map[string]string{}
			line = strings.TrimPrefix(line, "- ")
		}
		k, v, ok := strings.Cut(line, ": ")
		if !ok || cur == nil {
			t.Fatalf("cannot read registry line %q", line)
		}
		cur[k] = v
		if k == "name" {
			listed[v] = cur
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}

	for name, e := range listed {
		if !allowed[name] {
			t.Errorf("%s is not a sub-tool named by the specification", name)
		}
		if e["path"] != "tools/"+name {
			t.Errorf("%s: path is %q", name, e["path"])
		}
		for _, part := range []string{"match", "logic", "fixtures"} {
			p := e[part]
			if !strings.HasPrefix(p, "tools/"+name+"/") {
				t.Errorf("%s: %s is %q, want something under tools/%s/", name, part, p, name)
				continue
			}
			if _, err := os.Stat(filepath.Join("..", filepath.FromSlash(p))); err != nil {
				t.Errorf("%s: %s does not exist: %s", name, part, p)
			}
		}
	}

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.IsDir() && listed[e.Name()] == nil {
			t.Errorf("folder tools/%s is not in registry.yaml", e.Name())
		}
	}
	if len(listed) == 0 {
		t.Error("registry lists nothing")
	}
}
