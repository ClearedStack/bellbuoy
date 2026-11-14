package plainforge

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ClearedStack/bellbuoy/internal/codes"
)

func TestNoProxyAndNoRedirects(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://proxy.example.invalid:3128")
	t.Setenv("HTTPS_PROXY", "http://proxy.example.invalid:3128")
	c, err := NewClient("http://plainforge.example.invalid:8080", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	tr, ok := c.http.Transport.(*http.Transport)
	if !ok || tr.Proxy != nil {
		t.Fatal("the transport would use a proxy")
	}
	if c.http.CheckRedirect == nil {
		t.Fatal("redirects would be followed")
	}

	// Verify redirects are refused as E_PARSE and never followed to a second server.
	followed := false
	evil := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		followed = true
	}))
	defer evil.Close()
	redirectSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, evil.URL, http.StatusFound)
	}))
	defer redirectSrv.Close()

	rc, err := NewClient(redirectSrv.URL, "", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_, _, callErr := rc.FindOpenTask(context.Background(), "trawl", "fp-1")
	if callErr == nil || codes.From(callErr).Code != codes.Parse {
		t.Fatalf("redirect err = %v, want E_PARSE", callErr)
	}
	if followed {
		t.Fatal("redirect was followed")
	}
}

func TestTokenRotationAndValidation(t *testing.T) {
	dir := t.TempDir()
	tokenFile := filepath.Join(dir, "token")
	if err := os.WriteFile(tokenFile, []byte("token-v1\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var seenAuth []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenAuth = append(seenAuth, r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("{\"found\":false}\n"))
	}))
	defer srv.Close()

	c, err := NewClient(srv.URL, tokenFile, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, found, err := c.FindOpenTask(context.Background(), "trawl", "fp-1"); err != nil || found {
		t.Fatalf("call 1: found=%v err=%v", found, err)
	}
	if err := os.WriteFile(tokenFile, []byte("token-v2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, found, err := c.FindOpenTask(context.Background(), "trawl", "fp-1"); err != nil || found {
		t.Fatalf("call 2: found=%v err=%v", found, err)
	}
	if len(seenAuth) != 2 || seenAuth[0] != "Bearer token-v1" || seenAuth[1] != "Bearer token-v2" {
		t.Fatalf("seenAuth = %v", seenAuth)
	}

	// Unreadable or malformed token files fail closed as E_PARSE without leaking paths or secrets.
	if err := os.WriteFile(tokenFile, []byte("bad token with spaces\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, badErr := c.FindOpenTask(context.Background(), "trawl", "fp-1")
	if badErr == nil || codes.From(badErr).Code != codes.Parse || strings.Contains(badErr.Error(), tokenFile) || strings.Contains(badErr.Error(), "bad token") {
		t.Fatalf("bad token err = %v", badErr)
	}
}

func TestAddressTimeoutAndOversizedResponseChecks(t *testing.T) {
	for _, bad := range []string{
		"",
		"ftp://plainforge.example.invalid",
		"http://plainforge.example.invalid/subpath",
		"http://user:pass@plainforge.example.invalid",
		"http://plainforge.example.invalid?x=1",
		"http://plainforge.example.invalid#frag",
	} {
		if _, err := NewClient(bad, "", 0); err == nil || codes.From(err).Code != codes.Parse {
			t.Errorf("NewClient(%q) = %v, want E_PARSE", bad, err)
		}
	}

	// Oversized answer (> 1 MiB) is refused as E_PARSE.
	bigSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"found":false,"pad":"` + strings.Repeat("a", maxAnswer+16) + `"}`))
	}))
	defer bigSrv.Close()
	bc, err := NewClient(bigSrv.URL, "", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := bc.FindOpenTask(context.Background(), "trawl", "fp-1"); err == nil || codes.From(err).Code != codes.Parse {
		t.Fatalf("oversized answer err = %v, want E_PARSE", err)
	}

	// Closed server maps transport failure to E_TIMEOUT without leaking server URL.
	downSrv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	downURL := downSrv.URL
	downSrv.Close()
	dc, err := NewClient(downURL, "", 100*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	_, _, downErr := dc.FindOpenTask(context.Background(), "trawl", "fp-1")
	if downErr == nil || codes.From(downErr).Code != codes.Timeout || strings.Contains(downErr.Error(), downURL) {
		t.Fatalf("down server err = %v", downErr)
	}
}
