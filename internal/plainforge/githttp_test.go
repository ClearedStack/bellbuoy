package plainforge_test

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ClearedStack/bellbuoy/internal/codes"
	"github.com/ClearedStack/bellbuoy/internal/plainforge"
)

// gitRig wraps real git-http-backend behind Plainforge's exact multi-repo URL
// shape and capability/content-type checks so tests exercise both real Git
// pack/object rules (checked with git fsck --full --strict) and Plainforge's
// narrower wire rules.
type gitRig struct {
	t       *testing.T
	root    string
	backend string
	srv     *httptest.Server

	mu         sync.Mutex
	forceCode  int
	forceCount int
	pushHook   func(repo, ref string)
}

func newGitRig(t *testing.T, repos ...string) *gitRig {
	t.Helper()
	gitBin, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git binary not installed")
	}
	out, err := exec.Command(gitBin, "--exec-path").Output()
	if err != nil {
		t.Fatalf("git --exec-path: %v", err)
	}
	backend := filepath.Join(strings.TrimSpace(string(out)), "git-http-backend")
	if _, err := os.Stat(backend); err != nil {
		t.Skipf("git-http-backend not installed at %s", backend)
	}

	root := t.TempDir()
	r := &gitRig{t: t, root: root, backend: backend}
	for _, name := range repos {
		r.initBare(name)
	}
	r.srv = httptest.NewServer(http.HandlerFunc(r.serveHTTP))
	t.Cleanup(r.srv.Close)
	return r
}

func (r *gitRig) initBare(name string) string {
	r.t.Helper()
	dir := filepath.Join(r.root, name+".git")
	r.git(r.root, "init", "--bare", "--initial-branch=main", dir)
	r.git(dir, "config", "http.receivepack", "true")
	return dir
}

func (r *gitRig) seedMain(repo string, files map[string]fileSpec) {
	r.t.Helper()
	work := filepath.Join(r.t.TempDir(), "work")
	r.git(r.root, "clone", filepath.Join(r.root, repo+".git"), work)
	r.git(work, "checkout", "-b", "main")
	r.git(work, "config", "user.name", "Test")
	r.git(work, "config", "user.email", "test@clearedstack.internal")
	for rel, spec := range files {
		full := filepath.Join(work, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			r.t.Fatal(err)
		}
		mode := os.FileMode(0o644)
		if spec.exec {
			mode = 0o755
		}
		if err := os.WriteFile(full, []byte(spec.content), mode); err != nil {
			r.t.Fatal(err)
		}
	}
	r.git(work, "add", ".")
	r.git(work, "commit", "-m", "Initial seed")
	r.git(work, "push", "origin", "HEAD:refs/heads/main")
}

type fileSpec struct {
	content string
	exec    bool
}

func (r *gitRig) git(dir string, args ...string) string {
	r.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		r.t.Fatalf("git %v in %s failed: %v\n%s", args, dir, err, out)
	}
	return string(out)
}

func (r *gitRig) fsckAll(repos ...string) {
	r.t.Helper()
	for _, name := range repos {
		dir := filepath.Join(r.root, name+".git")
		r.git(dir, "fsck", "--full", "--strict")
	}
}

func (r *gitRig) forceHTTP(status, count int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.forceCode = status
	r.forceCount = count
}

func (r *gitRig) serveHTTP(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	if r.forceCount > 0 {
		r.forceCount--
		code := r.forceCode
		r.mu.Unlock()
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(code)
		io.WriteString(w, "E_TIMEOUT: simulated\n")
		return
	}
	hook := r.pushHook
	r.mu.Unlock()

	// Enforce Plainforge's exact Content-Type and capability rules before
	// handing the request to git-http-backend.
	trimmed := strings.TrimPrefix(req.URL.Path, "/")
	seg, rest, ok := strings.Cut(trimmed, "/")
	repoName, hasGit := strings.CutSuffix(seg, ".git")
	if !ok || !hasGit {
		http.Error(w, "E_NEEDS_SUBTOOL: no such repository", http.StatusNotFound)
		return
	}
	if _, err := os.Stat(filepath.Join(r.root, seg)); err != nil {
		http.Error(w, "E_NEEDS_SUBTOOL: no such repository", http.StatusNotFound)
		return
	}

	switch {
	case rest == "info/refs" && req.Method == http.MethodGet:
		svc := req.URL.Query().Get("service")
		if svc != "git-upload-pack" && svc != "git-receive-pack" {
			http.Error(w, "E_NEEDS_SUBTOOL: bad service", http.StatusUnprocessableEntity)
			return
		}
	case rest == "git-upload-pack" && req.Method == http.MethodPost:
		if req.Header.Get("Content-Type") != "application/x-git-upload-pack-request" {
			http.Error(w, "E_PARSE: bad content type", http.StatusBadRequest)
			return
		}
		body, _ := io.ReadAll(req.Body)
		if err := checkPlainforgeUploadCaps(body); err != nil {
			http.Error(w, err.Error(), http.StatusUnprocessableEntity)
			return
		}
		req.Body = io.NopCloser(bytes.NewReader(body))
		req.ContentLength = int64(len(body))
	case rest == "git-receive-pack" && req.Method == http.MethodPost:
		if req.Header.Get("Content-Type") != "application/x-git-receive-pack-request" {
			http.Error(w, "E_PARSE: bad content type", http.StatusBadRequest)
			return
		}
		body, _ := io.ReadAll(req.Body)
		ref, err := checkPlainforgeReceiveCaps(body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusUnprocessableEntity)
			return
		}
		if hook != nil {
			hook(repoName, ref)
		}
		req.Body = io.NopCloser(bytes.NewReader(body))
		req.ContentLength = int64(len(body))
	default:
		http.Error(w, "E_NEEDS_SUBTOOL: unknown path", http.StatusNotFound)
		return
	}

	h := &cgi.Handler{
		Path:   r.backend,
		Stderr: io.Discard,
		Env: []string{
			"GIT_PROJECT_ROOT=" + r.root,
			"GIT_HTTP_EXPORT_ALL=1",
		},
	}
	h.ServeHTTP(w, req)
}

func checkPlainforgeUploadCaps(body []byte) error {
	if len(body) < 4 {
		return io.ErrUnexpectedEOF
	}
	firstEnd := bytes.IndexByte(body[4:], '\n')
	if firstEnd < 0 {
		return io.ErrUnexpectedEOF
	}
	line := string(body[4 : 4+firstEnd])
	parts := strings.Fields(line)
	if len(parts) < 2 || parts[0] != "want" {
		return io.ErrUnexpectedEOF
	}
	for _, capName := range parts[2:] {
		if capName != "multi_ack_detailed" && !strings.HasPrefix(capName, "agent=") {
			return codes.Unfamiliar("unsupported upload-pack capability")
		}
	}
	return nil
}

func checkPlainforgeReceiveCaps(body []byte) (string, error) {
	if len(body) < 4 {
		return "", io.ErrUnexpectedEOF
	}
	nul := bytes.IndexByte(body[4:], 0)
	if nul < 0 {
		return "", io.ErrUnexpectedEOF
	}
	cmdFields := strings.Fields(string(body[4 : 4+nul]))
	if len(cmdFields) != 3 {
		return "", io.ErrUnexpectedEOF
	}
	rest := body[4+nul+1:]
	nl := bytes.IndexByte(rest, '\n')
	if nl < 0 {
		return "", io.ErrUnexpectedEOF
	}
	for _, capName := range strings.Fields(string(rest[:nl])) {
		if capName != "report-status" && capName != "ofs-delta" && !strings.HasPrefix(capName, "agent=") {
			return "", codes.Unfamiliar("unsupported receive-pack capability")
		}
	}
	return cmdFields[2], nil
}

func TestSmartHTTPProposeChangeAndIdempotency(t *testing.T) {
	rig := newGitRig(t, "clearedstack.com", "sextant", "emptyrepo")
	rig.seedMain("clearedstack.com", map[string]fileSpec{
		// Names chosen to exercise Git's directory-vs-file sort order:
		// "catalog-a" < "catalog.json" < "catalog/" < "catalog0".
		"src/data/catalog-a":                   {content: "a\n"},
		"src/data/catalog.json":                {content: "{\"v\":1}\n"},
		"src/data/catalog/nested.json":         {content: "{\"nested\":true}\n"},
		"src/data/catalog0":                    {content: "0\n"},
		"scripts/check.sh":                     {content: "#!/bin/sh\necho ok\n", exec: true},
		"unchanged/readme.txt":                 {content: "keep me\n"},
	})
	rig.seedMain("sextant", map[string]fileSpec{
		"README.md": {content: "sextant\n"},
	})
	// Pack objects with deltas on clearedstack.com to exercise OFS_DELTA/REF_DELTA unpacking.
	rig.git(filepath.Join(rig.root, "clearedstack.com.git"), "repack", "-a", "-d")

	client, err := plainforge.NewClient(rig.srv.URL, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	// 1. Propose a DATA change on an existing file with tricky sibling names.
	change := plainforge.Change{
		Repo:    "clearedstack.com",
		Branch:  "edit/EP-0001",
		Path:    "src/data/catalog.json",
		Content: "{\"v\":2}\n",
		Title:   "Portal proposal EP-0001",
		Message: "Portal proposal EP-0001\n\nClass: DATA\n",
	}
	created, err := client.ProposeChange(ctx, change)
	if err != nil || !created {
		t.Fatalf("first ProposeChange: created=%v err=%v", created, err)
	}

	// 2. Repeating the identical proposal is a clean no-op (created == false).
	created, err = client.ProposeChange(ctx, change)
	if err != nil || created {
		t.Fatalf("repeat ProposeChange: created=%v err=%v", created, err)
	}

	// 3. Asking for different content on the same branch is refused with E_PARSE.
	conflictChange := change
	conflictChange.Content = "{\"v\":3}\n"
	created, err = client.ProposeChange(ctx, conflictChange)
	if created || codes.From(err) == nil || codes.From(err).Code != codes.Parse {
		t.Fatalf("conflicting ProposeChange: created=%v err=%v", created, err)
	}

	// 4. Updating an executable script preserves mode 100755.
	execChange := plainforge.Change{
		Repo:    "clearedstack.com",
		Branch:  "edit/EP-0002",
		Path:    "scripts/check.sh",
		Content: "#!/bin/sh\necho updated\n",
		Title:   "Update script",
		Message: "Update script",
	}
	if created, err := client.ProposeChange(ctx, execChange); err != nil || !created {
		t.Fatalf("exec ProposeChange: created=%v err=%v", created, err)
	}

	// 5. Creating a brand-new nested directory path on sextant and on an empty repo.
	nestedChange := plainforge.Change{
		Repo:    "sextant",
		Branch:  "edit/EP-0003",
		Path:    "tools/sample-portal/options.json",
		Content: "{\"timeout\":30}\n",
		Title:   "Add options",
		Message: "Add options",
	}
	if created, err := client.ProposeChange(ctx, nestedChange); err != nil || !created {
		t.Fatalf("nested ProposeChange: created=%v err=%v", created, err)
	}
	emptyRepoChange := plainforge.Change{
		Repo:    "emptyrepo",
		Branch:  "edit/EP-0004",
		Path:    "deploy/config.yaml",
		Content: "port: 8080\n",
		Title:   "Bootstrap config",
		Message: "Bootstrap config",
	}
	if created, err := client.ProposeChange(ctx, emptyRepoChange); err != nil || !created {
		t.Fatalf("empty repo ProposeChange: created=%v err=%v", created, err)
	}

	// 6. Writing to main, HEAD, or bellbuoy/tasks is always refused locally.
	for _, forbidden := range []string{"main", "HEAD", "bellbuoy/tasks", "refs/heads/main", "../escape"} {
		bad := change
		bad.Branch = forbidden
		if _, err := client.ProposeChange(ctx, bad); codes.From(err) == nil || codes.From(err).Code != codes.Parse {
			t.Errorf("branch %q was not refused with E_PARSE: %v", forbidden, err)
		}
	}

	// 7. Verify git fsck --full --strict and inspect the committed Git trees.
	rig.fsckAll("clearedstack.com", "sextant", "emptyrepo")

	csDir := filepath.Join(rig.root, "clearedstack.com.git")
	if got := rig.git(csDir, "show", "refs/heads/main:src/data/catalog.json"); got != "{\"v\":1}\n" {
		t.Fatalf("main was modified: %q", got)
	}
	if got := rig.git(csDir, "show", "refs/heads/edit/EP-0001:src/data/catalog.json"); got != "{\"v\":2}\n" {
		t.Fatalf("edit/EP-0001 content = %q", got)
	}
	if got := rig.git(csDir, "show", "refs/heads/edit/EP-0001:unchanged/readme.txt"); got != "keep me\n" {
		t.Fatalf("sibling file lost: %q", got)
	}
	lsTree := rig.git(csDir, "ls-tree", "refs/heads/edit/EP-0002", "scripts/check.sh")
	if !strings.HasPrefix(lsTree, "100755 blob ") {
		t.Fatalf("executable mode was not preserved: %s", lsTree)
	}
}

func TestSmartHTTPTaskLifecycleAcrossPodRestartAndCASConflict(t *testing.T) {
	rig := newGitRig(t, "keelworks")
	rig.seedMain("keelworks", map[string]fileSpec{
		"README.md": {content: "keelworks\n"},
	})
	ctx := context.Background()

	client1, err := plainforge.NewClient(rig.srv.URL, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}

	// Nothing open initially.
	if _, ok, err := client1.FindOpenTask(ctx, "keelworks", "fp-1"); err != nil || ok {
		t.Fatalf("initial FindOpenTask: ok=%v err=%v", ok, err)
	}

	// Open first task and add an occurrence.
	t1, err := client1.OpenTask(ctx, "keelworks", plainforge.TaskDraft{
		Key:   "fp-1",
		Title: "keelworks: E_PARSE [fp-1]",
		Body:  "First seen on PA-0001",
	})
	if err != nil || t1.ID != "keelworks#1" || t1.Occurrences != 1 || !t1.Open {
		t.Fatalf("OpenTask #1: %+v err=%v", t1, err)
	}
	t1, err = client1.AddOccurrence(ctx, "keelworks", "keelworks#1")
	if err != nil || t1.Occurrences != 2 {
		t.Fatalf("AddOccurrence #1: %+v err=%v", t1, err)
	}

	// Simulate pod rescheduling onto a new node with an empty in-memory cache.
	client2, err := plainforge.NewClient(rig.srv.URL, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	found, ok, err := client2.FindOpenTask(ctx, "keelworks", "fp-1")
	if err != nil || !ok || found.ID != "keelworks#1" || found.Occurrences != 2 {
		t.Fatalf("FindOpenTask on fresh pod: found=%+v ok=%v err=%v", found, ok, err)
	}

	// Inject one concurrent push to refs/heads/bellbuoy/tasks right before
	// client1's receive-pack to force a CAS conflict and retry.
	injected := false
	rig.mu.Lock()
	rig.pushHook = func(repo, ref string) {
		if !injected && repo == "keelworks" && ref == plainforge.TasksRef {
			injected = true
			// Use a separate client without the hook firing recursively.
			rig.mu.Lock()
			rig.pushHook = nil
			rig.mu.Unlock()
			if _, err := client2.AddOccurrence(ctx, "keelworks", "keelworks#1"); err != nil {
				t.Errorf("concurrent AddOccurrence failed: %v", err)
			}
		}
	}
	rig.mu.Unlock()

	t1AfterRace, err := client1.AddOccurrence(ctx, "keelworks", "keelworks#1")
	if err != nil || t1AfterRace.Occurrences != 4 {
		t.Fatalf("AddOccurrence after CAS retry: got %+v err=%v, want occurrences=4", t1AfterRace, err)
	}

	// Close keelworks#1; a subsequent failure with the same key opens keelworks#2.
	closed, err := client2.CloseTask(ctx, "keelworks", "keelworks#1")
	if err != nil || !closed {
		t.Fatalf("CloseTask: closed=%v err=%v", closed, err)
	}
	if _, ok, err := client1.FindOpenTask(ctx, "keelworks", "fp-1"); err != nil || ok {
		t.Fatalf("FindOpenTask after close: ok=%v err=%v", ok, err)
	}
	t2, err := client1.OpenTask(ctx, "keelworks", plainforge.TaskDraft{
		Key:   "fp-1",
		Title: "keelworks: E_PARSE [fp-1]",
		Body:  "Reopened as new task",
	})
	if err != nil || t2.ID != "keelworks#2" || t2.Occurrences != 1 {
		t.Fatalf("OpenTask #2: %+v err=%v", t2, err)
	}

	rig.fsckAll("keelworks")
	kwDir := filepath.Join(rig.root, "keelworks.git")
	// refs/heads/main must still hold only its single seed commit.
	mainCount := strings.TrimSpace(rig.git(kwDir, "rev-list", "--count", "refs/heads/main"))
	if mainCount != "1" {
		t.Fatalf("refs/heads/main commit count = %s, want 1", mainCount)
	}
}

func TestSmartHTTPErrorClassificationAndNoProxyOrLeak(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")

	for _, badAddr := range []string{"", "plainforge:8080", "ftp://plainforge", "http://user@plainforge", "http://plainforge/subpath"} {
		if _, err := plainforge.NewClient(badAddr, time.Second); codes.From(err) == nil || codes.From(err).Code != codes.Parse {
			t.Errorf("bad address %q was not refused with E_PARSE: %v", badAddr, err)
		}
	}

	rig := newGitRig(t, "muster")
	client, err := plainforge.NewClient(rig.srv.URL, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	// Transient HTTP statuses map to E_TIMEOUT and never leak server URLs.
	for _, status := range []int{http.StatusTooManyRequests, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout} {
		rig.forceHTTP(status, 1)
		_, _, err := client.FindOpenTask(ctx, "muster", "k")
		e := codes.From(err)
		if e == nil || e.Code != codes.Timeout {
			t.Fatalf("status %d: got %v, want E_TIMEOUT", status, err)
		}
		if strings.Contains(err.Error(), rig.srv.URL) || strings.Contains(err.Error(), "127.0.0.1") {
			t.Fatalf("error leaked address: %v", err)
		}
	}

	// Non-transient unexpected HTTP statuses (including redirects) map to E_PARSE.
	for _, status := range []int{http.StatusMovedPermanently, http.StatusBadRequest, http.StatusNotFound, http.StatusUnprocessableEntity} {
		rig.forceHTTP(status, 1)
		_, _, err := client.FindOpenTask(ctx, "muster", "k")
		e := codes.From(err)
		if e == nil || e.Code != codes.Parse {
			t.Fatalf("status %d: got %v, want E_PARSE", status, err)
		}
	}

	// SyncMirror reports E_NEEDS_SUBTOOL honestly.
	if err := client.SyncMirror(ctx, "muster", []string{"source"}); codes.From(err) == nil || codes.From(err).Code != codes.NeedsSubtool {
		t.Fatalf("SyncMirror: got %v, want E_NEEDS_SUBTOOL", err)
	}
}
