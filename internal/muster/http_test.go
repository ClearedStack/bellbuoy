package muster_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ClearedStack/bellbuoy/internal/codes"
	"github.com/ClearedStack/bellbuoy/internal/muster"
	"github.com/ClearedStack/bellbuoy/internal/muster/mustertest"
	"github.com/ClearedStack/bellbuoy/internal/work"
)

var ctx = context.Background()

func codeOf(err error) codes.Code {
	if e := codes.From(err); e != nil {
		return e.Code
	}
	return ""
}

func writeToken(t *testing.T, path, token string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(token), 0o600); err != nil {
		t.Fatal(err)
	}
}

// rig is a client pointed at a fake Muster, with a token file the test can
// rewrite.
func rig(t *testing.T) (*muster.Client, *mustertest.Fake, string) {
	t.Helper()
	fake := mustertest.New("made-up-token-one")
	t.Cleanup(fake.Close)
	tokenFile := filepath.Join(t.TempDir(), "token")
	writeToken(t, tokenFile, "made-up-token-one\n")
	client, err := muster.NewClient(fake.Server.URL, tokenFile, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	return client, fake, tokenFile
}

func incident() map[string]any {
	return map[string]any{
		"kind": "incident", "id": "PA-0042", "state": "observed", "lane": "normal",
		"incident": map[string]any{"id": "PA-0042", "worker": "sextant", "code": "E_PARSE", "state": "observed"},
	}
}

func TestLeaseTurnsTheAnswerIntoAJob(t *testing.T) {
	client, fake, _ := rig(t)
	fake.Offer(incident())
	job, ok, err := client.Lease(ctx)
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	want := work.Job{
		ID: "PA-0042", Lane: "normal", Kind: "incident", Lease: "incident:PA-0042.1", State: "observed", Attempt: 1,
		Payload: json.RawMessage(`{"incident":{"code":"E_PARSE","id":"PA-0042","state":"observed","worker":"sextant"}}`),
	}
	if !reflect.DeepEqual(job, want) {
		t.Fatalf("job = %+v\npayload = %s", job, job.Payload)
	}
	if err := job.Check(); err != nil {
		t.Fatalf("a well-formed lease must pass the envelope check: %v", err)
	}
}

func TestLeaseWithNothingToDo(t *testing.T) {
	client, _, _ := rig(t)
	job, ok, err := client.Lease(ctx)
	if err != nil || ok || job.ID != "" {
		t.Fatalf("204 must mean nothing to do: job=%+v ok=%v err=%v", job, ok, err)
	}
}

// Every body carries the contract version and only the fields the call
// defines. The fake refuses anything else, as the real Muster does.
func TestTheFiveCallsSendExactlyWhatTheContractDefines(t *testing.T) {
	client, fake, _ := rig(t)
	for i := 0; i < 3; i++ {
		fake.Offer(map[string]any{"kind": "item", "id": []string{"PA-0001", "PA-0002", "PA-0003"}[i], "state": "observed", "lane": "normal"})
	}
	a, _, _ := client.Lease(ctx)
	b, _, _ := client.Lease(ctx)
	c, _, _ := client.Lease(ctx)
	steps := []error{
		client.Heartbeat(ctx, a.Lease),
		client.Complete(ctx, a.Lease, "observed", json.RawMessage(`{"task":{"id":"sextant#1"}}`)),
		client.Fail(ctx, b.Lease, codes.Parse),
		client.NeedsSubtool(ctx, c.Lease),
	}
	for i, err := range steps {
		if err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	want := []mustertest.Call{
		{Path: "/v1/lease", Token: "made-up-token-one", Body: `{"contract":"v1"}`},
		{Path: "/v1/lease", Token: "made-up-token-one", Body: `{"contract":"v1"}`},
		{Path: "/v1/lease", Token: "made-up-token-one", Body: `{"contract":"v1"}`},
		{Path: "/v1/heartbeat", Token: "made-up-token-one", Body: `{"contract":"v1","lease":"PA-0001.1"}`},
		{Path: "/v1/complete", Token: "made-up-token-one", Body: `{"contract":"v1","lease":"PA-0001.1","state":"observed","result":{"task":{"id":"sextant#1"}}}`},
		{Path: "/v1/fail", Token: "made-up-token-one", Body: `{"contract":"v1","lease":"PA-0002.1","code":"E_PARSE"}`},
		{Path: "/v1/needs_subtool", Token: "made-up-token-one", Body: `{"contract":"v1","lease":"PA-0003.1"}`},
	}
	if got := fake.Calls(); !reflect.DeepEqual(got, want) {
		t.Fatalf("calls:\n got %+v\nwant %+v", got, want)
	}
	ended := fake.Ended()
	if len(ended) != 3 || ended[0].Call != "complete" || ended[0].Result != `{"task":{"id":"sextant#1"}}` ||
		ended[1].Call != "fail" || ended[1].Code != "E_PARSE" || ended[2].Call != "needs_subtool" {
		t.Fatalf("ended = %+v", ended)
	}
}

func TestCompleteWithoutAResultLeavesTheFieldOut(t *testing.T) {
	client, fake, _ := rig(t)
	fake.Offer(incident())
	job, _, _ := client.Lease(ctx)
	if err := client.Complete(ctx, job.Lease, "observed", nil); err != nil {
		t.Fatal(err)
	}
	calls := fake.Calls()
	if got := calls[len(calls)-1].Body; got != `{"contract":"v1","lease":"incident:PA-0042.1","state":"observed"}` {
		t.Fatalf("body = %s", got)
	}
}

// The cluster replaces the token while the pod runs, so it is read from its
// file for every call.
func TestTheTokenIsReadFreshForEveryCall(t *testing.T) {
	client, fake, tokenFile := rig(t)
	client.Lease(ctx)
	fake.Accept("made-up-token-two")
	writeToken(t, tokenFile, "  made-up-token-two  \n")
	if _, _, err := client.Lease(ctx); err != nil {
		t.Fatalf("the replaced token was not picked up: %v", err)
	}
	calls := fake.Calls()
	if calls[0].Token != "made-up-token-one" || calls[1].Token != "made-up-token-two" {
		t.Fatalf("tokens sent: %q then %q", calls[0].Token, calls[1].Token)
	}
}

func TestATokenThatCannotBeUsedSendsNothing(t *testing.T) {
	client, fake, tokenFile := rig(t)
	for name, content := range map[string]string{
		"empty":        "",
		"only space":   " \n\t",
		"two lines":    "made-up-token-one\nInjected: header",
		"control char": "made-up\x00token",
		"too large":    strings.Repeat("a", 17<<10),
	} {
		writeToken(t, tokenFile, content)
		if _, _, err := client.Lease(ctx); codeOf(err) != codes.Parse {
			t.Errorf("%s: err = %v, want E_PARSE", name, err)
		}
	}
	os.Remove(tokenFile)
	_, _, err := client.Lease(ctx)
	if codeOf(err) != codes.Parse || strings.Contains(err.Error(), filepath.Dir(tokenFile)) {
		t.Errorf("missing file: err = %v", err)
	}
	if n := len(fake.Calls()); n != 0 {
		t.Fatalf("%d calls were made without a usable token", n)
	}
}

// What Muster's refusal means for the worker, by the code in its body, and
// by the status when the body is not a refusal.
func TestRefusals(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   codes.Code
	}{
		{400, `{"error":"E_PARSE","cause":"body","reason":"the body is not what this call takes"}`, codes.Parse},
		{401, `{"error":"E_PARSE","cause":"credential","reason":"no valid credential"}`, codes.Parse},
		{403, `{"error":"E_PARSE","cause":"caller","reason":"that call is not yours"}`, codes.Parse},
		{409, `{"error":"E_PARSE","cause":"lease_not_held","reason":"the lease is not held"}`, codes.Parse},
		{409, `{"error":"E_PARSE","cause":"move_not_allowed","reason":"that move is not allowed"}`, codes.Parse},
		{422, `{"error":"E_NEEDS_SUBTOOL","cause":"contract","reason":"that contract version is not known"}`, codes.NeedsSubtool},
		{429, `{"error":"E_TIMEOUT","cause":"rate","reason":"too many calls at once"}`, codes.Timeout},
		{503, `{"error":"E_TIMEOUT","cause":"store","reason":"the store took too long"}`, codes.Timeout},
		{500, `{"error":"E_PARSE","cause":"unexpected","reason":"something unexpected happened"}`, codes.Parse},
		// The body is not a refusal, so the status decides.
		{502, `<html>bad gateway</html>`, codes.Timeout},
		{503, ``, codes.Timeout},
		{408, `{}`, codes.Timeout},
		{429, `{"error":"E_SOMETHING_ELSE"}`, codes.Timeout},
		{404, `not found`, codes.Parse},
		{418, `{"error":7}`, codes.Parse},
		{301, ``, codes.Parse},
	}
	client, fake, _ := rig(t)
	for _, c := range cases {
		fake.Force(1, c.status, c.body)
		_, _, err := client.Lease(ctx)
		if got := codeOf(err); got != c.want {
			t.Errorf("status %d body %q: got %q, want %q", c.status, c.body, got, c.want)
		}
		// Nothing Muster wrote may be repeated in the error.
		for _, leak := range []string{"credential", "store took", "gateway", "not yours", "too many calls"} {
			if err != nil && strings.Contains(err.Error(), leak) {
				t.Errorf("status %d: error repeats Muster's words: %s", c.status, err)
			}
		}
	}
}

// Two causes are told apart, because the worker does different things about
// them.
func TestLeaseNotHeldAndMoveNotAllowedAreToldApart(t *testing.T) {
	client, fake, _ := rig(t)
	fake.Offer(incident())
	job, _, _ := client.Lease(ctx)
	if err := client.NeedsSubtool(ctx, job.Lease); !errors.Is(err, muster.ErrMoveNotAllowed) {
		t.Fatalf("pausing an incident: err = %v, want ErrMoveNotAllowed", err)
	}
	if err := client.Complete(ctx, job.Lease, "scored", nil); !errors.Is(err, muster.ErrMoveNotAllowed) {
		t.Fatalf("completing an incident to another state: err = %v, want ErrMoveNotAllowed", err)
	}
	if err := client.Complete(ctx, job.Lease, "observed", json.RawMessage(`{"task":"x"}`)); codeOf(err) != codes.Parse || errors.Is(err, muster.ErrMoveNotAllowed) {
		t.Fatalf("completing an incident with a result: err = %v, want a plain E_PARSE", err)
	}
	if err := client.Heartbeat(ctx, "incident:PA-0042.9"); !errors.Is(err, muster.ErrLeaseNotHeld) {
		t.Fatalf("a lease never given: err = %v, want ErrLeaseNotHeld", err)
	}
	if len(fake.Ended()) != 0 {
		t.Fatal("a refused call ended the step")
	}
	if err := client.Complete(ctx, job.Lease, "observed", nil); err != nil {
		t.Fatalf("completing an incident as the contract says: %v", err)
	}
}

func TestAStatusOtherThan200IsNotAnAcknowledgement(t *testing.T) {
	client, fake, _ := rig(t)
	fake.Offer(incident())
	job, _, _ := client.Lease(ctx)
	for _, call := range []func() error{
		func() error { return client.Heartbeat(ctx, job.Lease) },
		func() error { return client.Complete(ctx, job.Lease, "observed", nil) },
		func() error { return client.Fail(ctx, job.Lease, codes.Parse) },
		func() error { return client.NeedsSubtool(ctx, job.Lease) },
	} {
		fake.Force(1, 204, "")
		if got := codeOf(call()); got != codes.Parse {
			t.Errorf("a 204 was taken as an acknowledgement (got %q)", got)
		}
	}
	if len(fake.Ended()) != 0 {
		t.Fatal("the step ended on the fake although every call was answered 204")
	}
}

func TestMalformedLeaseAnswers(t *testing.T) {
	client, fake, _ := rig(t)
	// Without a usable lease token nothing can be told to Muster: an error.
	for _, body := range []string{
		``, `not json`, `[]`, `null`, `"text"`, `{}`, `{"kind":"incident","id":"PA-0001"}`,
		`{"lease":7}`, `{"lease":""}`, `{"lease":"has space"}`, `{"lease":"PA-0001.1"} trailing`,
	} {
		fake.Force(1, 200, body)
		if _, ok, err := client.Lease(ctx); ok || codeOf(err) != codes.Parse {
			t.Errorf("body %q: ok=%v err=%v, want E_PARSE", body, ok, err)
		}
	}
	// With a lease token, a malformed field makes a job that fails the
	// envelope check, so the worker can tell Muster it is broken.
	for _, body := range []string{
		`{"lease":"PA-0001.1","kind":7,"id":"PA-0001","state":"observed","lane":"normal","attempt":1}`,
		`{"lease":"PA-0001.1","kind":"incident","id":["PA-0001"],"state":"observed","lane":"normal","attempt":1}`,
		`{"lease":"PA-0001.1","kind":"incident","id":"PA-0001","state":"observed","lane":"soon","attempt":1}`,
		`{"lease":"PA-0001.1","kind":"incident","id":"PA-0001","state":"Not A State","lane":"normal","attempt":1}`,
		`{"lease":"PA-0001.1","kind":"incident","id":"PA-0001","state":"observed","lane":"normal","attempt":"one"}`,
	} {
		fake.Force(1, 200, body)
		job, ok, err := client.Lease(ctx)
		if !ok || err != nil || job.Lease != "PA-0001.1" {
			t.Errorf("body %q: ok=%v err=%v", body, ok, err)
			continue
		}
		if codeOf(job.Check()) != codes.Parse {
			t.Errorf("body %q: the job passed the envelope check", body)
		}
	}
}

func TestAnswerSizeIsBounded(t *testing.T) {
	client, fake, _ := rig(t)
	fake.Force(1, 200, `{"lease":"PA-0001.1","results":"`+strings.Repeat("x", 1<<20)+`"}`)
	if _, _, err := client.Lease(ctx); codeOf(err) != codes.Parse {
		t.Fatalf("err = %v, want E_PARSE", err)
	}
}

func TestOnlyTimeoutAndParseCanFailAStep(t *testing.T) {
	client, fake, _ := rig(t)
	if got := codeOf(client.Fail(ctx, "PA-0001.1", codes.NeedsSubtool)); got != codes.Parse {
		t.Fatalf("got %q", got)
	}
	if got := codeOf(client.Complete(ctx, "PA-0001.1", "observed", json.RawMessage(`["not","an","object"]`))); got != codes.Parse {
		t.Fatalf("got %q", got)
	}
	if n := len(fake.Calls()); n != 0 {
		t.Fatalf("%d calls were sent that the contract does not allow", n)
	}
}

func TestRedirectsAreNotFollowed(t *testing.T) {
	elsewhere := 0
	other := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { elsewhere++ }))
	defer other.Close()
	redirecting := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/v1/lease", http.StatusTemporaryRedirect)
	}))
	defer redirecting.Close()
	tokenFile := filepath.Join(t.TempDir(), "token")
	writeToken(t, tokenFile, "made-up-token-one")
	client, err := muster.NewClient(redirecting.URL, tokenFile, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := client.Lease(ctx); codeOf(err) != codes.Parse {
		t.Fatalf("err = %v, want E_PARSE", err)
	}
	if elsewhere != 0 {
		t.Fatal("the token was sent on to an address nobody configured")
	}
}

func TestMusterDownOrSlowIsATimeoutAndNamesNoAddress(t *testing.T) {
	tokenFile := filepath.Join(t.TempDir(), "token")
	writeToken(t, tokenFile, "made-up-token-one")

	down := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	address := down.URL
	down.Close()
	client, _ := muster.NewClient(address, tokenFile, time.Second)
	_, _, err := client.Lease(ctx)
	if codeOf(err) != codes.Timeout {
		t.Fatalf("down: err = %v", err)
	}
	if strings.Contains(err.Error(), "127.0.0.1") || strings.Contains(err.Error(), "http") || strings.Contains(err.Error(), "made-up") {
		t.Fatalf("error names the address or the token: %s", err)
	}

	release := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-release }))
	defer slow.Close()
	defer close(release)
	client, _ = muster.NewClient(slow.URL, tokenFile, 50*time.Millisecond)
	if err := client.Heartbeat(ctx, "PA-0001.1"); codeOf(err) != codes.Timeout {
		t.Fatalf("slow: err = %v", err)
	}
}

func TestNewClientRefusesBadAddresses(t *testing.T) {
	for _, address := range []string{
		"", "queue.example.invalid", "ftp://muster.example.invalid", "http://", "http://user:made-up@muster.example.invalid",
		"http://muster.example.invalid/v1", "http://muster.example.invalid/?x=1", "http://muster.example.invalid/#x", "http://%zz",
	} {
		_, err := muster.NewClient(address, "", 0)
		if codeOf(err) != codes.Parse {
			t.Errorf("NewClient(%q) = %v, want E_PARSE", address, err)
		} else if address != "" && strings.Contains(err.Error(), address) {
			t.Errorf("error repeats the address %q", address)
		}
	}
	for _, address := range []string{"http://muster.example.invalid", "http://muster.example.invalid:8080/", "https://muster.example.invalid"} {
		if _, err := muster.NewClient(address, "", 0); err != nil {
			t.Errorf("NewClient(%q) = %v", address, err)
		}
	}
}

func TestRepeatingTheCallThatEndedALeaseIsHarmless(t *testing.T) {
	client, fake, _ := rig(t)
	fake.Offer(incident())
	job, _, _ := client.Lease(ctx)
	for i := 0; i < 3; i++ {
		if err := client.Complete(ctx, job.Lease, "observed", nil); err != nil {
			t.Fatalf("repeat %d: %v", i, err)
		}
	}
	if n := len(fake.Ended()); n != 1 {
		t.Fatalf("the step ended %d times", n)
	}
	if got := codeOf(client.Fail(ctx, job.Lease, codes.Parse)); got != codes.Parse {
		t.Fatalf("a different call on an ended lease must be refused, got %q", got)
	}
}
