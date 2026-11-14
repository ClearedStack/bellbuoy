// Package plainforgetest serves Plainforge's Forge HTTP Contract v1 in tests
// against an in-memory store.
package plainforgetest

import (
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"

	"github.com/ClearedStack/bellbuoy/internal/codes"
	"github.com/ClearedStack/bellbuoy/internal/plainforge"
)

// Fake is an httptest.Server speaking Plainforge's Forge HTTP Contract v1.
type Fake struct {
	Server *httptest.Server
	Memory *plainforge.Memory

	mu     sync.Mutex
	token  string
	repos  map[string]bool
	forced map[string]forcedReply
	counts map[string]int
}

type forcedReply struct {
	left   int
	status int
	body   string
}

// New starts a strict Forge v1 test server. When token is non-empty, every
// call must supply Authorization: Bearer <token>. When repos is non-empty,
// only listed repositories exist (unlisted ones answer 404 E_NEEDS_SUBTOOL).
func New(token string, repos ...string) *Fake {
	f := &Fake{
		Memory: plainforge.NewMemory(),
		token:  token,
		repos:  map[string]bool{},
		forced: map[string]forcedReply{},
		counts: map[string]int{},
	}
	for _, r := range repos {
		f.repos[r] = true
	}
	f.Server = httptest.NewServer(http.HandlerFunc(f.serve))
	return f
}

// Close shuts down the underlying HTTP server.
func (f *Fake) Close() { f.Server.Close() }

// SetToken replaces the expected bearer token at runtime (simulating rotation).
func (f *Fake) SetToken(token string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.token = token
}

// ForcePath forces the next n requests whose path ends with suffix (for
// example "/mirror") to answer with status and body.
func (f *Fake) ForcePath(suffix string, n, status int, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.forced[suffix] = forcedReply{left: n, status: status, body: body}
}

// Count returns how many requests reached an endpoint suffix ("/tasks",
// "/tasks/occurrence", "/changes", "/mirror").
func (f *Fake) Count(suffix string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.counts[suffix]
}

func (f *Fake) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	wantTok := f.token
	f.mu.Unlock()

	if wantTok != "" && r.Header.Get("Authorization") != "Bearer "+wantTok {
		writeErr(w, http.StatusUnauthorized, "E_PARSE", "missing or invalid bearer token")
		return
	}

	const prefix = "/v1/repos/"
	if !strings.HasPrefix(r.URL.Path, prefix) {
		writeErr(w, http.StatusNotFound, "E_NEEDS_SUBTOOL", "Plainforge does not serve this path")
		return
	}
	repo, rest, ok := strings.Cut(strings.TrimPrefix(r.URL.Path, prefix), "/")
	if !ok || repo == "" {
		writeErr(w, http.StatusNotFound, "E_NEEDS_SUBTOOL", "Plainforge does not serve this path")
		return
	}

	suffix := "/" + rest
	f.mu.Lock()
	f.counts[suffix]++
	if len(f.repos) > 0 && !f.repos[repo] {
		f.mu.Unlock()
		writeErr(w, http.StatusNotFound, "E_NEEDS_SUBTOOL", "this Plainforge has no repository with that name")
		return
	}
	if fr, has := f.forced[suffix]; has && fr.left > 0 {
		fr.left--
		f.forced[suffix] = fr
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(fr.status)
		io.WriteString(w, fr.body)
		return
	}
	f.mu.Unlock()

	ctx := r.Context()
	switch rest {
	case "tasks":
		switch r.Method {
		case http.MethodGet:
			q, err := url.ParseQuery(r.URL.RawQuery)
			if err != nil || strings.Contains(r.URL.RawQuery, ";") {
				writeErr(w, http.StatusBadRequest, "E_PARSE", "the query string is malformed")
				return
			}
			if openVal := q.Get("open"); openVal != "" && openVal != "true" {
				writeErr(w, http.StatusUnprocessableEntity, "E_NEEDS_SUBTOOL", "only open=true task lookups are supported")
				return
			}
			key := q.Get("key")
			if key == "" {
				writeErr(w, http.StatusBadRequest, "E_PARSE", "the task key is missing or malformed")
				return
			}
			task, found, err := f.Memory.FindOpenTask(ctx, repo, key)
			if err != nil {
				writeCodeErr(w, err)
				return
			}
			if !found {
				writeJSON(w, http.StatusOK, map[string]any{"found": false})
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{"found": true, "task": task})
		case http.MethodPost:
			var draft plainforge.TaskDraft
			if !decodeStrict(w, r, 64<<10, &draft) {
				return
			}
			task, err := f.Memory.OpenTask(ctx, repo, draft)
			if err != nil {
				writeCodeErr(w, err)
				return
			}
			writeJSON(w, http.StatusCreated, task)
		default:
			writeErr(w, http.StatusMethodNotAllowed, "E_PARSE", "that HTTP method is not used here")
		}

	case "tasks/occurrence":
		if r.Method != http.MethodPost {
			writeErr(w, http.StatusMethodNotAllowed, "E_PARSE", "that HTTP method is not used here")
			return
		}
		var req struct {
			ID string `json:"id"`
		}
		if !decodeStrict(w, r, 4<<10, &req) {
			return
		}
		task, err := f.Memory.AddOccurrence(ctx, repo, req.ID)
		if err != nil {
			writeCodeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, task)

	case "changes":
		if r.Method != http.MethodPost {
			writeErr(w, http.StatusMethodNotAllowed, "E_PARSE", "that HTTP method is not used here")
			return
		}
		var change plainforge.Change
		if !decodeStrict(w, r, 256<<10, &change) {
			return
		}
		if change.Repo == "" {
			change.Repo = repo
		}
		if change.Repo != repo || change.Branch == "" || change.Branch == "main" {
			writeErr(w, http.StatusBadRequest, "E_PARSE", "the change branch must be a non-main branch name")
			return
		}
		created, err := f.Memory.ProposeChange(ctx, change)
		if err != nil {
			pub := codes.From(err)
			if pub.Reason == "the branch already holds a different change" {
				writeErr(w, http.StatusConflict, "E_PARSE", pub.Reason)
				return
			}
			writeCodeErr(w, err)
			return
		}
		sum := sha1.Sum([]byte(change.Repo + "\x00" + change.Branch + "\x00" + change.Path + "\x00" + change.Content))
		status := http.StatusOK
		if created {
			status = http.StatusCreated
		}
		writeJSON(w, status, map[string]any{
			"created": created,
			"branch":  change.Branch,
			"commit":  hex.EncodeToString(sum[:]),
		})

	case "mirror":
		if r.Method != http.MethodPost {
			writeErr(w, http.StatusMethodNotAllowed, "E_PARSE", "that HTTP method is not used here")
			return
		}
		var req plainforge.MirrorRequest
		if !decodeStrict(w, r, 4<<10, &req) {
			return
		}
		if req.Repo != "" && req.Repo != repo {
			writeErr(w, http.StatusBadRequest, "E_PARSE", "repository mismatch")
			return
		}
		if len(req.Content) == 0 {
			writeErr(w, http.StatusBadRequest, "E_PARSE", "mirror content list is empty")
			return
		}
		for _, c := range req.Content {
			if c != "source" && c != "task" {
				writeErr(w, http.StatusUnprocessableEntity, "E_NEEDS_SUBTOOL", "this kind of content may not be mirrored")
				return
			}
		}
		if err := f.Memory.SyncMirror(ctx, repo, req.Content); err != nil {
			writeCodeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})

	default:
		writeErr(w, http.StatusNotFound, "E_NEEDS_SUBTOOL", "Plainforge does not serve this path")
	}
}

func decodeStrict(w http.ResponseWriter, r *http.Request, limit int64, dst any) bool {
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		writeErr(w, http.StatusBadRequest, "E_PARSE", "Content-Type must be application/json")
		return false
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil || int64(len(raw)) > limit {
		writeErr(w, http.StatusBadRequest, "E_PARSE", "request body too large")
		return false
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		writeErr(w, http.StatusBadRequest, "E_PARSE", "the request JSON is malformed")
		return false
	}
	if _, err := dec.Token(); err != io.EOF {
		writeErr(w, http.StatusBadRequest, "E_PARSE", "the request JSON is malformed")
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
}

func writeErr(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]string{"code": code, "message": msg})
}

func writeCodeErr(w http.ResponseWriter, err error) {
	pub := codes.From(err)
	switch pub.Code {
	case codes.Timeout:
		writeErr(w, http.StatusServiceUnavailable, "E_TIMEOUT", pub.Reason)
	case codes.NeedsSubtool:
		writeErr(w, http.StatusUnprocessableEntity, "E_NEEDS_SUBTOOL", pub.Reason)
	default:
		writeErr(w, http.StatusBadRequest, "E_PARSE", pub.Reason)
	}
}
