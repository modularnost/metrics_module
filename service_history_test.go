package main

import (
	"bytes"
	"testing"
	"time"
)

func TestServiceHistoryReusesAndInvalidatesDocuments(t *testing.T) {
	db, err := openDB(t.TempDir() + "/samples.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var history serviceHistory
	now := time.Now()
	key := historyKey{service: "api", rangeLabel: "1h"}
	first, err := history.get(t.Context(), db, key, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := save(db, now.Add(-time.Minute), []stat{{Service: "api", CPU: 15}}); err != nil {
		t.Fatal(err)
	}
	cached, err := history.get(t.Context(), db, key, now)
	if err != nil || !bytes.Equal(first, cached) {
		t.Fatalf("identical history requests should reuse a document: %v", err)
	}
	history.clear()
	fresh, err := history.get(t.Context(), db, key, now)
	if err != nil || bytes.Equal(first, fresh) {
		t.Fatalf("invalidated history did not include the new sample: %v", err)
	}
}
