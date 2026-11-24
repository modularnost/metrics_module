package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"sync"
	"time"
)

const (
	historyTTL        = 2 * time.Second
	maxHistoryEntries = 128
)

type historyKey struct {
	service    string
	rangeLabel string
	tab        bool
}

type historyEntry struct {
	done    chan struct{}
	body    []byte
	err     error
	expires time.Time
}

type serviceHistory struct {
	mu      sync.Mutex
	entries map[historyKey]*historyEntry
}

func (h *serviceHistory) clear() {
	h.mu.Lock()
	h.entries = nil
	h.mu.Unlock()
}

func (h *serviceHistory) get(ctx context.Context, db *sql.DB, key historyKey, now time.Time) ([]byte, error) {
	h.mu.Lock()
	entry := h.entries[key]
	if entry != nil && (entry.expires.IsZero() || now.Before(entry.expires)) {
		h.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-entry.done:
			return entry.body, entry.err
		}
	}
	if h.entries == nil || len(h.entries) >= maxHistoryEntries {
		h.entries = make(map[historyKey]*historyEntry)
	}
	entry = &historyEntry{done: make(chan struct{})}
	h.entries[key] = entry
	h.mu.Unlock()

	doc, err := serviceDoc(db, key.service, key.tab, key.rangeLabel, now)
	var body []byte
	if err == nil {
		body, err = json.Marshal(doc)
	}
	h.mu.Lock()
	entry.body, entry.err = body, err
	entry.expires = time.Now().Add(historyTTL)
	if err != nil && h.entries[key] == entry {
		delete(h.entries, key)
	}
	close(entry.done)
	h.mu.Unlock()
	return body, err
}
