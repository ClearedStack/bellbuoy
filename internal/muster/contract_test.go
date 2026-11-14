package muster_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/ClearedStack/bellbuoy/internal/codes"
	"github.com/ClearedStack/bellbuoy/internal/muster"
	"github.com/ClearedStack/bellbuoy/internal/work"
)

// example is one of Muster's saved examples of contract v1.
type example struct {
	About   string `json:"about"`
	Request struct {
		As     string          `json:"as"`
		Method string          `json:"method"`
		Path   string          `json:"path"`
		Body   json.RawMessage `json:"body"`
	} `json:"request"`
	Expect struct {
		Status int             `json:"status"`
		Body   json.RawMessage `json:"body"`
		Stream string          `json:"stream"`
	} `json:"expect"`
}

func compact(t *testing.T, raw []byte) string {
	t.Helper()
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		t.Fatalf("not JSON: %v", err)
	}
	return buf.String()
}

// TestContractExamples replays every saved example in Muster's
// contract/v1/examples/.
//
// For the five worker calls Bellbuoy makes (/v1/lease, /v1/heartbeat,
// /v1/complete, /v1/fail, /v1/needs_subtool), it checks both that Bellbuoy's
// client sends the exact request body in the example (when the example shows a
// valid worker request) and that the client interprets Muster's answer as
// CONTRACT.md specifies.
//
// For refusal examples on other paths or methods, it feeds Muster's exact HTTP
// status and refusal body to Bellbuoy's client and verifies the resulting
// public code, cause mapping, and stripping of Muster's human reason text.
//
// For positive examples of calls reserved to other callers (/v1/items,
// /v1/edits, /v1/health, /v1/release, /v1/feed, /healthz), it checks that none
// is addressed as bellbuoy and that Bellbuoy's client exposes only the five
// worker methods allowed to bellbuoy in CONTRACT.md.
func TestContractExamples(t *testing.T) {
	files, err := filepath.Glob("../../testdata/contract/*.json")
	if err != nil || len(files) == 0 {
		t.Fatalf("no contract examples: %v", err)
	}
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte("made-up-token"), 0o600); err != nil {
		t.Fatal(err)
	}
	workerCalls := map[string]bool{
		"/v1/lease":         true,
		"/v1/heartbeat":     true,
		"/v1/complete":      true,
		"/v1/fail":          true,
		"/v1/needs_subtool": true,
	}
	clientMethods := exportedMethods(reflect.TypeOf(&muster.Client{}))
	wantMethods := []string{"Complete", "Fail", "Heartbeat", "Lease", "NeedsSubtool"}
	if !reflect.DeepEqual(clientMethods, wantMethods) {
		t.Fatalf("muster.Client methods = %v, want only %v", clientMethods, wantMethods)
	}

	replayed := map[string]bool{}
	sent := 0
	for _, file := range files {
		name := filepath.Base(file)
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		var ex example
		if err := json.Unmarshal(raw, &ex); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		t.Run(name, func(t *testing.T) {
			if ex.About == "" || ex.Request.Method == "" || ex.Request.Path == "" {
				t.Fatalf("incomplete example metadata: %+v", ex)
			}

			// Positive examples on calls outside the five worker endpoints
			// belong to other callers in CONTRACT.md ("Who may call what").
			if (ex.Request.Method != http.MethodPost || !workerCalls[ex.Request.Path]) && ex.Expect.Status < 400 {
				if ex.Request.As == "bellbuoy" {
					t.Fatalf("contract example %s has bellbuoy calling %s %s", name, ex.Request.Method, ex.Request.Path)
				}
				switch ex.Expect.Status {
				case http.StatusOK, http.StatusCreated, http.StatusAccepted, http.StatusNoContent:
				default:
					t.Fatalf("unexpected positive status %d", ex.Expect.Status)
				}
				if len(ex.Expect.Body) > 0 {
					_ = compact(t, ex.Expect.Body)
				} else if ex.Expect.Status != http.StatusNoContent && ex.Expect.Stream == "" {
					t.Fatalf("expected body or stream for status %d", ex.Expect.Status)
				}
				replayed[name] = true
				return
			}

			var gotPath, gotBody, gotAuth, gotType string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				b, _ := io.ReadAll(r.Body)
				gotPath, gotBody, gotAuth, gotType = r.URL.Path, string(b), r.Header.Get("Authorization"), r.Header.Get("Content-Type")
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(ex.Expect.Status)
				if len(ex.Expect.Body) > 0 {
					w.Write(ex.Expect.Body)
				}
			}))
			defer srv.Close()
			client, err := muster.NewClient(srv.URL, tokenFile, time.Second)
			if err != nil {
				t.Fatal(err)
			}

			// Refusal examples on non-worker paths or methods still use the
			// same refusal envelope; replay the response through Client.Lease.
			if ex.Request.Method != http.MethodPost || !workerCalls[ex.Request.Path] {
				_, leased, callErr := client.Lease(ctx)
				if leased || callErr == nil {
					t.Fatalf("refusal %d must fail: leased=%v err=%v", ex.Expect.Status, leased, callErr)
				}
				checkRefusal(t, ex.Expect.Status, ex.Expect.Body, callErr)
				replayed[name] = true
				return
			}

			// What the example's request asks for.
			var fields map[string]json.RawMessage
			var args struct {
				Contract string          `json:"contract"`
				Lease    string          `json:"lease"`
				State    string          `json:"state"`
				Result   json.RawMessage `json:"result"`
				Code     string          `json:"code"`
			}
			sendable := json.Unmarshal(ex.Request.Body, &fields) == nil && json.Unmarshal(ex.Request.Body, &args) == nil && args.Contract == muster.Contract
			want := map[string][]string{
				"/v1/lease":         {"contract"},
				"/v1/heartbeat":     {"contract", "lease"},
				"/v1/complete":      {"contract", "lease", "state", "result"},
				"/v1/fail":          {"contract", "lease", "code"},
				"/v1/needs_subtool": {"contract", "lease"},
			}[ex.Request.Path]
			for field := range fields {
				known := false
				for _, w := range want {
					known = known || w == field
				}
				sendable = sendable && known
			}
			if ex.Request.Path != "/v1/lease" && args.Lease == "" {
				sendable = false
			}
			if ex.Request.Path == "/v1/complete" && args.State == "" {
				sendable = false
			}
			if ex.Request.Path == "/v1/fail" && args.Code != "E_TIMEOUT" && args.Code != "E_PARSE" {
				sendable = false
			}
			if len(args.Result) > 0 {
				var object map[string]json.RawMessage
				if json.Unmarshal(args.Result, &object) != nil || object == nil {
					sendable = false // a result that is not a JSON object
				}
			}
			if !sendable {
				// A request no correct client sends. Use stand-in values so
				// the answer can still be read.
				args.Lease, args.State, args.Code, args.Result = "PA-0001.1", "recorded", "E_PARSE", nil
			}

			var job work.Job
			var leased bool
			var callErr error
			switch ex.Request.Path {
			case "/v1/lease":
				job, leased, callErr = client.Lease(ctx)
			case "/v1/heartbeat":
				callErr = client.Heartbeat(ctx, args.Lease)
			case "/v1/complete":
				callErr = client.Complete(ctx, args.Lease, args.State, args.Result)
			case "/v1/fail":
				callErr = client.Fail(ctx, args.Lease, codes.Code(args.Code))
			case "/v1/needs_subtool":
				callErr = client.NeedsSubtool(ctx, args.Lease)
			}

			// The request.
			if gotPath != ex.Request.Path || gotAuth != "Bearer made-up-token" || gotType != "application/json" {
				t.Fatalf("sent path %q, authorization %q, content type %q", gotPath, gotAuth, gotType)
			}
			if sendable {
				sent++
				if want := compact(t, ex.Request.Body); gotBody != want {
					t.Errorf("request body differs from the contract example\n sent %s\n want %s", gotBody, want)
				}
			}

			// The answer.
			switch {
			case ex.Expect.Status == http.StatusNoContent:
				if ex.Request.Path != "/v1/lease" {
					t.Fatalf("a 204 for %s is not in the contract", ex.Request.Path)
				}
				if leased || callErr != nil {
					t.Errorf("204 must mean nothing to lease: leased=%v err=%v", leased, callErr)
				}
			case ex.Expect.Status == http.StatusOK && ex.Request.Path == "/v1/lease":
				if !leased || callErr != nil {
					t.Fatalf("leased=%v err=%v", leased, callErr)
				}
				var body map[string]json.RawMessage
				if err := json.Unmarshal(ex.Expect.Body, &body); err != nil {
					t.Fatal(err)
				}
				text := func(name string) string {
					var s string
					json.Unmarshal(body[name], &s)
					delete(body, name)
					return s
				}
				var attempt int
				json.Unmarshal(body["attempt"], &attempt)
				delete(body, "attempt")
				delete(body, "expires")
				wantJob := work.Job{Lease: text("lease"), Kind: text("kind"), ID: text("id"), State: text("state"), Lane: text("lane"), Attempt: attempt}
				gotJob := job
				gotJob.Payload = nil
				if !reflect.DeepEqual(gotJob, wantJob) {
					t.Errorf("job = %+v\nwant %+v", gotJob, wantJob)
				}
				// Everything else in the answer is the payload, untouched.
				var payload map[string]json.RawMessage
				if err := json.Unmarshal(job.Payload, &payload); err != nil {
					t.Fatal(err)
				}
				if len(payload) != len(body) {
					t.Errorf("payload has fields %v, the answer has %v besides the envelope", keysOf(payload), keysOf(body))
				}
				for name, want := range body {
					if compact(t, payload[name]) != compact(t, want) {
						t.Errorf("payload field %q = %s, want %s", name, payload[name], want)
					}
				}
				if err := job.Check(); err != nil {
					t.Errorf("a lease from the contract fails the envelope check: %v", err)
				}
			case ex.Expect.Status == http.StatusOK:
				if callErr != nil {
					t.Errorf("a 200 must be an acknowledgement: %v", callErr)
				}
			default:
				checkRefusal(t, ex.Expect.Status, ex.Expect.Body, callErr)
			}
			replayed[name] = true
		})
	}

	if len(replayed) != len(files) || len(files) < 52 {
		t.Fatalf("replayed %d of %d contract examples; every saved example must run", len(replayed), len(files))
	}
	for _, name := range []string{
		"lease.json", "lease-incident.json", "lease-nothing.json", "lease-proposal.json", "lease-proposal-results.json",
		"heartbeat.json", "complete.json", "complete-proposal.json",
		"fail-parse.json", "fail-timeout.json", "needs_subtool.json", "release.json",
		"refused-400-bad_field-logbook.json", "refused-409-lease_not_held.json", "refused-409-move_not_allowed.json",
		"refused-422-unknown_contract.json", "refused-422-unknown_value-logbook.json", "refused-503-busy.json",
	} {
		if !replayed[name] {
			t.Errorf("contract example %s was not replayed", name)
		}
	}
	if sent < 14 {
		t.Errorf("sent %d exact requests; expected at least 14", sent)
	}
	t.Logf("replayed all %d contract examples; sent the exact request of %d", len(replayed), sent)
}

func checkRefusal(t *testing.T, status int, raw json.RawMessage, callErr error) {
	t.Helper()
	var refusal struct {
		Error  string `json:"error"`
		Cause  string `json:"cause"`
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(raw, &refusal); err != nil {
		t.Fatal(err)
	}
	got := codes.From(callErr)
	if got == nil || string(got.Code) != refusal.Error {
		t.Fatalf("refusal %d %s: client reported %v, want %s", status, refusal.Cause, callErr, refusal.Error)
	}
	if errors.Is(callErr, muster.ErrLeaseNotHeld) != (refusal.Cause == "lease_not_held") {
		t.Errorf("cause %s: ErrLeaseNotHeld = %v", refusal.Cause, errors.Is(callErr, muster.ErrLeaseNotHeld))
	}
	if errors.Is(callErr, muster.ErrMoveNotAllowed) != (refusal.Cause == "move_not_allowed") {
		t.Errorf("cause %s: ErrMoveNotAllowed = %v", refusal.Cause, errors.Is(callErr, muster.ErrMoveNotAllowed))
	}
	fixedReasons := map[string]bool{
		"muster asked to be tried again":             true,
		codes.From(muster.ErrLeaseNotHeld).Reason:    true,
		codes.From(muster.ErrMoveNotAllowed).Reason:  true,
		"muster refused the call":                    true,
		"muster does not know this contract version": true,
	}
	if !fixedReasons[got.Reason] {
		t.Errorf("client reported unexpected non-fixed reason %q", got.Reason)
	}
}

func exportedMethods(t reflect.Type) []string {
	out := make([]string, 0, t.NumMethod())
	for i := 0; i < t.NumMethod(); i++ {
		out = append(out, t.Method(i).Name)
	}
	sort.Strings(out)
	return out
}

func keysOf(m map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
