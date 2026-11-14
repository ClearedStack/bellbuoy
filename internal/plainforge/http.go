package plainforge

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
	"unicode/utf8"

	"github.com/ClearedStack/bellbuoy/internal/codes"
	"github.com/ClearedStack/bellbuoy/internal/sanitize"
)

const (
	// DefaultTimeout is how long Plainforge has to answer one call.
	DefaultTimeout = 10 * time.Second

	maxToken  = 16 << 10
	maxAnswer = 1 << 20
)

// Client makes the five calls of Plainforge's Forge HTTP Contract v1
// (/v1/repos/{repo}/...).
//
// When tokenFile is set, the bearer token is read from its file for every
// call because the cluster rotates projected tokens while the pod runs.
// Redirects are not followed and no proxy is used: requests and credentials
// go to the configured Plainforge address and nowhere else.
//
// Neither the Plainforge address, the token, nor any human message text in a
// refusal ever appears in a returned error.
type Client struct {
	base      string
	tokenFile string
	http      *http.Client
}

var _ Forge = (*Client)(nil)

// NewClient returns a Forge v1 HTTP client for the Plainforge at address,
// which must be an http or https address with no path, query, user or
// fragment. An empty tokenFile sends no Authorization header, and a zero
// timeout means DefaultTimeout.
func NewClient(address, tokenFile string, timeout time.Duration) (*Client, error) {
	u, err := url.Parse(address)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" ||
		u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return nil, codes.Broken("plainforge address is not usable")
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

func (c *Client) token() (string, error) {
	if c.tokenFile == "" {
		return "", nil
	}
	f, err := os.Open(c.tokenFile)
	if err != nil {
		return "", codes.Broken("plainforge token cannot be read")
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxToken+1))
	if err != nil || len(data) > maxToken {
		return "", codes.Broken("plainforge token cannot be read")
	}
	tok := strings.TrimSpace(string(data))
	if tok == "" {
		return "", codes.Broken("plainforge token cannot be read")
	}
	for i := 0; i < len(tok); i++ {
		if tok[i] <= ' ' || tok[i] > '~' {
			return "", codes.Broken("plainforge token cannot be read")
		}
	}
	return tok, nil
}

type refusal struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type findResult struct {
	Found bool  `json:"found"`
	Task  *Task `json:"task,omitempty"`
}

type occurrenceRequest struct {
	ID string `json:"id"`
}

type changeResult struct {
	Created bool   `json:"created"`
	Branch  string `json:"branch"`
	Commit  string `json:"commit"`
}

type mirrorResult struct {
	Status string `json:"status"`
}

func (c *Client) do(ctx context.Context, method, path string, reqBody, respBody any) (int, error) {
	var bodyReader io.Reader
	if reqBody != nil {
		payload, err := json.Marshal(reqBody)
		if err != nil {
			return 0, codes.Broken("the call to plainforge could not be written out")
		}
		bodyReader = bytes.NewReader(payload)
	}
	tok, err := c.token()
	if err != nil {
		return 0, err
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, bodyReader)
	if err != nil {
		return 0, codes.Broken("plainforge address is not usable")
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "bellbuoy")
	if reqBody != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		// Drop the transport error text: it holds the remote address.
		return 0, codes.TimedOut("plainforge did not answer in time")
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxAnswer+1))
	if err != nil {
		return 0, codes.TimedOut("plainforge did not answer in time")
	}
	if len(data) > maxAnswer {
		return 0, codes.Broken("plainforge's answer was too large")
	}

	if resp.StatusCode >= 200 && resp.StatusCode <= 299 {
		if respBody != nil {
			dec := json.NewDecoder(bytes.NewReader(data))
			dec.DisallowUnknownFields()
			if err := dec.Decode(respBody); err != nil {
				return 0, codes.Broken("the answer from plainforge was not understood")
			}
			if _, err := dec.Token(); err != io.EOF {
				return 0, codes.Broken("the answer from plainforge was not understood")
			}
		}
		return resp.StatusCode, nil
	}

	var r refusal
	if json.Unmarshal(data, &r) == nil {
		switch code, _ := codes.Lookup(r.Code); code {
		case codes.Timeout:
			return 0, codes.TimedOut("plainforge asked to be tried again")
		case codes.Parse:
			return 0, codes.Broken("plainforge refused the call")
		case codes.NeedsSubtool:
			return 0, codes.Unfamiliar("plainforge has no rule for this call")
		}
	}
	switch {
	case resp.StatusCode == http.StatusRequestTimeout,
		resp.StatusCode == http.StatusTooManyRequests,
		resp.StatusCode >= 500:
		return 0, codes.TimedOut("plainforge asked to be tried again")
	case resp.StatusCode == http.StatusNotFound,
		resp.StatusCode == http.StatusUnprocessableEntity:
		return 0, codes.Unfamiliar("plainforge has no rule for this call")
	default:
		return 0, codes.Broken("plainforge refused the call")
	}
}

// isForgeRepo checks the repository name rule in Plainforge's contract/v1/forge.md:
// ^[a-z0-9]([a-z0-9._-]{0,62}[a-z0-9])?$, no "..", and not ending in ".git".
func isForgeRepo(s string) bool {
	if len(s) < 1 || len(s) > 64 || strings.Contains(s, "..") || strings.HasSuffix(s, ".git") {
		return false
	}
	if !isLowerAlnum(s[0]) || !isLowerAlnum(s[len(s)-1]) {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !isLowerAlnum(c) && c != '.' && c != '_' && c != '-' {
			return false
		}
	}
	return true
}

func isLowerAlnum(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9')
}

func isForgePath(s string) bool {
	if len(s) < 1 || len(s) > 512 {
		return false
	}
	for _, seg := range strings.Split(s, "/") {
		if seg == "" || seg == "." || seg == ".." || seg == ".git" {
			return false
		}
		for i := 0; i < len(seg); i++ {
			c := seg[i]
			if !isLowerAlnum(c) && !(c >= 'A' && c <= 'Z') && c != '.' && c != '_' && c != '-' {
				return false
			}
		}
	}
	return true
}

func isSingleLinePrintable(s string, max int) bool {
	if len(s) < 1 || len(s) > max || !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

func validTask(t Task, wantRepo string) bool {
	return t.ID != "" && t.Repo == wantRepo && t.Key != "" && t.Title != "" && t.Occurrences >= 1 && t.Open
}

// FindOpenTask looks up an open task in repo by exact key.
func (c *Client) FindOpenTask(ctx context.Context, repo, key string) (Task, bool, error) {
	if !isForgeRepo(repo) {
		return Task{}, false, codes.Broken("repository name is missing or malformed")
	}
	if !isSingleLinePrintable(key, 256) {
		return Task{}, false, codes.Broken("task key is missing or malformed")
	}
	q := url.Values{}
	q.Set("key", key)
	q.Set("open", "true")
	var res findResult
	status, err := c.do(ctx, http.MethodGet, "/v1/repos/"+repo+"/tasks?"+q.Encode(), nil, &res)
	if err != nil {
		return Task{}, false, err
	}
	if status != http.StatusOK {
		return Task{}, false, codes.Broken("the answer from plainforge was not understood")
	}
	if !res.Found {
		if res.Task != nil {
			return Task{}, false, codes.Broken("the answer from plainforge was not understood")
		}
		return Task{}, false, nil
	}
	if res.Task == nil || !validTask(*res.Task, repo) || res.Task.Key != key {
		return Task{}, false, codes.Broken("the answer from plainforge was not understood")
	}
	return *res.Task, true, nil
}

// OpenTask opens a new task in repo with an occurrence count of one.
func (c *Client) OpenTask(ctx context.Context, repo string, draft TaskDraft) (Task, error) {
	if !isForgeRepo(repo) {
		return Task{}, codes.Broken("repository name is missing or malformed")
	}
	if !isSingleLinePrintable(draft.Key, 256) || !isSingleLinePrintable(draft.Title, 512) ||
		!sanitize.IsText(draft.Body, 32768) || strings.ContainsRune(draft.Body, '\r') {
		return Task{}, codes.Broken("task draft is missing or malformed")
	}
	var t Task
	status, err := c.do(ctx, http.MethodPost, "/v1/repos/"+repo+"/tasks", draft, &t)
	if err != nil {
		return Task{}, err
	}
	if status != http.StatusCreated || !validTask(t, repo) || t.Key != draft.Key {
		return Task{}, codes.Broken("the answer from plainforge was not understood")
	}
	return t, nil
}

// AddOccurrence increments an open task's occurrence count in repo.
func (c *Client) AddOccurrence(ctx context.Context, repo, taskID string) (Task, error) {
	if !isForgeRepo(repo) {
		return Task{}, codes.Broken("repository name is missing or malformed")
	}
	if !isSingleLinePrintable(taskID, 128) {
		return Task{}, codes.Broken("task identifier is missing or malformed")
	}
	var t Task
	status, err := c.do(ctx, http.MethodPost, "/v1/repos/"+repo+"/tasks/occurrence", occurrenceRequest{ID: taskID}, &t)
	if err != nil {
		return Task{}, err
	}
	if status != http.StatusOK || !validTask(t, repo) || t.ID != taskID {
		return Task{}, codes.Broken("the answer from plainforge was not understood")
	}
	return t, nil
}

// ProposeChange commits a single-file replacement on change.Branch in
// change.Repo. It refuses any attempt to write to main or to a malformed path
// before touching the network.
func (c *Client) ProposeChange(ctx context.Context, change Change) (bool, error) {
	if !isForgeRepo(change.Repo) {
		return false, codes.Broken("repository name is missing or malformed")
	}
	if change.Branch == "main" || strings.HasPrefix(change.Branch, "refs/") || !isForgePath(change.Branch) {
		return false, codes.Broken("change branch is missing or malformed")
	}
	if !isForgePath(change.Path) {
		return false, codes.Broken("change file path is missing or malformed")
	}
	if !isSingleLinePrintable(change.Title, 512) || change.Message == "" ||
		!sanitize.IsText(change.Message, 16384) || strings.ContainsRune(change.Message, '\r') ||
		!sanitize.IsText(change.Content, 131072) || strings.ContainsRune(change.Content, '\r') {
		return false, codes.Broken("change text is missing or malformed")
	}
	var res changeResult
	status, err := c.do(ctx, http.MethodPost, "/v1/repos/"+change.Repo+"/changes", change, &res)
	if err != nil {
		return false, err
	}
	switch {
	case status == http.StatusCreated && res.Created:
	case status == http.StatusOK && !res.Created:
	default:
		return false, codes.Broken("the answer from plainforge was not understood")
	}
	if res.Branch != change.Branch || res.Commit == "" {
		return false, codes.Broken("the answer from plainforge was not understood")
	}
	return res.Created, nil
}

// SyncMirror asks Plainforge to synchronize the named content kinds for repo.
func (c *Client) SyncMirror(ctx context.Context, repo string, content []string) error {
	if !isForgeRepo(repo) {
		return codes.Broken("repository name is missing or malformed")
	}
	if len(content) == 0 {
		return codes.Broken("mirror content list is empty")
	}
	for _, kind := range content {
		if !sanitize.IsKind(kind) {
			return codes.Broken("mirror content kind is malformed")
		}
	}
	var res mirrorResult
	status, err := c.do(ctx, http.MethodPost, "/v1/repos/"+repo+"/mirror", MirrorRequest{Repo: repo, Content: content}, &res)
	if err != nil {
		return err
	}
	if status != http.StatusOK || res.Status != "ok" {
		return codes.Broken("the answer from plainforge was not understood")
	}
	return nil
}
