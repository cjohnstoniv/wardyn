// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/doctor"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// relayReprobeInterval is how often a serving runner checks its run relay again, besides on every
// reconnect; relayProbeBound bounds one check. relayUnreachableWarning is the log line that names a
// relay that stopped answering.
const (
	relayReprobeInterval    = 5 * time.Minute
	relayProbeBound         = 15 * time.Second
	relayUnreachableWarning = "run relay unreachable"
)

// startupFailureFile, in the state directory, holds the last refusal of serve's start (engine, proxy
// image, relay): the process exits on one, so the next serve sends it in its offline summary.
const startupFailureFile = "startup-failure.json"

// stageError is a refusal of serve's start with the doctor stage it belongs to.
type stageError struct {
	stage string
	err   error
}

func (e *stageError) Error() string { return e.err.Error() }
func (e *stageError) Unwrap() error { return e.err }

func atStage(stage string, err error) error {
	if err == nil {
		return nil
	}
	return &stageError{stage, err}
}

// urlSecrets is the user name and password a URL carries, for doctor.Scrub.
func urlSecrets(raw string) []string {
	u, err := url.Parse(raw)
	if err != nil || u.User == nil {
		return nil
	}
	pw, _ := u.User.Password()
	return []string{u.User.Username(), pw}
}

// reasonText is err's text as an offline summary or relay status carries it: credentials removed
// (doctor.Scrub: any URL's userinfo, then each secret), one line, capped.
func reasonText(err error, secrets []string) string {
	return types.ClipRunnerReason(doctor.Scrub(err.Error(), secrets...))
}

// offlineLog keeps why the runner has not been connected since its last session: the first and last
// failure, the stage of the last one and its reason. It is sent in the next caps and cleared once the
// organisation acknowledged it. A refusal of serve's start is also kept in the state directory.
type offlineLog struct {
	path    string // the startup failure file; "" keeps nothing on disk
	secrets []string
	now     func() time.Time

	mu  sync.Mutex
	cur *types.RunnerOfflineSummary
}

// newOfflineLog starts from the refusal an earlier serve recorded, if any.
func newOfflineLog(stateDir string, secrets []string) *offlineLog {
	l := &offlineLog{path: filepath.Join(stateDir, startupFailureFile), secrets: secrets, now: time.Now}
	if b, err := os.ReadFile(l.path); err == nil && len(b) <= types.MaxRunnerOfflineSummaryBytes {
		var s types.RunnerOfflineSummary
		if json.Unmarshal(b, &s) == nil && s.Stage != "" {
			l.cur = &s
		}
	}
	return l
}

// secretsOrNil is the values reasons are scrubbed of; a nil log has none.
func (l *offlineLog) secretsOrNil() []string {
	if l == nil {
		return nil
	}
	return l.secrets
}

// connectFailed is the client's ConnectFailed hook.
func (l *offlineLog) connectFailed(err error) {
	l.fail(runnerwire.ConnectStage(err), err)
}

// startFailed records a refusal of serve's start and keeps it on disk; an error without a stage is
// not one the summary names.
func (l *offlineLog) startFailed(err error) {
	var se *stageError
	if !errors.As(err, &se) {
		return
	}
	s := l.fail(se.stage, err)
	if b, err := json.Marshal(s); err == nil && l.path != "" {
		if err := os.WriteFile(l.path, b, 0o600); err != nil {
			slog.Warn("runner: the startup refusal could not be kept for the organisation", "error", err)
		}
	}
}

func (l *offlineLog) fail(stage string, err error) types.RunnerOfflineSummary {
	now := l.now().UTC()
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.cur == nil {
		l.cur = &types.RunnerOfflineSummary{FirstFailureAt: now}
	}
	l.cur.Stage, l.cur.Reason, l.cur.LastFailureAt = stage, reasonText(err, l.secrets), now
	return *l.cur
}

// pending is the summary not yet acknowledged, or nil.
func (l *offlineLog) pending() *types.RunnerOfflineSummary {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.cur == nil {
		return nil
	}
	s := *l.cur
	return &s
}

// sent clears s once the organisation acknowledged it, unless a newer failure replaced it since.
func (l *offlineLog) sent(s *types.RunnerOfflineSummary) {
	if s == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.cur != nil && *l.cur == *s {
		l.cur = nil
		if l.path != "" {
			_ = os.Remove(l.path)
		}
	}
}

// relayWatch re-checks a Docker runner's run relay on every reconnect and every relayReprobeInterval.
// A relay that stops answering is warned about once, as relayUnreachableWarning, and its status goes
// with the next caps; the warning clears with a log line when it answers again.
type relayWatch struct {
	probe   func(context.Context) error
	secrets []string
	now     func() time.Time
	// ticker is time.NewTicker's channel and stop; tests replace it.
	ticker func(time.Duration) (<-chan time.Time, func())

	mu     sync.Mutex
	status types.RunnerRelayStatus
	// unsent is set by every state change and cleared only once caps carrying that state went out, so
	// a change made while the runner was offline still goes with a resumed session's caps.
	unsent bool
}

// newRelayWatch starts reachable: serve's start found the relay with a probe container.
func newRelayWatch(probe func(context.Context) error, secrets []string) *relayWatch {
	return &relayWatch{probe: probe, secrets: secrets, now: time.Now,
		ticker: func(d time.Duration) (<-chan time.Time, func()) {
			t := time.NewTicker(d)
			return t.C, t.Stop
		},
		status: types.RunnerRelayStatus{State: types.RelayReachable, Since: time.Now().UTC()}}
}

// check probes once and says whether the state changed.
func (w *relayWatch) check(ctx context.Context) bool {
	ctx, cancel := context.WithTimeout(ctx, relayProbeBound)
	defer cancel()
	err := w.probe(ctx)
	state, reason := types.RelayReachable, ""
	if err != nil {
		state, reason = types.RelayUnreachable, reasonText(err, w.secrets)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.status.State == state {
		return false
	}
	w.status = types.RunnerRelayStatus{State: state, Since: w.now().UTC(), Reason: reason}
	w.unsent = true
	if err != nil {
		slog.WarnContext(ctx, relayUnreachableWarning, "warning", "relay_unreachable", "error", reason)
	} else {
		slog.InfoContext(ctx, "run relay reachable again", "warning_cleared", "relay_unreachable")
	}
	return true
}

func (w *relayWatch) current() *types.RunnerRelayStatus {
	w.mu.Lock()
	defer w.mu.Unlock()
	s := w.status
	return &s
}

// pending is the current status and whether the organisation has yet to receive it.
func (w *relayWatch) pending() (*types.RunnerRelayStatus, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	s := w.status
	return &s, w.unsent
}

// sent clears unsent once caps carrying s went out, unless the state changed again since.
func (w *relayWatch) sent(s *types.RunnerRelayStatus) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if s != nil && *s == w.status {
		w.unsent = false
	}
}

// run checks every relayReprobeInterval until ctx ends, and calls changed after a check that changed
// the state.
func (w *relayWatch) run(ctx context.Context, changed func()) {
	tick, stop := w.ticker(relayReprobeInterval)
	defer stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick:
		}
		if w.check(ctx) {
			changed()
		}
	}
}

// connectReport is what a runner's caps carry beyond its substrate's own: the offline summary and the
// relay status. Either part may be nil (a runner with no relay to check).
type connectReport struct {
	log   *offlineLog
	relay *relayWatch
}

// fill adds both parts to caps, re-probing the relay first when reprobe (every reconnect does). It says
// whether a relay state the organisation has not received or a summary waits, so a resumed session
// knows to send caps again.
func (r connectReport) fill(ctx context.Context, caps *runnerwire.Caps, reprobe bool) bool {
	news := false
	if r.relay != nil {
		if reprobe {
			r.relay.check(ctx)
		}
		caps.Relay, news = r.relay.pending()
	}
	if r.log != nil {
		caps.OfflineSummary = r.log.pending()
		news = news || caps.OfflineSummary != nil
	}
	return news
}

// sent is called once the organisation acknowledged caps.
func (r connectReport) sent(caps runnerwire.Caps) {
	if r.log != nil {
		r.log.sent(caps.OfflineSummary)
	}
	if r.relay != nil {
		r.relay.sent(caps.Relay)
	}
}
