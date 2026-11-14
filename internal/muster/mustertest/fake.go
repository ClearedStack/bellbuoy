// Package mustertest is a fake Muster for tests: an HTTP server that speaks
// the five worker calls of contract v1 and is as strict about them as the
// contract says the real one is.
package mustertest

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
)

// Call is one request the fake received.
type Call struct {
	Path  string
	Token string
	Body  string
}

// Ended is one step a worker ended.
type Ended struct {
	Lease  string
	ID     string
	Call   string // complete, fail or needs_subtool
	State  string
	Code   string
	Result string
}

type step struct {
	fields  map[string]any
	attempt int
}

// Fake is a fake Muster.
type Fake struct {
	Server *httptest.Server

	mu     sync.Mutex
	tokens map[string]bool
	queue  []*step
	held   map[string]*step
	ended  map[string]int // lease -> index into done
	done   []Ended
	calls  []Call
	forced []forced
}

type forced struct {
	path   string // empty means any call
	status int
	body   string
}

// New starts a fake that accepts the given bearer tokens.
func New(tokens ...string) *Fake {
	f := &Fake{tokens: map[string]bool{}, held: map[string]*step{}, ended: map[string]int{}}
	for _, t := range tokens {
		f.tokens[t] = true
	}
	f.Server = httptest.NewServer(http.HandlerFunc(f.serve))
	return f
}

// Close stops the server.
func (f *Fake) Close() { f.Server.Close() }

// Offer queues a step. fields are the fields of the lease answer other than
// lease, attempt and expires, which the fake fills in.
func (f *Fake) Offer(fields map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queue = append(f.queue, &step{fields: fields})
}

// Accept adds a bearer token to the ones the fake accepts.
func (f *Fake) Accept(token string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tokens[token] = true
}

// Force makes the next n calls get this status and body, whatever they are.
func (f *Fake) Force(n, status int, body string) { f.ForcePath("", n, status, body) }

// ForcePath makes the next n calls to one path get this status and body.
// Calls to other paths are answered as usual.
func (f *Fake) ForcePath(path string, n, status int, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := 0; i < n; i++ {
		f.forced = append(f.forced, forced{path, status, body})
	}
}

// Expire ends a lease without an answer and offers its step again, first.
func (f *Fake) Expire(lease string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.held[lease]
	if !ok {
		return false
	}
	delete(f.held, lease)
	f.queue = append([]*step{s}, f.queue...)
	return true
}

// Calls returns every request received, in order.
func (f *Fake) Calls() []Call {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Call(nil), f.calls...)
}

// Count returns how many requests a path has received.
func (f *Fake) Count(path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if c.Path == path {
			n++
		}
	}
	return n
}

// Ended returns every step a worker ended, in order.
func (f *Fake) Ended() []Ended {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Ended(nil), f.done...)
}

// Held returns the lease tokens currently out.
func (f *Fake) Held() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for lease := range f.held {
		out = append(out, lease)
	}
	return out
}

func refuse(w http.ResponseWriter, status int, code, cause, reason string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": code, "cause": cause, "reason": reason})
}

func answer(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

// strict decodes a request body into v, refusing unknown fields and anything
// after the object.
func strict(body []byte, v any) bool {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if dec.Decode(v) != nil {
		return false
	}
	_, err := dec.Token()
	return err == io.EOF
}

func (f *Fake) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(io.LimitReader(r.Body, 64<<10+1))
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")

	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, Call{Path: r.URL.Path, Token: token, Body: string(body)})

	for i, next := range f.forced {
		if next.path == "" || next.path == r.URL.Path {
			f.forced = append(f.forced[:i:i], f.forced[i+1:]...)
			w.WriteHeader(next.status)
			io.WriteString(w, next.body)
			return
		}
	}
	switch {
	case r.Method != http.MethodPost:
		refuse(w, http.StatusMethodNotAllowed, "E_PARSE", "method", "that call is made with POST")
		return
	case r.Header.Get("Content-Type") != "application/json":
		refuse(w, http.StatusUnsupportedMediaType, "E_PARSE", "content-type", "the body must be JSON")
		return
	case len(body) > 64<<10:
		refuse(w, http.StatusRequestEntityTooLarge, "E_PARSE", "size", "the request is too large")
		return
	case !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") || !f.tokens[token]:
		refuse(w, http.StatusUnauthorized, "E_PARSE", "credential", "no valid credential")
		return
	}

	var req struct {
		Contract string           `json:"contract"`
		Lease    *string          `json:"lease"`
		State    *string          `json:"state"`
		Result   *json.RawMessage `json:"result"`
		Code     *string          `json:"code"`
	}
	if !strict(body, &req) {
		refuse(w, http.StatusBadRequest, "E_PARSE", "body", "the body is not what this call takes")
		return
	}
	// Each call takes its own fields and no others.
	allowed := map[string][3]bool{ // lease, state+result, code
		"/v1/lease":         {false, false, false},
		"/v1/heartbeat":     {true, false, false},
		"/v1/complete":      {true, true, false},
		"/v1/fail":          {true, false, true},
		"/v1/needs_subtool": {true, false, false},
	}
	takes, ok := allowed[r.URL.Path]
	if !ok {
		refuse(w, http.StatusNotFound, "E_PARSE", "path", "no such call")
		return
	}
	if (req.Lease != nil) != takes[0] || (!takes[1] && (req.State != nil || req.Result != nil)) ||
		(takes[1] && req.State == nil) || (req.Code != nil) != takes[2] {
		refuse(w, http.StatusBadRequest, "E_PARSE", "body", "the body is not what this call takes")
		return
	}
	if req.Contract != "v1" {
		refuse(w, http.StatusUnprocessableEntity, "E_NEEDS_SUBTOOL", "contract", "that contract version is not known")
		return
	}

	if r.URL.Path == "/v1/lease" {
		if len(f.queue) == 0 {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		s := f.queue[0]
		f.queue = f.queue[1:]
		s.attempt++
		id, _ := s.fields["id"].(string)
		kind, _ := s.fields["kind"].(string)
		lease := id + "." + strconv.Itoa(s.attempt)
		if kind == "incident" || kind == "release" {
			lease = kind + ":" + lease
		}
		f.held[lease] = s
		out := map[string]any{}
		for k, v := range s.fields {
			out[k] = v
		}
		out["lease"] = lease
		out["attempt"] = s.attempt
		out["expires"] = "2026-01-05T00:01:00Z"
		answer(w, out)
		return
	}

	lease := *req.Lease
	if i, ok := f.ended[lease]; ok {
		// Repeating the call that ended a lease gives the same answer and
		// does nothing twice.
		if "/v1/"+f.done[i].Call == r.URL.Path {
			answer(w, map[string]any{"id": f.done[i].ID, "state": f.done[i].State})
			return
		}
		refuse(w, http.StatusConflict, "E_PARSE", "lease_not_held", "the lease is not held")
		return
	}
	s, ok := f.held[lease]
	if !ok {
		refuse(w, http.StatusConflict, "E_PARSE", "lease_not_held", "the lease is not held")
		return
	}
	id, _ := s.fields["id"].(string)
	state, _ := s.fields["state"].(string)
	kind, _ := s.fields["kind"].(string)
	end := Ended{Lease: lease, ID: id, State: state}
	notAllowed := func() {
		refuse(w, http.StatusConflict, "E_PARSE", "move_not_allowed", "that move is not allowed")
	}
	switch r.URL.Path {
	case "/v1/heartbeat":
		answer(w, map[string]any{"lease": lease, "expires": "2026-01-05T00:02:00Z"})
		return
	case "/v1/complete":
		if kind == "incident" {
			// An incident step changes no state and takes no result.
			if *req.State != state {
				notAllowed()
				return
			}
			if req.Result != nil {
				refuse(w, http.StatusBadRequest, "E_PARSE", "bad_field", "a field has the wrong type or form")
				return
			}
		}
		end.Call = "complete"
		end.State = *req.State
		if req.Result != nil {
			var object map[string]json.RawMessage
			if json.Unmarshal(*req.Result, &object) != nil || object == nil {
				refuse(w, http.StatusBadRequest, "E_PARSE", "result", "a result is a JSON object")
				return
			}
			end.Result = string(*req.Result)
		}
	case "/v1/fail":
		if *req.Code != "E_TIMEOUT" && *req.Code != "E_PARSE" {
			refuse(w, http.StatusBadRequest, "E_PARSE", "code", "a step fails with E_TIMEOUT or E_PARSE")
			return
		}
		end.Call = "fail"
		end.Code = *req.Code
	case "/v1/needs_subtool":
		if kind != "item" {
			// Only an item's own step can be paused.
			notAllowed()
			return
		}
		end.Call = "needs_subtool"
		end.State = "needs_subtool"
	}
	delete(f.held, lease)
	f.ended[lease] = len(f.done)
	f.done = append(f.done, end)
	answer(w, map[string]any{"id": end.ID, "state": end.State})
}
