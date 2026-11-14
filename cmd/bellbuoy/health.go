package main

import (
	"context"
	"net"
	"net/http"
	"time"

	"github.com/ClearedStack/bellbuoy/internal/codes"
)

// startProbe binds listenAddr and serves GET /healthz for kubelet liveness and
// Shipkeeper TCP readiness probes. When listenAddr is empty, no port is bound.
func startProbe(listenAddr string) (boundAddr string, stop func(), err error) {
	if listenAddr == "" {
		return "", func() {}, nil
	}
	ln, err := net.Listen("tcp", listenAddr)
	if err != nil {
		return "", nil, codes.Broken("probe listen address is not usable")
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte("{\"code\":\"E_NEEDS_SUBTOOL\"}\n"))
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusMethodNotAllowed)
			_, _ = w.Write([]byte("{\"code\":\"E_PARSE\"}\n"))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte("{\"status\":\"ok\"}\n"))
		}
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("{\"code\":\"E_NEEDS_SUBTOOL\"}\n"))
	})

	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 3 * time.Second,
		ReadTimeout:       3 * time.Second,
		WriteTimeout:      3 * time.Second,
		IdleTimeout:       30 * time.Second,
		MaxHeaderBytes:    4096,
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = srv.Serve(ln)
	}()

	stopFn := func() {
		shutCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
		<-done
	}
	return ln.Addr().String(), stopFn, nil
}
