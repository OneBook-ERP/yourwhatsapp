package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHealthcheckCommand(t *testing.T) {
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer ok.Close()
	if err := healthcheck(ok.URL); err != nil {
		t.Fatalf("healthy server: %v", err)
	}

	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "down", http.StatusServiceUnavailable)
	}))
	defer bad.Close()
	if err := healthcheck(bad.URL); err == nil {
		t.Fatal("expected error for 503")
	}

	closed := httptest.NewServer(http.NotFoundHandler())
	url := closed.URL
	closed.Close()
	if err := healthcheck(url); err == nil {
		t.Fatal("expected error for unreachable server")
	}
}
