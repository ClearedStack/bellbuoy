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

// TestContractExamples replays Muster's own saved examples of the five worker
// calls. For each one it checks two things.
//
// The request: Bellbuoy's client, asked to make the call in the example,
// sends exactly the body in the example, field for field and in the same
// order. Examples of requests no correct client would send (an unknown
// field, a contract version that does not exist) are not sent.
//
// The answer: given the status and body in the example, the client reports
// what the contract means by them.
func TestContractExamples(t *testing.T) {
	files, err := filepath.Glob("../../testdata/contract/*.json")
	if err != nil || len(files) == 0 {
		t.Fatalf("no contract examples: %v", err)
	}
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte("made-up-token"), 0o600); err != nil {
		t.Fatal(err)
	}
	workerCalls := map[string]bool{"/v1/lease": true, "/v1/heartbeat": true, "/v1/complete": true, "/v1/fail": true, "/v1/needs_subtool": true}

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
		if ex.Request.Method != http.MethodPost || !workerCalls[ex.Request.Path] {
			continue // not one of the five worker calls
		}
		t.Run(name, func(t *testing.T) {
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
				var refusal struct {
					Error string `json:"error"`
					Cause string `json:"cause"`
				}
				if err := json.Unmarshal(ex.Expect.Body, &refusal); err != nil {
					t.Fatal(err)
				}
				got := codes.From(callErr)
				if got == nil || string(got.Code) != refusal.Error {
					t.Fatalf("refusal %d %s: client reported %v, want %s", ex.Expect.Status, refusal.Cause, callErr, refusal.Error)
				}
				if errors.Is(callErr, muster.ErrLeaseNotHeld) != (refusal.Cause == "lease_not_held") {
					t.Errorf("cause %s: ErrLeaseNotHeld = %v", refusal.Cause, errors.Is(callErr, muster.ErrLeaseNotHeld))
				}
				if errors.Is(callErr, muster.ErrMoveNotAllowed) != (refusal.Cause == "move_not_allowed") {
					t.Errorf("cause %s: ErrMoveNotAllowed = %v", refusal.Cause, errors.Is(callErr, muster.ErrMoveNotAllowed))
				}
			}
			replayed[name] = true
		})
	}

	// The examples this component depends on must be among them.
	for _, name := range []string{
		"lease.json", "lease-incident.json", "lease-nothing.json", "lease-proposal.json", "heartbeat.json",
		"complete.json", "fail-parse.json", "fail-timeout.json", "needs_subtool.json",
		"refused-409-lease_not_held.json", "refused-409-move_not_allowed.json", "refused-422-unknown_contract.json", "refused-503-busy.json",
	} {
		if !replayed[name] {
			t.Errorf("contract example %s was not replayed", name)
		}
	}
	if len(replayed) < 25 || sent < 12 {
		t.Errorf("replayed %d examples and sent %d of their requests; expected more", len(replayed), sent)
	}
	t.Logf("replayed %d contract examples; sent the exact request of %d", len(replayed), sent)
}

func keysOf(m map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
