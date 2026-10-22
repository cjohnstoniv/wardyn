// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"
)

// statusFile is the runner's last connection state, in its state directory, for `wardyn doctor`.
// serve is the only writer.
const statusFile = "status.json"

// runnerStatus is what statusFile holds: the link state, when it began, and the last error with its
// time. It holds no token, key or proxy credential: the error text is cleaned of any user name and
// password (cleanStatusError), and nothing else comes from the environment.
type runnerStatus struct {
	State       string     `json:"state"` // starting, connected, offline or stopped
	PID         int        `json:"pid"`
	Since       time.Time  `json:"since"`
	LastError   string     `json:"last_error,omitempty"`
	LastErrorAt *time.Time `json:"last_error_at,omitempty"`
	UpdatedAt   time.Time  `json:"updated_at"`
}

// statusWriter keeps statusFile current. A write that fails is logged once and never stops the runner.
type statusWriter struct {
	mu     sync.Mutex
	path   string
	now    func() time.Time
	cur    runnerStatus
	warned bool
}

func newStatusWriter(stateDir string) *statusWriter {
	w := &statusWriter{path: filepath.Join(stateDir, statusFile), now: time.Now}
	w.cur = runnerStatus{PID: os.Getpid()}
	w.set("starting")
	return w
}

// userinfo matches the user name and password of a URL inside an error message.
var userinfo = regexp.MustCompile(`(//)[^\s/@]*@`)

// cleanStatusError removes any user name and password from an error's text and bounds its length.
func cleanStatusError(s string) string {
	s = userinfo.ReplaceAllString(s, "$1")
	if len(s) > 500 {
		s = s[:500]
	}
	return s
}

func (w *statusWriter) set(state string) {
	now := w.now()
	if w.cur.State != state {
		w.cur.Since = now
	}
	w.cur.State, w.cur.UpdatedAt = state, now
	w.write()
}

// linkChanged is runnerwire.ClientConfig.LinkChanged.
func (w *statusWriter) linkChanged(up bool, reason string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if reason != "" {
		at := w.now()
		w.cur.LastError, w.cur.LastErrorAt = cleanStatusError(reason), &at
	}
	if up {
		w.set("connected")
	} else {
		w.set("offline")
	}
}

// stopped is written when serve returns.
func (w *statusWriter) stopped() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.set("stopped")
}

func (w *statusWriter) write() {
	b, err := json.Marshal(w.cur)
	if err == nil {
		err = writeAtomic(w.path, 0o600, func(f io.Writer) error { _, err := f.Write(b); return err })
	}
	if err != nil && !w.warned {
		w.warned = true
		slog.Warn("cannot write the runner status file; wardyn doctor will not see the connection state", "file", w.path, "err", err)
	}
}
