// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"fmt"
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

// runnerStatus is what statusFile holds: the link state, when it began, the last error with its
// time, the stage the last failed connect or start stopped at, whether the run relay answers and the
// proxy image, if any, that refused peers-only. It holds no token, key or proxy credential: the error
// text is cleaned of any user name and password (cleanStatusError), a failure's reason is scrubbed as
// the offline summary's is, and nothing else comes from the environment but a pinned proxy image's name.
type runnerStatus struct {
	State       string     `json:"state"` // starting, connected, offline or stopped
	PID         int        `json:"pid"`
	Since       time.Time  `json:"since"`
	LastError   string     `json:"last_error,omitempty"`
	LastErrorAt *time.Time `json:"last_error_at,omitempty"`
	// Stage is the doctor stage (types.RunnerStage*) of the last failed connect or start, and StageAt
	// when. Both are empty once connected, and after a clean shutdown unless a refusal at start set them.
	Stage   string     `json:"stage,omitempty"`
	StageAt *time.Time `json:"stage_at,omitempty"`
	// Relay is types.RelayReachable or types.RelayUnreachable; empty when no relay is probed.
	Relay     string    `json:"relay,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
	// PeersOnlyRefusedBy is the organisation's proxy image that refused allowed_peers at the last
	// proxy start, so run proxies serve any peer (WARDYN_DOCKER_PROXY_SANDBOX_PEERS_ONLY on only by
	// default); empty once one takes them.
	PeersOnlyRefusedBy string `json:"peers_only_refused_by,omitempty"`
}

// statusPath is statusFile in stateDir.
func statusPath(stateDir string) string { return filepath.Join(stateDir, statusFile) }

// loadStatus is what the service last wrote to path, for doctor. It reads only.
func loadStatus(path string) (runnerStatus, error) {
	var st runnerStatus
	b, err := os.ReadFile(path)
	if err != nil {
		return st, err
	}
	if err := json.Unmarshal(b, &st); err != nil {
		return st, fmt.Errorf("%s is not readable as the runner's status: %w", path, err)
	}
	return st, nil
}

// statusWriter keeps statusFile current. A write that fails is logged once and never stops the runner.
type statusWriter struct {
	mu     sync.Mutex
	path   string
	now    func() time.Time
	cur    runnerStatus
	warned bool
	// refused is the last refusal at start, kept until a connect succeeds: the process exits on one, so
	// its own shutdown, and a failed connect before it, must not lose what the next start shows.
	refused *runnerStatus
}

func newStatusWriter(stateDir string) *statusWriter {
	w := &statusWriter{path: statusPath(stateDir), now: time.Now}
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
		w.cur.Stage, w.cur.StageAt, w.refused = "", nil, nil
		w.set("connected")
	} else {
		w.set("offline")
	}
}

// failed records the stage a connect or a start stopped at, with its scrubbed reason (the offline
// summary's), at the time the summary dates it. A refusal at start (refusal) outlives a clean shutdown.
// A nil writer records nothing.
func (w *statusWriter) failed(stage, reason string, at time.Time, refusal bool) {
	if w == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	at = at.UTC()
	w.cur.LastError, w.cur.LastErrorAt = reason, &at
	w.cur.Stage, w.cur.StageAt = stage, &at
	if refusal {
		w.refused = &runnerStatus{LastError: reason, LastErrorAt: &at, Stage: stage, StageAt: &at}
	}
	w.cur.UpdatedAt = w.now()
	w.write()
}

// startError records a start that failed with no stage (a bad flag): the error is the last one, and is kept
// through the stop that follows like a refusal. It replaces any earlier refusal, stage included: the stage's
// fix would not mend this error. A nil writer records nothing.
func (w *statusWriter) startError(reason string, at time.Time) {
	if w == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	at = at.UTC()
	w.cur.LastError, w.cur.LastErrorAt, w.cur.Stage, w.cur.StageAt = reason, &at, "", nil
	w.refused = &runnerStatus{LastError: reason, LastErrorAt: &at}
	w.cur.UpdatedAt = w.now()
	w.write()
}

// relayChanged records whether the run relay answers (types.RelayReachable or types.RelayUnreachable).
// A nil writer records nothing.
func (w *statusWriter) relayChanged(state string) {
	if w == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.cur.Relay, w.cur.UpdatedAt = state, w.now()
	w.write()
}

// peersOnlyChanged records the proxy image whose runs serve any peer because it refused allowed_peers,
// "" once one takes them. A nil writer records nothing.
func (w *statusWriter) peersOnlyChanged(image string) {
	if w == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.cur.PeersOnlyRefusedBy, w.cur.UpdatedAt = image, w.now()
	w.write()
}

// stopped is written when serve returns. A runner's own shutdown records no stage and clears the
// one a failed connect left; a refusal at start stays, and is what the file shows at rest.
func (w *statusWriter) stopped() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.cur.Stage, w.cur.StageAt = "", nil
	if r := w.refused; r != nil {
		w.cur.LastError, w.cur.LastErrorAt, w.cur.Stage, w.cur.StageAt = r.LastError, r.LastErrorAt, r.Stage, r.StageAt
	}
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
