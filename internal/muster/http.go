package muster

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/ClearedStack/bellbuoy/internal/codes"
	"github.com/ClearedStack/bellbuoy/internal/sanitize"
	"github.com/ClearedStack/bellbuoy/internal/work"
)

const (
	// DefaultTokenFile is where a pod finds its ServiceAccount token.
	DefaultTokenFile = "/var/run/secrets/kubernetes.io/serviceaccount/token"
	// DefaultTimeout is how long Muster has to answer one call.
	DefaultTimeout = 10 * time.Second

	maxToken  = 16 << 10
	maxAnswer = 1 << 20
)

// Client makes the five worker calls of Muster's contract v1 over HTTP.
//
// Every call is a POST with a JSON body that carries "contract": "v1" and
// only the fields the contract defines for that call. The ServiceAccount
// token is read from its file for every call, because the cluster replaces
// it while the pod runs. Redirects are not followed and no proxy is used:
// the token goes to the configured address and nowhere else.
//
// The address, the token and anything Muster writes in a refusal never
// appear in an error.
type Client struct {
	base      string
	tokenFile string
	http      *http.Client
}

var _ Queue = (*Client)(nil)

// NewClient returns a client for the Muster at address, which must be an
// http or https address with no path. An empty tokenFile means
// DefaultTokenFile, and a zero timeout means DefaultTimeout.
func NewClient(address, tokenFile string, timeout time.Duration) (*Client, error) {
	u, err := url.Parse(address)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" ||
		u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return nil, codes.Broken("muster address is not usable")
	}
	if tokenFile == "" {
		tokenFile = DefaultTokenFile
	}
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	return &Client{
		base:      u.Scheme + "://" + u.Host,
		tokenFile: tokenFile,
		http: &http.Client{
			Timeout: timeout,
			Transport: &http.Transport{
				Proxy:               nil,
				MaxIdleConns:        2,
				MaxIdleConnsPerHost: 2,
				IdleConnTimeout:     90 * time.Second,
			},
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}, nil
}

// token reads the ServiceAccount token as it is now.
func (c *Client) token() (string, error) {
	f, err := os.Open(c.tokenFile)
	if err != nil {
		return "", codes.Broken("service account token cannot be read")
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxToken+1))
	if err != nil || len(data) > maxToken {
		return "", codes.Broken("service account token cannot be read")
	}
	token := strings.TrimSpace(string(data))
	if token == "" {
		return "", codes.Broken("service account token cannot be read")
	}
	for i := 0; i < len(token); i++ {
		if token[i] <= ' ' || token[i] > '~' {
			return "", codes.Broken("service account token cannot be read")
		}
	}
	return token, nil
}

// refusal is the body Muster sends with any status other than 200 and 204.
// The code decides what happens next, and two causes are told apart. The
// reason is a sentence for a person and is dropped.
type refusal struct {
	Error  string `json:"error"`
	Cause  string `json:"cause"`
	Reason string `json:"reason"`
}

// post makes one call and returns the status and body of a 200 or 204
// answer. Anything else comes back as an error with a public code.
func (c *Client) post(ctx context.Context, path string, body any) (int, []byte, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return 0, nil, codes.Broken("the call to muster could not be written out")
	}
	token, err := c.token()
	if err != nil {
		return 0, nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, bytes.NewReader(payload))
	if err != nil {
		return 0, nil, codes.Broken("muster address is not usable")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("User-Agent", "bellbuoy")
	resp, err := c.http.Do(req)
	if err != nil {
		// The text of this error holds the address. Drop it.
		return 0, nil, codes.TimedOut("muster did not answer in time")
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxAnswer+1))
	if err != nil {
		return 0, nil, codes.TimedOut("muster did not answer in time")
	}
	if len(data) > maxAnswer {
		return 0, nil, codes.Broken("muster's answer was too large")
	}
	switch resp.StatusCode {
	case http.StatusOK, http.StatusNoContent:
		return resp.StatusCode, data, nil
	}

	// A refusal. The code Muster gives decides what happens next. If the
	// body is not a refusal at all, the status has to do.
	var r refusal
	if json.Unmarshal(data, &r) == nil {
		switch code, _ := codes.Lookup(r.Error); code {
		case codes.Timeout:
			return 0, nil, codes.TimedOut("muster asked to be tried again")
		case codes.Parse:
			switch r.Cause {
			case "lease_not_held":
				return 0, nil, ErrLeaseNotHeld
			case "move_not_allowed":
				return 0, nil, ErrMoveNotAllowed
			}
			return 0, nil, codes.Broken("muster refused the call")
		case codes.NeedsSubtool:
			return 0, nil, codes.Unfamiliar("muster does not know this contract version")
		}
	}
	switch {
	case resp.StatusCode == http.StatusRequestTimeout,
		resp.StatusCode == http.StatusTooManyRequests,
		resp.StatusCode >= 500:
		return 0, nil, codes.TimedOut("muster asked to be tried again")
	default:
		return 0, nil, codes.Broken("muster refused the call")
	}
}

// done makes a call whose only good answer is 200.
func (c *Client) done(ctx context.Context, path string, body any) error {
	status, _, err := c.post(ctx, path, body)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return codes.Broken("muster gave an unexpected answer")
	}
	return nil
}

type leaseRequest struct {
	Contract string `json:"contract"`
}

type heldRequest struct {
	Contract string `json:"contract"`
	Lease    string `json:"lease"`
}

type completeRequest struct {
	Contract string          `json:"contract"`
	Lease    string          `json:"lease"`
	State    string          `json:"state"`
	Result   json.RawMessage `json:"result,omitempty"`
}

type failRequest struct {
	Contract string `json:"contract"`
	Lease    string `json:"lease"`
	Code     string `json:"code"`
}

// Lease asks for the next step. A 204 means there is none.
//
// The answer's lease, kind, id, state, lane and attempt become the job's own
// fields, and expires is dropped: Bellbuoy keeps a lease alive by heartbeat
// and does not read the clock in it. Every other field of the answer goes
// into the job's payload untouched, for the sub-tool that knows the kind.
//
// An answer without a usable lease token is an error, because nothing could
// be told to Muster about it. Any other malformed field is left empty in the
// job, which the worker then refuses with E_PARSE.
func (c *Client) Lease(ctx context.Context) (work.Job, bool, error) {
	status, data, err := c.post(ctx, "/v1/lease", leaseRequest{Contract: Contract})
	if err != nil {
		return work.Job{}, false, err
	}
	if status == http.StatusNoContent {
		return work.Job{}, false, nil
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	var fields map[string]json.RawMessage
	if err := dec.Decode(&fields); err != nil || fields == nil {
		return work.Job{}, false, codes.Broken("muster's lease answer was not understood")
	}
	if _, err := dec.Token(); err != io.EOF {
		return work.Job{}, false, codes.Broken("muster's lease answer was not understood")
	}
	text := func(name string) string {
		var s string
		if json.Unmarshal(fields[name], &s) != nil {
			s = ""
		}
		delete(fields, name)
		return s
	}
	job := work.Job{
		Lease: text("lease"),
		Kind:  text("kind"),
		ID:    text("id"),
		State: text("state"),
		Lane:  text("lane"),
	}
	if !sanitize.IsLease(job.Lease) {
		return work.Job{}, false, codes.Broken("muster's lease answer was not understood")
	}
	if raw, ok := fields["attempt"]; ok {
		if json.Unmarshal(raw, &job.Attempt) != nil {
			job.Attempt = -1
		}
		delete(fields, "attempt")
	}
	delete(fields, "expires")
	payload, err := json.Marshal(fields)
	if err != nil {
		return work.Job{}, false, codes.Broken("muster's lease answer was not understood")
	}
	job.Payload = payload
	return job, true, nil
}

// Heartbeat keeps a lease alive.
func (c *Client) Heartbeat(ctx context.Context, lease string) error {
	return c.done(ctx, "/v1/heartbeat", heldRequest{Contract: Contract, Lease: lease})
}

// Complete reports a step as done. result must be a JSON object, or empty
// to send none.
func (c *Client) Complete(ctx context.Context, lease, state string, result json.RawMessage) error {
	if len(result) > 0 {
		var object map[string]json.RawMessage
		if json.Unmarshal(result, &object) != nil || object == nil {
			return codes.Broken("a result must be a JSON object")
		}
	}
	return c.done(ctx, "/v1/complete", completeRequest{Contract: Contract, Lease: lease, State: state, Result: result})
}

// Fail reports a step as failed. The contract takes E_TIMEOUT and E_PARSE
// here and nothing else.
func (c *Client) Fail(ctx context.Context, lease string, code codes.Code) error {
	if code != codes.Timeout && code != codes.Parse {
		return codes.Broken("a step can be failed only with E_TIMEOUT or E_PARSE")
	}
	return c.done(ctx, "/v1/fail", failRequest{Contract: Contract, Lease: lease, Code: string(code)})
}

// NeedsSubtool pauses a step until a rule exists for it.
func (c *Client) NeedsSubtool(ctx context.Context, lease string) error {
	return c.done(ctx, "/v1/needs_subtool", heldRequest{Contract: Contract, Lease: lease})
}
