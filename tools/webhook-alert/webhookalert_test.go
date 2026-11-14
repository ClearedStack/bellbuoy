package webhookalert

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ClearedStack/bellbuoy/internal/codes"
	"github.com/ClearedStack/bellbuoy/internal/fixture"
	"github.com/ClearedStack/bellbuoy/internal/work"
)

func TestFixtures(t *testing.T) {
	fixture.Run(t, "fixtures", "../../testdata/ownership_map.json", func(d fixture.Deps) work.Tool {
		return &Tool{Sender: d.Alerts}
	})
}

func TestNoWebhookConfiguredPausesTheJob(t *testing.T) {
	tool := &Tool{}
	job := work.Job{ID: "job-1", Lane: work.LaneUrgent, Kind: work.KindDrift, Payload: []byte(`{"ref":"rule-0001"}`)}
	st := work.State{}
	if !tool.Match(job, st) {
		t.Fatal("urgent drift must match")
	}
	err := tool.Run(context.Background(), job, &st)
	if e := codes.From(err); e == nil || e.Code != codes.NeedsSubtool {
		t.Fatalf("err = %v, want E_NEEDS_SUBTOOL", err)
	}
}

func codeOf(err error) codes.Code {
	if e := codes.From(err); e != nil {
		return e.Code
	}
	return ""
}

func TestHTTPSenderStatusRule(t *testing.T) {
	cases := []struct {
		status int
		want   codes.Code
	}{
		{200, ""}, {201, ""}, {202, ""}, {204, ""},
		{408, codes.Timeout}, {429, codes.Timeout}, {500, codes.Timeout}, {502, codes.Timeout}, {503, codes.Timeout},
		{301, codes.Parse}, {302, codes.Parse}, {400, codes.Parse}, {401, codes.Parse}, {403, codes.Parse}, {404, codes.Parse}, {410, codes.Parse},
	}
	for _, c := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if c.status >= 300 && c.status < 400 {
				w.Header().Set("Location", "/elsewhere")
			}
			w.WriteHeader(c.status)
		}))
		sender, err := NewHTTPSender(srv.URL+"/hook", time.Second)
		if err != nil {
			t.Fatal(err)
		}
		if got := codeOf(sender.Send(context.Background(), []byte(`{}`))); got != c.want {
			t.Errorf("status %d: got %q, want %q", c.status, got, c.want)
		}
		srv.Close()
	}
}

func TestHTTPSenderSendsTheBodyAsJSON(t *testing.T) {
	var gotBody, gotType, gotMethod string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody, gotType, gotMethod = string(b), r.Header.Get("Content-Type"), r.Method
	}))
	defer srv.Close()
	sender, _ := NewHTTPSender(srv.URL, time.Second)
	if err := sender.Send(context.Background(), []byte(`{"source":"bellbuoy"}`)); err != nil {
		t.Fatal(err)
	}
	if gotBody != `{"source":"bellbuoy"}` || gotType != "application/json" || gotMethod != "POST" {
		t.Fatalf("got %q %q %q", gotMethod, gotType, gotBody)
	}
}

func TestHTTPSenderDoesNotFollowRedirects(t *testing.T) {
	followed := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/elsewhere" {
			followed = true
			return
		}
		http.Redirect(w, r, "/elsewhere", http.StatusTemporaryRedirect)
	}))
	defer srv.Close()
	sender, _ := NewHTTPSender(srv.URL, time.Second)
	if got := codeOf(sender.Send(context.Background(), []byte(`{}`))); got != codes.Parse {
		t.Fatalf("redirect: got %q, want E_PARSE", got)
	}
	if followed {
		t.Fatal("the alert was sent on to an address nobody configured")
	}
}

func TestHTTPSenderSlowEndpointIsATimeout(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-release }))
	defer srv.Close()
	defer close(release)
	sender, _ := NewHTTPSender(srv.URL, 50*time.Millisecond)
	if got := codeOf(sender.Send(context.Background(), []byte(`{}`))); got != codes.Timeout {
		t.Fatalf("slow endpoint: got %q, want E_TIMEOUT", got)
	}
}

// An endpoint that is down must give E_TIMEOUT, and the error must not name
// the address, which may carry a secret in its path.
func TestHTTPSenderErrorsNeverNameTheAddress(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	address := srv.URL + "/hooks/made-up-secret-token"
	srv.Close() // nothing listens there now
	sender, err := NewHTTPSender(address, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	err = sender.Send(context.Background(), []byte(`{}`))
	if codeOf(err) != codes.Timeout {
		t.Fatalf("got %v, want E_TIMEOUT", err)
	}
	host := strings.TrimPrefix(srv.URL, "http://")
	for _, leak := range []string{"made-up-secret-token", host, "127.0.0.1", "http"} {
		if strings.Contains(err.Error(), leak) {
			t.Fatalf("error text leaks %q: %s", leak, err)
		}
	}
}

func TestNewHTTPSenderRefusesBadAddresses(t *testing.T) {
	for _, address := range []string{"", "not an address", "ftp://example.invalid/x", "file:///etc/hosts", "https://", "//example.invalid/x", "http://%zz"} {
		_, err := NewHTTPSender(address, 0)
		if codeOf(err) != codes.Parse {
			t.Errorf("NewHTTPSender(%q) = %v, want E_PARSE", address, err)
			continue
		}
		if address != "" && strings.Contains(err.Error(), address) {
			t.Errorf("error text repeats the address %q", address)
		}
	}
}
