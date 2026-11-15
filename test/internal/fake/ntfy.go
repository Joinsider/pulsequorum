// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// maxNtfyBody bounds how much of one ntfy request body is read and recorded.
const maxNtfyBody = 64 << 10

// ntfyMessage is one notification that Shoutrrr's ntfy service posted.
type ntfyMessage struct {
	At       time.Time
	Path     string
	Title    string
	Priority string
	Tags     string
	Body     string
}

// ntfyRecorder is an in-process stand-in for an ntfy server. It accepts
// POST /<topic> as Shoutrrr's ntfy service sends it (plain-text body, Title,
// Priority and Tags headers) and records every message with its arrival time.
type ntfyRecorder struct {
	mu   sync.Mutex
	msgs []ntfyMessage
	seq  int
	now  func() time.Time
}

func newNtfyRecorder() *ntfyRecorder {
	return &ntfyRecorder{now: time.Now}
}

// ServeHTTP implements http.Handler.
func (r *ntfyRecorder) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(req.Body, maxNtfyBody))
	if err != nil {
		http.Error(w, "cannot read body", http.StatusBadRequest)
		return
	}
	topic := strings.Trim(req.URL.Path, "/")

	r.mu.Lock()
	r.seq++
	id := r.seq
	r.msgs = append(r.msgs, ntfyMessage{
		At:       r.now(),
		Path:     req.URL.Path,
		Title:    req.Header.Get("Title"),
		Priority: req.Header.Get("Priority"),
		Tags:     req.Header.Get("Tags"),
		Body:     string(body),
	})
	r.mu.Unlock()

	// Answer like ntfy does: a JSON message object.
	topicJSON, _ := json.Marshal(topic)
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"id":"as01-%d","time":%d,"event":"message","topic":%s}`+"\n",
		id, r.now().Unix(), topicJSON)
}

// Messages returns a snapshot of all recorded messages.
func (r *ntfyRecorder) Messages() []ntfyMessage {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]ntfyMessage(nil), r.msgs...)
}

// WaitFor returns the first recorded message that arrived at or after since
// and satisfies match. It polls until ctx ends.
func (r *ntfyRecorder) WaitFor(ctx context.Context, since time.Time, match func(ntfyMessage) bool) (ntfyMessage, bool) {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		for _, m := range r.Messages() {
			if !m.At.Before(since) && match(m) {
				return m, true
			}
		}
		select {
		case <-ctx.Done():
			return ntfyMessage{}, false
		case <-ticker.C:
		}
	}
}
