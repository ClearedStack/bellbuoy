package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

const (
	jobsFile   = "../../testdata/replay/jobs.jsonl"
	ownersFile = "../../testdata/ownership_map.json"
	wantFile   = "../../testdata/replay/want.json"
)

func noEnv(string) string { return "" }

func replayOnce(t *testing.T, getenv func(string) string, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	var out, errs bytes.Buffer
	code = run(context.Background(), append([]string{"replay"}, args...), getenv, &out, &errs)
	return out.String(), errs.String(), code
}

// The saved example for the whole worker: seventeen jobs of every kind in,
// the exact state of Muster, Plainforge and the webhook out.
func TestReplayMatchesTheSavedExample(t *testing.T) {
	want, err := os.ReadFile(wantFile)
	if err != nil {
		t.Fatal(err)
	}
	got, _, code := replayOnce(t, noEnv, "-jobs", jobsFile, "-owners", ownersFile)
	if code != 0 {
		t.Fatalf("exit code %d", code)
	}
	if got != string(want) {
		t.Fatalf("replay output differs from %s\n--- got\n%s", wantFile, got)
	}
}

func TestReplayIsTheSameEveryTime(t *testing.T) {
	first, firstLog, _ := replayOnce(t, noEnv, "-jobs", jobsFile, "-owners", ownersFile)
	for i := 0; i < 5; i++ {
		again, againLog, _ := replayOnce(t, noEnv, "-jobs", jobsFile, "-owners", ownersFile, "-spool", t.TempDir())
		if again != first || againLog != firstLog {
			t.Fatal("the same input gave different output")
		}
	}
}

// Nothing from a failure message may reach anything that leaves Bellbuoy:
// not the tasks, the events, the alerts, or the log.
func TestReplayOutputHoldsNoAddressesOrNodeNames(t *testing.T) {
	out, logs, _ := replayOnce(t, noEnv, "-jobs", jobsFile, "-owners", ownersFile)
	for _, leak := range []string{"10.0.0.7", "10.0.0.9", "5432", "node-3", "node-8", "31s", "12s"} {
		if strings.Contains(out, leak) || strings.Contains(logs, leak) {
			t.Errorf("output leaks %q", leak)
		}
	}
}

func TestReplayPostsAlertsToAConfiguredWebhook(t *testing.T) {
	var mu sync.Mutex
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(b))
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	getenv := func(key string) string {
		if key == WebhookEnv {
			return srv.URL + "/hooks/made-up"
		}
		return ""
	}
	out, logs, code := replayOnce(t, getenv, "-jobs", jobsFile, "-owners", ownersFile)
	if code != 0 {
		t.Fatalf("exit code %d", code)
	}
	var rep struct {
		Alerts []json.RawMessage `json:"alerts"`
		Held   int               `json:"held"`
	}
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatal(err)
	}
	if len(bodies) != 5 || len(rep.Alerts) != 0 || rep.Held != 0 {
		t.Fatalf("webhook got %d alerts, report kept %d, held %d", len(bodies), len(rep.Alerts), rep.Held)
	}
	if !strings.Contains(bodies[0], `"kind":"crash"`) || !strings.Contains(bodies[0], `"tier":"P0"`) {
		t.Fatalf("first alert = %s", bodies[0])
	}
	if strings.Contains(out, "made-up") || strings.Contains(logs, "made-up") || strings.Contains(logs, srv.URL) {
		t.Fatal("the webhook address appeared in the output")
	}
}

func TestReplayWithAWebhookThatIsDownHoldsTheAlerts(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	address := srv.URL
	srv.Close()
	getenv := func(key string) string {
		if key == WebhookEnv {
			return address
		}
		return ""
	}
	dir := t.TempDir()
	out, _, code := replayOnce(t, getenv, "-jobs", jobsFile, "-owners", ownersFile, "-spool", dir)
	if code != 0 {
		t.Fatalf("exit code %d", code)
	}
	var rep struct {
		Answers []json.RawMessage `json:"answers"`
		Held    int               `json:"held"`
	}
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatal(err)
	}
	// Five jobs want an alert. They stay staged on disk, unanswered. The
	// other twelve are answered as usual.
	if rep.Held != 5 || len(rep.Answers) != 12 {
		t.Fatalf("held = %d, answers = %d", rep.Held, len(rep.Answers))
	}
	files, _ := filepath.Glob(filepath.Join(dir, "*.json"))
	if len(files) != 5 {
		t.Fatalf("%d records in the spool, want 5", len(files))
	}
}

func TestBadInputsAreReportedWithoutPaths(t *testing.T) {
	dir := t.TempDir()
	broken := filepath.Join(dir, "broken.jsonl")
	os.WriteFile(broken, []byte("{\"id\":\"job-1\",\"lane\":\"normal\",\"kind\":\"error\",\"payload\":{}}\nnot a job\n"), 0o600)
	extra := filepath.Join(dir, "extra.jsonl")
	os.WriteFile(extra, []byte("{\"id\":\"job-1\",\"lane\":\"normal\",\"kind\":\"error\",\"payload\":{},\"priority\":9}\n"), 0o600)
	badMap := filepath.Join(dir, "map.json")
	os.WriteFile(badMap, []byte("{"), 0o600)

	cases := []struct {
		name string
		args []string
		want string
	}{
		{"missing jobs file", []string{"-jobs", filepath.Join(dir, "absent.jsonl"), "-owners", ownersFile}, "bellbuoy: E_PARSE: jobs file cannot be read\n"},
		{"broken line", []string{"-jobs", broken, "-owners", ownersFile}, "bellbuoy: E_PARSE: jobs file has a line that is not a job\n"},
		{"unknown job field", []string{"-jobs", extra, "-owners", ownersFile}, "bellbuoy: E_PARSE: jobs file has a line that is not a job\n"},
		{"missing map", []string{"-jobs", jobsFile, "-owners", filepath.Join(dir, "absent.json")}, "bellbuoy: E_PARSE: ownership map cannot be read\n"},
		{"broken map", []string{"-jobs", jobsFile, "-owners", badMap}, "bellbuoy: E_PARSE: ownership map is not in the expected form\n"},
	}
	for _, c := range cases {
		out, errs, code := replayOnce(t, noEnv, c.args...)
		if code != 1 || out != "" || errs != c.want {
			t.Errorf("%s: code=%d stdout=%q stderr=%q", c.name, code, out, errs)
		}
	}

	getenv := func(string) string { return "ftp://example.invalid/made-up" }
	_, errs, code := replayOnce(t, getenv, "-jobs", jobsFile, "-owners", ownersFile)
	if code != 1 || errs != "bellbuoy: E_PARSE: webhook address is not usable\n" {
		t.Errorf("bad webhook address: code=%d stderr=%q", code, errs)
	}
}

func TestUsage(t *testing.T) {
	for _, args := range [][]string{nil, {"serve"}, {"replay"}, {"replay", "-jobs", jobsFile}, {"replay", "-jobs", jobsFile, "-owners", ownersFile, "stray"}} {
		var out, errs bytes.Buffer
		if code := run(context.Background(), args, noEnv, &out, &errs); code != 2 {
			t.Errorf("run(%v) = %d, want 2", args, code)
		}
		if out.Len() != 0 || !strings.Contains(errs.String(), "bellbuoy replay") {
			t.Errorf("run(%v): usage not shown on stderr", args)
		}
	}
	var out, errs bytes.Buffer
	if code := run(context.Background(), []string{"help"}, noEnv, &out, &errs); code != 0 || !strings.Contains(out.String(), "bellbuoy replay") {
		t.Error("help must print the usage and succeed")
	}
}

// The names in the program must be the ones in the registry entry.
func TestNamesComeFromTheRegistry(t *testing.T) {
	raw, err := os.ReadFile("../../component.json")
	if err != nil {
		t.Fatal(err)
	}
	var component struct {
		Slug    string `json:"slug"`
		Image   string `json:"image"`
		Version string `json:"version"`
	}
	if err := json.Unmarshal(raw, &component); err != nil {
		t.Fatal(err)
	}
	if Name != component.Slug || Name != component.Image || Version != component.Version {
		t.Fatalf("program says %s %s, component.json says %s (image %s) %s", Name, Version, component.Slug, component.Image, component.Version)
	}
	var out bytes.Buffer
	run(context.Background(), []string{"version"}, noEnv, &out, io.Discard)
	if out.String() != "bellbuoy v0.1.0-draft contract v1\n" {
		t.Fatalf("version prints %q", out.String())
	}
}

// Two of the build rules can be checked from here: the image is built
// without Docker, and the license file is kept.
func TestRepositoryRules(t *testing.T) {
	for _, name := range []string{"Dockerfile", "dockerfile", "Containerfile", ".dockerignore"} {
		if _, err := os.Stat(filepath.Join("..", "..", name)); err == nil {
			t.Errorf("%s must not exist: the image is built from source with ko", name)
		}
	}
	license, err := os.ReadFile("../../LICENSE")
	if err != nil || !strings.HasPrefix(string(license), "MIT License") {
		t.Error("the MIT LICENSE file is missing or changed")
	}
	if _, err := os.Stat("../../.ko.yaml"); err != nil {
		t.Error(".ko.yaml is missing")
	}
}
