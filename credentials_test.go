package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestDiscoveryCredentialsRequireCoreAcceptance(t *testing.T) {
	coreServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/internal/stats" {
			t.Errorf("unexpected validation request: %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("X-Panel-Module") == "metrics" && r.Header.Get("X-Panel-Token") == "current" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(coreServer.Close)
	oldCore := core
	core = coreServer.URL
	t.Cleanup(func() { core = oldCore })

	if !verifiedCredentials([2]string{"metrics", "current"}) {
		t.Fatal("accepted credentials were rejected")
	}
	if verifiedCredentials([2]string{"metrics", "expired"}) {
		t.Fatal("rejected credentials were accepted")
	}
}
