package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ClearedStack/bellbuoy/internal/muster/mustertest"
	"github.com/ClearedStack/bellbuoy/internal/plainforge"
	"github.com/ClearedStack/bellbuoy/internal/plainforge/plainforgetest"
)

// syncBuffer is a log that the worker writes and the test reads.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// served is one run of "bellbuoy serve" against a fake Muster.
type served struct {
	t      *testing.T
	fake   *mustertest.Fake
	dir    string
	logs   *syncBuffer
	cancel context.CancelFunc
	done   chan int
}

const token = "made-up-service-account-token"

func startServe(t *testing.T, fake *mustertest.Fake, dir string) *served {
	t.Helper()
	return startServeEnv(t, fake, dir, noEnv)
}

func startServeEnv(t *testing.T, fake *mustertest.Fake, dir string, getenv func(string) string) *served {
	t.Helper()
	tokenFile := filepath.Join(dir, "token")
	if err := os.WriteFile(tokenFile, []byte(token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := &served{t: t, fake: fake, dir: dir, logs: &syncBuffer{}, done: make(chan int, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	args := []string{"serve", "-muster", fake.Server.URL, "-owners", ownersFile,
		"-spool", filepath.Join(dir, "spool"), "-token-file", tokenFile, "-idle", "5ms"}
	go func() { s.done <- run(ctx, args, getenv, &bytes.Buffer{}, s.logs) }()
	return s
}

// until waits for a condition, failing the test if it does not come true.
func (s *served) until(what string, cond func() bool) {
	s.t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			s.t.Fatalf("timed out waiting for %s\nlog:\n%s", what, s.logs.String())
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func (s *served) stop() string {
	s.t.Helper()
	s.cancel()
	select {
	case code := <-s.done:
		if code != 0 {
			s.t.Fatalf("serve exited with %d\nlog:\n%s", code, s.logs.String())
		}
	case <-time.After(15 * time.Second):
		s.t.Fatal("serve did not stop")
	}
	return s.logs.String()
}

func (s *served) records() int {
	files, _ := filepath.Glob(filepath.Join(s.dir, "spool", "*.json"))
	return len(files)
}

func (s *served) ledger() []plainforge.Task {
	s.t.Helper()
	l, err := plainforge.OpenLedger(filepath.Join(s.dir, "spool", "tasks.ledger"))
	if err != nil {
		s.t.Fatal(err)
	}
	return l.Tasks()
}

// incident is the lease of an incident step, in the shape Muster's contract
// gives it.
func incident(id, state, worker, code string) map[string]any {
	return map[string]any{"kind": "incident", "id": id, "state": state, "lane": "normal",
		"incident": map[string]any{"id": id, "worker": worker, "code": code, "state": state}}
}

// The whole lease loop against a Muster that is strict about contract v1:
// every held or paused item gets a task, and each step is completed with the
// item's state unchanged.
func TestServeFilesATaskForEveryHeldOrPausedItem(t *testing.T) {
	fake := mustertest.New(token)
	defer fake.Close()
	s := startServe(t, fake, t.TempDir())

	// Nothing to lease yet: a 204, after which serve waits and asks again.
	s.until("serve to keep asking", func() bool { return fake.Count("/v1/lease") >= 3 })
	if n := len(fake.Calls()) - fake.Count("/v1/lease"); n != 0 {
		t.Fatalf("%d calls other than lease were made with nothing to do", n)
	}

	fake.Offer(incident("PA-0001", "observed", "keelworks", "E_PARSE"))
	fake.Offer(incident("PA-0002", "observed", "keelworks", "E_PARSE"))
	fake.Offer(incident("PA-0003", "recorded", "keelworks", "E_NEEDS_SUBTOOL"))
	fake.Offer(incident("PA-0004", "observed", "newcomer", "E_PARSE")) // no owner recorded
	fake.Offer(map[string]any{"kind": "incident", "id": "PA-0005", "state": "observed", "lane": "normal",
		"incident": map[string]any{"id": "PA-0005", "code": "E_PARSE", "state": "observed"}}) // no worker
	fake.Offer(map[string]any{"kind": "audit_note", "id": "PA-0006", "state": "observed", "lane": "urgent",
		"note": map[string]any{"target": "made-up"}}) // unfamiliar job kind
	s.until("four steps to end and two pauses to be refused", func() bool {
		return len(fake.Ended()) == 4 && fake.Count("/v1/needs_subtool") == 2
	})
	logs := s.stop()

	ended := fake.Ended()
	type row struct{ id, call, state, code, result string }
	want := []row{
		// Completed with the state unchanged and no result, as the
		// contract says of an incident.
		{"PA-0001", "complete", "observed", "", ""},
		{"PA-0002", "complete", "observed", "", ""},
		{"PA-0003", "complete", "recorded", "", ""},
		// Broken data: failed with E_PARSE.
		{"PA-0005", "fail", "observed", "E_PARSE", ""},
	}
	for i, w := range want {
		got := row{ended[i].ID, ended[i].Call, ended[i].State, ended[i].Code, ended[i].Result}
		if got != w {
			t.Errorf("step %d: got %+v, want %+v", i, got, w)
		}
	}
	// The two steps with no rule were not ended. Muster would not let them
	// be paused, so their leases are left to run out and Muster will offer
	// them again.
	if held := fake.Held(); len(held) != 2 {
		t.Errorf("leases still out = %v, want the two steps with no rule", held)
	}
	for _, reason := range []string{`reason="no repository is recorded as owning this worker"`, `reason="no rule covers this kind of job"`} {
		if !strings.Contains(logs, "result=pause-refused code=E_NEEDS_SUBTOOL "+reason) {
			t.Errorf("log does not say %s:\n%s", reason, logs)
		}
	}

	// Two tasks cover the three items, and they are on disk.
	tasks := s.ledger()
	if len(tasks) != 2 || tasks[0].Occurrences != 2 || tasks[1].Occurrences != 1 {
		t.Fatalf("ledger = %+v", tasks)
	}
	if tasks[0].ID != "keelworks#1" || tasks[1].ID != "keelworks#2" ||
		!strings.Contains(tasks[0].Body, "Failure pattern: held in observed") || !strings.Contains(tasks[0].Body, "First seen on item: PA-0001") ||
		!strings.Contains(tasks[1].Body, "Failure pattern: paused in recorded") {
		t.Errorf("task bodies:\n%s\n---\n%s", tasks[0].Body, tasks[1].Body)
	}

	// Every answer was acknowledged, so nothing is left in the spool.
	if n := s.records(); n != 0 {
		t.Errorf("%d records left in the spool", n)
	}
	// Neither the address nor the token is ever logged.
	for _, leak := range []string{fake.Server.URL, strings.TrimPrefix(fake.Server.URL, "http://"), token} {
		if strings.Contains(logs, leak) {
			t.Errorf("log holds %q", leak)
		}
	}
	if !strings.Contains(logs, "bellbuoy serve contract=v1 tools=internal-git ") || !strings.Contains(logs, "bellbuoy stopped staged=0") {
		t.Errorf("start or stop line missing:\n%s", logs)
	}
}

// The lease Muster's own contract example gives for an incident, taken from
// the saved example, ends as a task and a complete call exactly as the
// contract describes.
func TestServeHandlesMustersExampleOfAnIncident(t *testing.T) {
	raw, err := os.ReadFile("../../testdata/contract/lease-incident.json")
	if err != nil {
		t.Fatal(err)
	}
	var example struct {
		Expect struct {
			Body map[string]any `json:"body"`
		} `json:"expect"`
	}
	if err := json.Unmarshal(raw, &example); err != nil {
		t.Fatal(err)
	}
	step := example.Expect.Body
	// The fake hands out its own lease token, attempt count and expiry.
	delete(step, "lease")
	delete(step, "attempt")
	delete(step, "expires")

	fake := mustertest.New(token)
	defer fake.Close()
	s := startServe(t, fake, t.TempDir())
	fake.Offer(step)
	s.until("the step to end", func() bool { return len(fake.Ended()) == 1 })
	s.stop()

	ended := fake.Ended()[0]
	if ended.ID != "PA-0001" || ended.Call != "complete" || ended.State != "recorded" || ended.Result != "" || ended.Lease != "incident:PA-0001.1" {
		t.Fatalf("ended = %+v", ended)
	}
	tasks := s.ledger()
	if len(tasks) != 1 || tasks[0].Repo != "sextant" || tasks[0].Title != "sextant: E_PARSE [03d43879f9fa]" ||
		!strings.Contains(tasks[0].Body, "Failure pattern: held in recorded") {
		t.Fatalf("ledger = %+v", tasks)
	}
}

// The lease Muster's own contract example gives for an approved proposal,
// both with Countersign's in-memory results and after a Muster restart over
// PostgreSQL 17 (where file, reason and results are empty), ends as a
// proposed change or task and completes the proposal to building.
func TestServeHandlesMustersExampleOfAProposal(t *testing.T) {
	raw, err := os.ReadFile("../../testdata/contract/lease-proposal-results.json")
	if err != nil {
		t.Fatal(err)
	}
	var example struct {
		Expect struct {
			Body map[string]any `json:"body"`
		} `json:"expect"`
	}
	if err := json.Unmarshal(raw, &example); err != nil {
		t.Fatal(err)
	}
	step := example.Expect.Body
	delete(step, "lease")
	delete(step, "attempt")
	delete(step, "expires")

	// A second proposal shaped as Muster reads core.edit_proposal from
	// PostgreSQL 17 after a restart: file, reason and results are empty.
	restarted := map[string]any{
		"kind":  "proposal",
		"id":    "EP-0002",
		"state": "approved",
		"lane":  "urgent",
		"proposal": map[string]any{
			"target":     "sextant.sample-portal.options",
			"repository": "sextant",
			"file":       "",
			"edit_kind":  "CONFIG",
			"change":     "timeout = 30\n",
			"reason":     "",
		},
	}

	// A third proposal of class ISSUE opens one task in the owning repository.
	issueStep := map[string]any{
		"kind":  "proposal",
		"id":    "EP-0003",
		"state": "approved",
		"lane":  "urgent",
		"proposal": map[string]any{
			"target":     "bellbuoy.sample-rule.report",
			"repository": "bellbuoy",
			"file":       "tools/internal-git/logic.go",
			"edit_kind":  "ISSUE",
			"change":     "Explain how an approved proposal becomes a branch.",
			"reason":     "Readers look here first.",
		},
	}

	fake := mustertest.New(token)
	defer fake.Close()
	dir := t.TempDir()
	s := startServe(t, fake, dir)
	fake.Offer(step)
	fake.Offer(restarted)
	fake.Offer(issueStep)
	s.until("all three proposals to end", func() bool { return len(fake.Ended()) == 3 })
	s.stop()

	ended := fake.Ended()
	if ended[0].ID != "EP-0001" || ended[0].Call != "complete" || ended[0].State != "building" ||
		!strings.Contains(ended[0].Result, `"branch":"edit/EP-0001"`) {
		t.Fatalf("ended[0] = %+v", ended[0])
	}
	if ended[1].ID != "EP-0002" || ended[1].Call != "complete" || ended[1].State != "building" ||
		!strings.Contains(ended[1].Result, `"branch":"edit/EP-0002"`) {
		t.Fatalf("ended[1] = %+v", ended[1])
	}
	if ended[2].ID != "EP-0003" || ended[2].Call != "complete" || ended[2].State != "building" ||
		!strings.Contains(ended[2].Result, `"id":"bellbuoy#1"`) {
		t.Fatalf("ended[2] = %+v", ended[2])
	}
	l, err := plainforge.OpenLedger(filepath.Join(dir, "spool", "tasks.ledger"))
	if err != nil {
		t.Fatal(err)
	}
	changes := l.Changes()
	if len(changes) != 2 || changes[0].Repo != "clearedstack.com" || changes[0].Path != "src/data/sample-catalog.json" ||
		changes[1].Repo != "sextant" || changes[1].Path != "tools/sample-portal/options.json" {
		t.Fatalf("changes = %+v", changes)
	}
	tasks := l.Tasks()
	if len(tasks) != 1 || tasks[0].ID != "bellbuoy#1" || tasks[0].Key != "proposal-EP-0003" {
		t.Fatalf("tasks = %+v", tasks)
	}
}

// Tasks outlive the process. After a restart the same kind of incident adds
// to the task that is already there.
func TestServeCountsOnTheSameTaskAfterARestart(t *testing.T) {
	fake := mustertest.New(token)
	defer fake.Close()
	dir := t.TempDir()

	s := startServe(t, fake, dir)
	fake.Offer(incident("PA-0001", "observed", "keelworks", "E_PARSE"))
	s.until("the first step to end", func() bool { return len(fake.Ended()) == 1 })
	s.stop()

	s = startServe(t, fake, dir)
	fake.Offer(incident("PA-0002", "observed", "keelworks", "E_PARSE"))
	s.until("the second step to end", func() bool { return len(fake.Ended()) == 2 })
	s.stop()

	if tasks := s.ledger(); len(tasks) != 1 || tasks[0].ID != "keelworks#1" || tasks[0].Occurrences != 2 {
		t.Fatalf("ledger = %+v", tasks)
	}
}

// The spool record is deleted only after a 200. While Muster answers the
// complete call with an error the record stays, and the work is not redone.
func TestServeKeepsTheSpoolUntilMusterAnswers200(t *testing.T) {
	fake := mustertest.New(token)
	defer fake.Close()
	s := startServe(t, fake, t.TempDir())

	fake.ForcePath("/v1/complete", 1, 503, `{"error":"E_TIMEOUT","cause":"store","reason":"the store took too long"}`)
	fake.Offer(incident("PA-0001", "observed", "keelworks", "E_PARSE"))
	s.until("the first complete call", func() bool { return fake.Count("/v1/complete") == 1 })
	s.until("the result to be staged", func() bool { return s.records() == 1 })
	if len(fake.Ended()) != 0 {
		t.Fatal("the step ended although Muster answered 503")
	}

	// The retry comes one second later and is acknowledged.
	s.until("the step to end", func() bool { return len(fake.Ended()) == 1 })
	s.until("the record to be removed", func() bool { return s.records() == 0 })
	s.stop()

	if n := fake.Count("/v1/complete"); n != 2 {
		t.Fatalf("complete was called %d times, want 2", n)
	}
	if tasks := s.ledger(); len(tasks) != 1 || tasks[0].Occurrences != 1 {
		t.Fatalf("the failure was counted again on the retry: %+v", tasks)
	}
}

// A result that was staged when the process stopped is delivered by the
// next process, without leasing the step again and without redoing the work.
func TestServeDeliversAStagedResultAfterARestart(t *testing.T) {
	fake := mustertest.New(token)
	defer fake.Close()
	dir := t.TempDir()

	s := startServe(t, fake, dir)
	fake.ForcePath("/v1/complete", 1, 503, ``)
	fake.Offer(incident("PA-0001", "observed", "keelworks", "E_PARSE"))
	s.until("the result to be staged", func() bool { return fake.Count("/v1/complete") == 1 && s.records() == 1 })
	logs := s.stop()
	if !strings.Contains(logs, "bellbuoy stopped staged=1") {
		t.Fatalf("stop line does not report the staged result:\n%s", logs)
	}

	s = startServe(t, fake, dir)
	s.until("the step to end", func() bool { return len(fake.Ended()) == 1 })
	s.until("the record to be removed", func() bool { return s.records() == 0 })
	logs = s.stop()
	if !strings.Contains(logs, "staged=1 unreadable=0") {
		t.Fatalf("start line does not report the staged result:\n%s", logs)
	}
	if tasks := s.ledger(); len(tasks) != 1 || tasks[0].Occurrences != 1 {
		t.Fatalf("ledger = %+v", tasks)
	}
	if ended := fake.Ended(); ended[0].Call != "complete" || ended[0].State != "observed" {
		t.Fatalf("ended = %+v", ended)
	}
}

// With a webhook configured, the first incident of a kind alerts the owner
// and a repeat does not.
func TestServeAlertsOnceForANewKindOfIncident(t *testing.T) {
	var mu sync.Mutex
	var alerts []string
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		alerts = append(alerts, string(b))
		mu.Unlock()
	}))
	defer hook.Close()
	fake := mustertest.New(token)
	defer fake.Close()
	getenv := func(key string) string {
		if key == WebhookEnv {
			return hook.URL + "/hooks/made-up"
		}
		return ""
	}
	s := startServeEnv(t, fake, t.TempDir(), getenv)
	fake.Offer(incident("PA-0001", "observed", "keelworks", "E_PARSE"))
	fake.Offer(incident("PA-0002", "observed", "keelworks", "E_PARSE"))
	s.until("two steps to end", func() bool { return len(fake.Ended()) == 2 })
	logs := s.stop()

	mu.Lock()
	defer mu.Unlock()
	if len(alerts) != 1 || !strings.Contains(alerts[0], `"kind":"incident"`) || !strings.Contains(alerts[0], `"new":true`) {
		t.Fatalf("alerts = %v", alerts)
	}
	if !strings.Contains(logs, "tools=internal-git,webhook-alert ") {
		t.Errorf("start line does not list webhook-alert:\n%s", logs)
	}
	if strings.Contains(logs, "made-up") || strings.Contains(logs, hook.URL) {
		t.Error("the webhook address was logged")
	}
}

func TestServeRefusesToStartOnBadSettings(t *testing.T) {
	dir := t.TempDir()
	badLedger := filepath.Join(dir, "bad.ledger")
	os.WriteFile(badLedger, []byte("{"), 0o600)
	cases := []struct {
		name string
		args []string
		env  string
		want string
	}{
		{"bad address", []string{"-muster", "muster.example.invalid:8080/path", "-owners", ownersFile, "-spool", dir}, "", "bellbuoy: E_PARSE: muster address is not usable\n"},
		{"bad plainforge", []string{"-muster", "http://muster.example.invalid", "-plainforge", "plainforge.example.invalid:8080/path", "-owners", ownersFile, "-spool", dir}, "", "bellbuoy: E_PARSE: plainforge address is not usable\n"},
		{"plainforge and ledger", []string{"-muster", "http://muster.example.invalid", "-plainforge", "http://plainforge.example.invalid", "-ledger", badLedger, "-owners", ownersFile, "-spool", dir}, "", "bellbuoy: E_PARSE: plainforge address and task ledger cannot both be set\n"},
		{"missing map", []string{"-muster", "http://muster.example.invalid", "-owners", filepath.Join(dir, "absent.json"), "-spool", dir}, "", "bellbuoy: E_PARSE: ownership map cannot be read\n"},
		{"bad ledger", []string{"-muster", "http://muster.example.invalid", "-owners", ownersFile, "-spool", dir, "-ledger", badLedger}, "", "bellbuoy: E_PARSE: task ledger is not in the expected form\n"},
		{"bad listen", []string{"-muster", "http://muster.example.invalid", "-owners", ownersFile, "-spool", dir, "-listen", ":notaport"}, "", "bellbuoy: E_PARSE: probe listen address is not usable\n"},
		{"bad webhook", []string{"-muster", "http://muster.example.invalid", "-owners", ownersFile, "-spool", dir}, "ftp://example.invalid/made-up", "bellbuoy: E_PARSE: webhook address is not usable\n"},
	}
	for _, c := range cases {
		var out, errs bytes.Buffer
		getenv := func(key string) string {
			if key == WebhookEnv {
				return c.env
			}
			return ""
		}
		code := run(context.Background(), append([]string{"serve"}, c.args...), getenv, &out, &errs)
		if code != 1 || errs.String() != c.want {
			t.Errorf("%s: code=%d stderr=%q", c.name, code, errs.String())
		}
	}
}

// TestServeAgainstPlainforgeContractV1 drives bellbuoy serve end to end
// against mustertest.Fake and plainforgetest.Fake (Forge HTTP Contract v1),
// covering internal-git, github-mirror, bearer token rotation, pod restart
// onto an empty spool, and partial retry when mirror sync returns 503.
func TestServeAgainstPlainforgeContractV1(t *testing.T) {
	pf := plainforgetest.New("pf-token-1", "clearedstack.com", "sextant", "bellbuoy", "shipkeeper")
	defer pf.Close()

	fake := mustertest.New(token)
	defer fake.Close()

	pfTokenDir := t.TempDir()
	pfTokenFile := filepath.Join(pfTokenDir, "pf-token")
	if err := os.WriteFile(pfTokenFile, []byte("pf-token-1\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	startWithPlainforge := func(dir string) *served {
		tokenFile := filepath.Join(dir, "token")
		if err := os.WriteFile(tokenFile, []byte(token+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		s := &served{t: t, fake: fake, dir: dir, logs: &syncBuffer{}, done: make(chan int, 1)}
		ctx, cancel := context.WithCancel(context.Background())
		s.cancel = cancel
		args := []string{
			"serve",
			"-muster", fake.Server.URL,
			"-plainforge", pf.Server.URL,
			"-plainforge-token-file", pfTokenFile,
			"-owners", ownersFile,
			"-spool", filepath.Join(dir, "spool"),
			"-token-file", tokenFile,
			"-idle", "5ms",
		}
		go func() { s.done <- run(ctx, args, noEnv, &bytes.Buffer{}, s.logs) }()
		return s
	}

	rawProposal, err := os.ReadFile("../../testdata/contract/lease-proposal-results.json")
	if err != nil {
		t.Fatal(err)
	}
	var ex struct {
		Expect struct {
			Body map[string]any `json:"body"`
		} `json:"expect"`
	}
	if err := json.Unmarshal(rawProposal, &ex); err != nil {
		t.Fatal(err)
	}
	ep1 := ex.Expect.Body
	delete(ep1, "lease")
	delete(ep1, "attempt")
	delete(ep1, "expires")

	ep2 := map[string]any{
		"kind":  "proposal",
		"id":    "EP-0002",
		"state": "approved",
		"lane":  "urgent",
		"proposal": map[string]any{
			"target":     "sextant.sample-portal.options",
			"repository": "sextant",
			"file":       "",
			"edit_kind":  "CONFIG",
			"change":     "timeout = 30\n",
			"reason":     "",
		},
	}
	ep3 := map[string]any{
		"kind":  "proposal",
		"id":    "EP-0003",
		"state": "approved",
		"lane":  "urgent",
		"proposal": map[string]any{
			"target":     "bellbuoy.sample-rule.report",
			"repository": "bellbuoy",
			"file":       "tools/internal-git/logic.go",
			"edit_kind":  "ISSUE",
			"change":     "Explain how an approved proposal becomes a branch.",
			"reason":     "Readers look here first.",
		},
	}

	// Pod 1 handles an incident on mirrored sextant, an incident on unmirrored
	// shipkeeper, and all three proposal classes.
	pod1 := startWithPlainforge(t.TempDir())
	fake.Offer(incident("PA-0001", "observed", "sextant", "E_PARSE"))
	fake.Offer(incident("PA-0002", "observed", "shipkeeper", "E_PARSE"))
	fake.Offer(ep1)
	fake.Offer(ep2)
	fake.Offer(ep3)
	pod1.until("five steps to end on pod 1", func() bool { return len(fake.Ended()) == 5 })
	logs1 := pod1.stop()
	if !strings.Contains(logs1, "tools=internal-git,github-mirror ") {
		t.Fatalf("expected github-mirror enabled when -plainforge is set:\n%s", logs1)
	}

	// Rotate Plainforge bearer token and start Pod 2 on a fresh empty spool
	// directory. Also force one 503 E_TIMEOUT on /mirror to prove partial
	// progress retries github-mirror without counting the incident twice.
	pf.SetToken("pf-token-2")
	if err := os.WriteFile(pfTokenFile, []byte("pf-token-2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	pf.ForcePath("/mirror", 1, http.StatusServiceUnavailable, `{"code":"E_TIMEOUT","message":"mirror busy"}`)

	pod2 := startWithPlainforge(t.TempDir())
	fake.Offer(incident("PA-0003", "observed", "sextant", "E_PARSE"))
	pod2.until("repeat incident with mirror retry to end on pod 2", func() bool { return len(fake.Ended()) == 6 })
	logs2 := pod2.stop()

	for _, secret := range []string{pf.Server.URL, "pf-token-1", "pf-token-2", "mirror busy"} {
		if strings.Contains(logs1+logs2, secret) {
			t.Fatalf("logs leaked %q:\n%s\n%s", secret, logs1, logs2)
		}
	}

	changes := pf.Memory.Changes()
	if len(changes) != 2 ||
		changes[0].Repo != "clearedstack.com" || changes[0].Branch != "edit/EP-0001" ||
		changes[0].Path != "src/data/sample-catalog.json" ||
		changes[0].Content != "The sample description should say which made-up rule it follows." ||
		changes[1].Repo != "sextant" || changes[1].Branch != "edit/EP-0002" ||
		changes[1].Path != "tools/sample-portal/options.json" || changes[1].Content != "timeout = 30\n" {
		t.Fatalf("changes = %+v", changes)
	}

	tasks := pf.Memory.Tasks()
	if len(tasks) != 3 ||
		tasks[0].ID != "sextant#1" || tasks[0].Occurrences != 2 ||
		tasks[1].ID != "shipkeeper#1" || tasks[1].Occurrences != 1 ||
		tasks[2].ID != "bellbuoy#1" || tasks[2].Key != "proposal-EP-0003" || tasks[2].Occurrences != 1 {
		t.Fatalf("tasks = %+v", tasks)
	}

	// Mirrored repositories (sextant, clearedstack.com, bellbuoy) received
	// SyncMirror calls; unmirrored shipkeeper did not.
	mirrors := pf.Memory.Mirrors()
	if len(mirrors) != 5 {
		t.Fatalf("mirrors = %+v, want 5 successful calls", mirrors)
	}
	for _, m := range mirrors {
		if m.Repo == "shipkeeper" {
			t.Fatalf("unmirrored repository shipkeeper received SyncMirror: %+v", m)
		}
	}
	if pf.Count("/mirror") != 6 {
		t.Fatalf("/mirror hit count = %d, want 6 (1 forced 503 + 5 successes)", pf.Count("/mirror"))
	}
}

// TestProbeEndpointsAndClusterZeroArgMode checks the /healthz probe listener
// and zero-argument in-cluster startup used when Shipkeeper reconciles app.yaml.
func TestProbeEndpointsAndClusterZeroArgMode(t *testing.T) {
	addr, stopProbe, err := startProbe("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer stopProbe()

	// Shipkeeper TCP readiness check.
	conn, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		t.Fatalf("tcp readiness probe failed: %v", err)
	}
	conn.Close()

	// Kubelet HTTP liveness check and negative method/path checks.
	resp, err := http.Get("http://" + addr + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != "{\"status\":\"ok\"}\n" {
		t.Fatalf("GET /healthz = %d %q", resp.StatusCode, string(body))
	}

	headResp, err := http.Head("http://" + addr + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	headResp.Body.Close()
	if headResp.StatusCode != http.StatusOK {
		t.Fatalf("HEAD /healthz = %d", headResp.StatusCode)
	}

	postResp, err := http.Post("http://"+addr+"/healthz", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	postResp.Body.Close()
	if postResp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("POST /healthz = %d, want 405", postResp.StatusCode)
	}

	notFoundResp, err := http.Get("http://" + addr + "/other")
	if err != nil {
		t.Fatal(err)
	}
	notFoundResp.Body.Close()
	if notFoundResp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET /other = %d, want 404", notFoundResp.StatusCode)
	}
	stopProbe()

	// Zero-argument startup with environment settings and built-in Default() map.
	fake := mustertest.New(token)
	defer fake.Close()
	pf := plainforgetest.New("", "trawl")
	defer pf.Close()

	dir := t.TempDir()
	tokenFile := filepath.Join(dir, "token")
	if err := os.WriteFile(tokenFile, []byte(token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{
		MusterEnv:     fake.Server.URL,
		PlainforgeEnv: pf.Server.URL,
		TokenFileEnv:  tokenFile,
		SpoolEnv:      filepath.Join(dir, "spool"),
		ListenEnv:     "127.0.0.1:0",
	}
	logs := &syncBuffer{}
	done := make(chan int, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		done <- run(ctx, nil, func(k string) string { return env[k] }, &bytes.Buffer{}, logs)
	}()

	fake.Offer(incident("PA-0010", "observed", "trawl", "E_PARSE"))
	deadline := time.Now().Add(15 * time.Second)
	for len(fake.Ended()) == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for zero-arg cluster mode\nlog:\n%s", logs.String())
		}
		time.Sleep(2 * time.Millisecond)
	}
	cancel()
	if code := <-done; code != 0 {
		t.Fatalf("zero-arg cluster mode exited %d:\n%s", code, logs.String())
	}
	if tasks := pf.Memory.Tasks(); len(tasks) != 1 || tasks[0].ID != "trawl#1" {
		t.Fatalf("tasks = %+v", tasks)
	}
}
