package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestFetchStatsRejectsOversizeResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// A complete JSON array at the start must not hide an oversized body.
		if _, err := io.WriteString(w, "[]"+strings.Repeat(" ", maxStatsResponse)); err != nil {
			t.Error(err)
		}
	}))
	t.Cleanup(server.Close)
	oldCore := core
	oldCreds := creds.Swap(&[2]string{"metrics", "token"})
	core = server.URL
	t.Cleanup(func() { core = oldCore; creds.Store(oldCreds) })

	stats, err := fetchStats()
	if stats != nil || err == nil || !strings.Contains(err.Error(), "stats response exceeds") {
		t.Fatalf("fetchStats returned %v, %v", stats, err)
	}
}
