package muster

import (
	"net/http"
	"testing"
)

// The client must not pick up a proxy from the environment: the token goes
// to the configured address and nowhere else.
func TestNoProxyIsUsed(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://proxy.example.invalid:3128")
	t.Setenv("HTTPS_PROXY", "http://proxy.example.invalid:3128")
	c, err := NewClient("http://muster.example.invalid", "", 0)
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
}
