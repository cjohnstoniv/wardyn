// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
)

// statusFile is the runner's last connection state, in its state directory, for `wardyn doctor`.
// serve is the only writer.
const statusFile = "status.json"

// runnerStatus is what statusFile holds: the link state, when it began, the last error with its
// time, and the stage the last failure stopped at (a runnerwire.Stage) with its time. It holds no token, key or proxy credential: the error text is cleaned of any user name and
// password (cleanStatusError), and nothing else comes from the environment.
type runnerStatus struct {
	State       string     `json:"state"` // starting, connected, offline or stopped
	PID         int        `json:"pid"`
	Since       time.Time  `json:"since"`
	LastError   string     `json:"last_error,omitempty"`
	LastErrorAt *time.Time `json:"last_error_at,omitempty"`
	// Stage and StageAt are where the last failure stopped (connecting, starting, or the run relay)
	// and when. They outlive a restart, so a refusal at startup reaches the next connect.
	Stage   runnerwire.Stage `json:"stage,omitempty"`
	StageAt *time.Time       `json:"stage_at,omitempty"`
	// Relay is "reachable" or "unreachable": whether the engine reached the run relay at the last
	// check. Empty on a substrate with no local relay.
	Relay     string    `json:"relay,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Values of runnerStatus.Relay.
const (
	relayReachable   = "reachable"
	relayUnreachable = "unreachable"
)

// startupError is a refusal at startup that stopped at stage.
type startupError struct {
	stage runnerwire.Stage
	err   error
}

func (e *startupError) Error() string { return e.err.Error() }
func (e *startupError) Unwrap() error { return e.err }

// atStage records stage on err, keeping its text and chain.
func atStage(stage runnerwire.Stage, err error) error {
	if err == nil {
		return nil
	}
	return &startupError{stage: stage, err: err}
}

// statusWriter keeps statusFile current. A write that fails is logged once and never stops the runner.
type statusWriter struct {
	mu     sync.Mutex
	path   string
	now    func() time.Time
	cur    runnerStatus
	warned bool
	// relayLostAt is when the run relay was lost, while Relay is unreachable. It is separate from
	// StageAt, which a later connect failure overwrites.
	relayLostAt time.Time
}

func newStatusWriter(stateDir string) *statusWriter {
	w := &statusWriter{path: filepath.Join(stateDir, statusFile), now: time.Now}
	w.cur = runnerStatus{PID: os.Getpid()}
	w.carryStage()
	w.set("starting")
	return w
}

// carryStage keeps the last failure's stage and time from the file an earlier run of serve left, so a
// startup refusal is still reported once a later start connects. Anything else in the file is dropped.
func (w *statusWriter) carryStage() {
	b, err := os.ReadFile(w.path)
	if err != nil {
		return
	}
	var prev runnerStatus
	if json.Unmarshal(b, &prev) != nil || !prev.Stage.Valid() || prev.StageAt == nil {
		return
	}
	w.cur.Stage, w.cur.StageAt = prev.Stage, prev.StageAt
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
func (w *statusWriter) linkChanged(up bool, reason string, stage runnerwire.Stage) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if reason != "" {
		at := w.now()
		w.cur.LastError, w.cur.LastErrorAt = cleanStatusError(reason), &at
		if stage.Valid() {
			w.cur.Stage, w.cur.StageAt = stage, &at
		}
	}
	if up {
		w.set("connected")
	} else {
		w.set("offline")
	}
}

// startupFailed records the stage a refusal at startup stopped at, when err names one.
func (w *statusWriter) startupFailed(err error) {
	var se *startupError
	if w == nil || !errors.As(err, &se) {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	at := w.now()
	w.cur.LastError, w.cur.LastErrorAt = cleanStatusError(err.Error()), &at
	w.cur.Stage, w.cur.StageAt = se.stage, &at
	w.write()
}

// relayChanged records whether the engine reaches the run relay. Losing it is a failure at the relay
// stage; regaining it keeps that stage's record and says it is over.
func (w *statusWriter) relayChanged(reachable bool, reason string) {
	if w == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if reachable {
		w.cur.Relay = relayReachable
	} else {
		at := w.now()
		if w.cur.Relay != relayUnreachable {
			w.relayLostAt = at
		}
		w.cur.Relay = relayUnreachable
		w.cur.LastError, w.cur.LastErrorAt = cleanStatusError(reason), &at
		w.cur.Stage, w.cur.StageAt = runnerwire.StageRelay, &at
	}
	w.cur.UpdatedAt = w.now()
	w.write()
}

// failure is the last failure the caps event reports to the organisation, nil when there is none. The
// relay is the one failure that can be going on now: while it is unreachable that is what is reported,
// whatever connect failure came after it.
func (w *statusWriter) failure() *runnerwire.Failure {
	if w == nil {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.cur.Relay == relayUnreachable {
		return &runnerwire.Failure{Stage: runnerwire.StageRelay, At: w.relayLostAt, Ongoing: true}
	}
	if !w.cur.Stage.Valid() || w.cur.StageAt == nil {
		return nil
	}
	return &runnerwire.Failure{Stage: w.cur.Stage, At: *w.cur.StageAt}
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
