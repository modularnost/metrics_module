package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
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

	stats, _, err := fetchStats()
	if stats != nil || err == nil || !strings.Contains(err.Error(), "stats response exceeds") {
		t.Fatalf("fetchStats returned %v, %v", stats, err)
	}
}

func TestFetchStatsUsesPageInterestWhenAvailable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page_interest") != "1" {
			t.Error("stats request did not ask for page interest")
		}
		w.Header().Set("X-Panel-Watched-Pages", "path=%2Fservice")
		_, _ = io.WriteString(w, "[]")
	}))
	t.Cleanup(server.Close)
	oldCore := core
	oldCreds := creds.Swap(&[2]string{"metrics", "token"})
	core = server.URL
	t.Cleanup(func() { core = oldCore; creds.Store(oldCreds) })
	_, hint, err := fetchStats()
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		hint string
		want []string
	}{
		{hint, []string{"/service"}},
		{"none", nil},
		{"", []string{"/", "/service"}},
		{"path=%zz", []string{"/", "/service"}},
		{"path=%2F&path=%2F", []string{"/"}},
	} {
		if got := notificationPages(tt.hint); !slices.Equal(got, tt.want) {
			t.Errorf("hint %q: got %v, want %v", tt.hint, got, tt.want)
		}
	}
}
