package plainforge_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/ClearedStack/bellbuoy/internal/codes"
	"github.com/ClearedStack/bellbuoy/internal/plainforge"
)

type forgeExample struct {
	Name        string `json:"name"`
	Method      string `json:"method"`
	Path        string `json:"path"`
	ContentType string `json:"contentType"`
	Body        string `json:"body"`
	WantStatus  int    `json:"wantStatus"`
	WantBody    string `json:"wantBody"`
}

func compactJSON(t *testing.T, raw string) string {
	t.Helper()
	var buf bytes.Buffer
	if err := json.Compact(&buf, []byte(raw)); err != nil {
		t.Fatalf("not JSON (%q): %v", raw, err)
	}
	return buf.String()
}

func exportedMethods(typ reflect.Type) []string {
	var out []string
	for i := 0; i < typ.NumMethod(); i++ {
		out = append(out, typ.Method(i).Name)
	}
	sort.Strings(out)
	return out
}

// TestPlainforgeContractExamples replays all 15 saved examples from
// ClearedStack/plainforge's contract/v1/examples/forge_exchange.json.
func TestPlainforgeContractExamples(t *testing.T) {
	raw, err := os.ReadFile("../../testdata/plainforge_contract/forge_exchange.json")
	if err != nil {
		t.Fatalf("read forge_exchange.json: %v", err)
	}
	var examples []forgeExample
	if err := json.Unmarshal(raw, &examples); err != nil {
		t.Fatalf("decode forge_exchange.json: %v", err)
	}
	if len(examples) != 15 {
		t.Fatalf("loaded %d examples, want 15", len(examples))
	}

	gotMethods := exportedMethods(reflect.TypeOf(&plainforge.Client{}))
	wantMethods := []string{"AddOccurrence", "FindOpenTask", "OpenTask", "ProposeChange", "SyncMirror"}
	if !reflect.DeepEqual(gotMethods, wantMethods) {
		t.Fatalf("plainforge.Client methods = %v, want %v", gotMethods, wantMethods)
	}

	ctx := context.Background()
	replayed := 0

	for _, ex := range examples {
		t.Run(ex.Name, func(t *testing.T) {
			var hits int
			var gotMethod, gotURI, gotType, gotBody string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hits++
				b, _ := io.ReadAll(r.Body)
				gotMethod = r.Method
				gotURI = r.URL.RequestURI()
				gotType = r.Header.Get("Content-Type")
				gotBody = string(b)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(ex.WantStatus)
				io.WriteString(w, ex.WantBody)
			}))
			defer srv.Close()

			client, err := plainforge.NewClient(srv.URL, "", time.Second)
			if err != nil {
				t.Fatal(err)
			}

			u, err := url.Parse(ex.Path)
			if err != nil {
				t.Fatalf("bad path %q: %v", ex.Path, err)
			}
			const prefix = "/v1/repos/"
			if !strings.HasPrefix(u.Path, prefix) {
				t.Fatalf("unexpected path %q", u.Path)
			}
			repo, rest, ok := strings.Cut(strings.TrimPrefix(u.Path, prefix), "/")
			if !ok {
				t.Fatalf("unexpected repo path %q", u.Path)
			}

			switch {
			case ex.Method == http.MethodGet && rest == "tasks":
				key := u.Query().Get("key")
				task, found, callErr := client.FindOpenTask(ctx, repo, key)
				if hits != 1 || gotMethod != ex.Method || gotURI != ex.Path {
					t.Fatalf("wire = (%d, %s, %s), want (1, %s, %s)", hits, gotMethod, gotURI, ex.Method, ex.Path)
				}
				if ex.WantStatus >= 400 {
					checkForgeRefusal(t, ex.WantBody, callErr)
					return
				}
				if callErr != nil {
					t.Fatalf("FindOpenTask error: %v", callErr)
				}
				var want struct {
					Found bool             `json:"found"`
					Task  *plainforge.Task `json:"task"`
				}
				if err := json.Unmarshal([]byte(ex.WantBody), &want); err != nil {
					t.Fatal(err)
				}
				if found != want.Found {
					t.Fatalf("found = %v, want %v", found, want.Found)
				}
				if found && task != *want.Task {
					t.Fatalf("task = %+v, want %+v", task, *want.Task)
				}

			case ex.Method == http.MethodPost && rest == "tasks":
				var fields map[string]json.RawMessage
				if err := json.Unmarshal([]byte(ex.Body), &fields); err != nil {
					t.Fatal(err)
				}
				sendable := true
				for k := range fields {
					if k != "key" && k != "title" && k != "body" {
						sendable = false
					}
				}
				var draft plainforge.TaskDraft
				if sendable {
					if err := json.Unmarshal([]byte(ex.Body), &draft); err != nil {
						t.Fatal(err)
					}
				} else {
					draft = plainforge.TaskDraft{Key: "fp-1", Title: "t", Body: "b"}
				}
				task, callErr := client.OpenTask(ctx, repo, draft)
				if hits != 1 || gotMethod != ex.Method || gotURI != ex.Path || gotType != ex.ContentType {
					t.Fatalf("wire = (%d, %s, %s, %s)", hits, gotMethod, gotURI, gotType)
				}
				if sendable && compactJSON(t, gotBody) != compactJSON(t, ex.Body) {
					t.Fatalf("sent body = %s, want %s", gotBody, ex.Body)
				}
				if ex.WantStatus >= 400 {
					checkForgeRefusal(t, ex.WantBody, callErr)
					return
				}
				if callErr != nil {
					t.Fatalf("OpenTask error: %v", callErr)
				}
				var want plainforge.Task
				if err := json.Unmarshal([]byte(ex.WantBody), &want); err != nil {
					t.Fatal(err)
				}
				if task != want {
					t.Fatalf("task = %+v, want %+v", task, want)
				}

			case ex.Method == http.MethodPost && rest == "tasks/occurrence":
				var req struct {
					ID string `json:"id"`
				}
				if err := json.Unmarshal([]byte(ex.Body), &req); err != nil {
					t.Fatal(err)
				}
				task, callErr := client.AddOccurrence(ctx, repo, req.ID)
				if hits != 1 || gotMethod != ex.Method || gotURI != ex.Path || gotType != ex.ContentType ||
					compactJSON(t, gotBody) != compactJSON(t, ex.Body) {
					t.Fatalf("wire = (%d, %s, %s, %s, %s)", hits, gotMethod, gotURI, gotType, gotBody)
				}
				if ex.WantStatus >= 400 {
					checkForgeRefusal(t, ex.WantBody, callErr)
					return
				}
				if callErr != nil {
					t.Fatalf("AddOccurrence error: %v", callErr)
				}
				var want plainforge.Task
				if err := json.Unmarshal([]byte(ex.WantBody), &want); err != nil {
					t.Fatal(err)
				}
				if task != want {
					t.Fatalf("task = %+v, want %+v", task, want)
				}

			case ex.Method == http.MethodPost && rest == "changes":
				var change plainforge.Change
				if err := json.Unmarshal([]byte(ex.Body), &change); err != nil {
					t.Fatal(err)
				}
				if change.Repo == "" {
					change.Repo = repo
				}
				// Client-side defense in depth rejects main and path traversal
				// before touching the wire.
				if change.Branch == "main" || strings.Contains(change.Path, "..") {
					if _, localErr := client.ProposeChange(ctx, change); localErr == nil || codes.From(localErr).Code != codes.Parse {
						t.Fatalf("expected local E_PARSE refusal before HTTP, got %v", localErr)
					}
					if hits != 0 {
						t.Fatalf("unsafe change reached the network (%d hits)", hits)
					}
					// Also verify that if Plainforge returns this refusal on the
					// wire, Client maps its code and strips its message.
					_, wireErr := client.ProposeChange(ctx, plainforge.Change{
						Repo:    repo,
						Branch:  "edit/EP-0001",
						Path:    "config/sources.json",
						Content: "{}",
						Title:   "Stand-in",
						Message: "Stand-in",
					})
					checkForgeRefusal(t, ex.WantBody, wireErr)
					return
				}

				created, callErr := client.ProposeChange(ctx, change)
				if hits != 1 || gotMethod != ex.Method || gotURI != ex.Path || gotType != ex.ContentType ||
					compactJSON(t, gotBody) != compactJSON(t, ex.Body) {
					t.Fatalf("wire = (%d, %s, %s, %s, %s)", hits, gotMethod, gotURI, gotType, gotBody)
				}
				if ex.WantStatus >= 400 {
					checkForgeRefusal(t, ex.WantBody, callErr)
					return
				}
				if callErr != nil {
					t.Fatalf("ProposeChange error: %v", callErr)
				}
				var want struct {
					Created bool `json:"created"`
				}
				if err := json.Unmarshal([]byte(ex.WantBody), &want); err != nil {
					t.Fatal(err)
				}
				if created != want.Created {
					t.Fatalf("created = %v, want %v", created, want.Created)
				}

			case ex.Method == http.MethodPost && rest == "mirror":
				var req plainforge.MirrorRequest
				if err := json.Unmarshal([]byte(ex.Body), &req); err != nil {
					t.Fatal(err)
				}
				callErr := client.SyncMirror(ctx, repo, req.Content)
				if hits != 1 || gotMethod != ex.Method || gotURI != ex.Path || gotType != ex.ContentType ||
					compactJSON(t, gotBody) != compactJSON(t, ex.Body) {
					t.Fatalf("wire = (%d, %s, %s, %s, %s)", hits, gotMethod, gotURI, gotType, gotBody)
				}
				if ex.WantStatus >= 400 {
					checkForgeRefusal(t, ex.WantBody, callErr)
					return
				}
				if callErr != nil {
					t.Fatalf("SyncMirror error: %v", callErr)
				}

			default:
				t.Fatalf("unhandled contract example: %s %s", ex.Method, ex.Path)
			}
		})
		replayed++
	}

	if replayed != len(examples) {
		t.Fatalf("replayed %d examples, want %d", replayed, len(examples))
	}
}

func checkForgeRefusal(t *testing.T, wantBody string, gotErr error) {
	t.Helper()
	if gotErr == nil {
		t.Fatal("expected refusal error, got nil")
	}
	var env struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal([]byte(wantBody), &env); err != nil {
		t.Fatalf("decode refusal body: %v", err)
	}
	pub := codes.From(gotErr)
	if string(pub.Code) != env.Code {
		t.Fatalf("code = %s, want %s (err=%v)", pub.Code, env.Code, gotErr)
	}
	if env.Message != "" && strings.Contains(gotErr.Error(), env.Message) {
		t.Fatalf("error %q leaked Plainforge refusal message %q", gotErr.Error(), env.Message)
	}
}
