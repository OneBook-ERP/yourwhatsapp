package outbox

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const pingKey = "ping-key-0123456789-0123456789-0123"

func TestPingPostsKeyAndHostToFrappe(t *testing.T) {
	var gotPath, gotHost, gotKey, gotMethod string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotHost, gotKey, gotMethod = r.URL.Path, r.Host, r.Header.Get("X-Api-Key"), r.Method
		w.Write([]byte(`{"message":{"ok":true}}`))
	}))
	defer srv.Close()
	if err := Ping(context.Background(), srv.URL+"/", "crm.example.com", pingKey); err != nil {
		t.Fatal(err)
	}
	if gotMethod != "POST" || gotPath != "/api/method/yourwhatsapp.yourwhatsapp.api.ping" {
		t.Fatalf("request = %s %s", gotMethod, gotPath)
	}
	if gotHost != "crm.example.com" || gotKey != pingKey {
		t.Fatalf("host %q key %q", gotHost, gotKey)
	}
}

func TestPingNon2xxIsUnreachable(t *testing.T) {
	for _, status := range []int{http.StatusForbidden, http.StatusNotFound, http.StatusFound, http.StatusInternalServerError} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if status == http.StatusFound {
				http.Redirect(w, r, "/login", status)
				return
			}
			w.WriteHeader(status)
		}))
		err := Ping(context.Background(), srv.URL, "s", pingKey)
		srv.Close()
		var ue *UnreachableError
		if !errors.As(err, &ue) || ue.Detail != fmt.Sprintf("http %d", status) {
			t.Errorf("status %d: err = %v", status, err)
		}
	}
}

func TestPingNetworkErrorNamesClassNotKey(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	err := Ping(context.Background(), url, "s", pingKey)
	var ue *UnreachableError
	if !errors.As(err, &ue) || ue.Detail != "connection error" {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(err.Error(), pingKey) {
		t.Fatal("error leaks the key")
	}
}
