// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/federation"
	"github.com/cjohnstoniv/wardyn/internal/runneridentity"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
)

func serveFixture(t *testing.T) (org, stateDir string) {
	t.Helper()
	org = "https://org.example"
	stateDir = filepath.Join(t.TempDir(), "runner")
	key, err := runneridentity.Generate(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	identity := runneridentity.Identity{RunnerID: uuid.New(), PrivateKey: key, Fingerprint: runnerwire.Fingerprint(key.Public().(ed25519.PublicKey)), OrgURLSHA256: federation.OrgURLSHA256(org)}
	if err := runneridentity.Save(stateDir, identity); err != nil {
		t.Fatal(err)
	}
	return org, stateDir
}

func serveRefusing(t *testing.T, org, stateDir string, refusal error, extra ...string) {
	t.Helper()
	cmd := newServeCommand(func(context.Context, runneridentity.Identity, string, serveOptions) (runnerRuntime, error) {
		return runnerRuntime{}, refusal
	})
	cmd.SetArgs(append([]string{"--org", org, "--state-dir", stateDir}, extra...))
	if err := cmd.Execute(); err == nil {
		t.Fatal("serve started")
	}
}

// A refusal at startup (engine, image, relay, CA) is on record in status.json with its stage, and a
// restart keeps it, so the next successful connect can report why the runner was down. An error that
// names no stage leaves the record alone.
func TestStartupRefusalIsRecordedAndSurvivesARestart(t *testing.T) {
	org, dir := serveFixture(t)
	serveRefusing(t, org, dir, atStage(runnerwire.StageEngine, errors.New("docker engine not reachable")))
	st, _ := readStatus(t, dir)
	if st.Stage != runnerwire.StageEngine || st.StageAt == nil || !strings.Contains(st.LastError, "docker engine") || st.State != "stopped" {
		t.Fatalf("after the refusal: %+v", st)
	}
	serveRefusing(t, org, dir, errors.New("something with no stage"))
	st, _ = readStatus(t, dir)
	if st.Stage != runnerwire.StageEngine {
		t.Errorf("a restart lost the earlier stage: %+v", st)
	}
	if f := newStatusWriter(dir).failure(); f == nil || f.Stage != runnerwire.StageEngine || f.Ongoing {
		t.Errorf("failure reported to the organisation = %+v, want the engine stage, over", f)
	}
}

func TestCAFileRefusalNamesTheCAStage(t *testing.T) {
	for _, flag := range []string{"--org-ca-file", "--trusted-ca-file"} {
		t.Run(flag, func(t *testing.T) {
			org, dir := serveFixture(t)
			bad := filepath.Join(t.TempDir(), "ca.pem")
			if err := os.WriteFile(bad, []byte("not a certificate"), 0o600); err != nil {
				t.Fatal(err)
			}
			serveRefusing(t, org, dir, errors.New("never reached"), flag, bad)
			if st, _ := readStatus(t, dir); st.Stage != runnerwire.StageCA {
				t.Errorf("stage = %q, want ca: %+v", st.Stage, st)
			}
		})
	}
}

// A failed connect attempt's stage reaches status.json beside its error, and an attempt that carries no
// stage (a clean end) leaves the earlier one.
func TestStatusFileRecordsTheConnectStageAndNoText(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC().Truncate(time.Second)
	w := newStatusWriter(dir)
	w.now = func() time.Time { return now }
	w.linkChanged(false, `dial https://user:Pw-secret-31@proxy.corp:3128: refused`, runnerwire.StageProxy)
	w.linkChanged(true, "", "")
	st, raw := readStatus(t, dir)
	if st.Stage != runnerwire.StageProxy || st.StageAt == nil || !st.StageAt.Equal(now) {
		t.Errorf("stage = %q at %v, want proxy at %v", st.Stage, st.StageAt, now)
	}
	if strings.Contains(raw, "Pw-secret-31") {
		t.Errorf("status.json holds a credential:\n%s", raw)
	}
	w.linkChanged(false, "bad", "not a stage")
	if st, _ = readStatus(t, dir); st.Stage != runnerwire.StageProxy {
		t.Errorf("a free-form stage was recorded: %q", st.Stage)
	}
}

// The relay's loss is a failure still going on; regaining it ends that, and the record stays.
func TestRelayLossIsOngoingUntilRegained(t *testing.T) {
	var w *statusWriter
	w.relayChanged(false, "x") // a nil writer is a no-op
	if w.failure() != nil {
		t.Fatal("a nil writer reports a failure")
	}
	dir := t.TempDir()
	w = newStatusWriter(dir)
	if w.failure() != nil {
		t.Fatalf("a fresh runner reports %+v", w.failure())
	}
	w.relayChanged(true, "")
	if w.failure() != nil {
		t.Fatalf("a reachable relay reports %+v", w.failure())
	}
	w.relayChanged(false, "a container could not reach 172.17.0.1")
	if f := w.failure(); f == nil || f.Stage != runnerwire.StageRelay || !f.Ongoing {
		t.Fatalf("lost relay reports %+v", f)
	}
	if st, _ := readStatus(t, dir); st.Relay != relayUnreachable || st.Stage != runnerwire.StageRelay {
		t.Errorf("status.json: %+v", st)
	}
	w.relayChanged(true, "")
	if f := w.failure(); f == nil || f.Stage != runnerwire.StageRelay || f.Ongoing {
		t.Fatalf("regained relay reports %+v, want the relay record, over", f)
	}
}

var errRelayDown = errors.New("relay unreachable")

func watchFor(check func(context.Context) error) relayWatch {
	return relayWatch{check: check, proves: func(err error) bool { return errors.Is(err, errRelayDown) },
		changed: func(context.Context, bool, error) {}, send: func(context.Context) error { return nil }}
}

// The watch records a relay that is lost once, says nothing while it stays lost, records it again when
// it is regained; a check cut short by shutdown is not a loss.
func TestRelayWatchReportsEachChangeOnce(t *testing.T) {
	var next error
	var got []bool
	w := watchFor(func(context.Context) error { return next })
	w.changed = func(_ context.Context, reachable bool, err error) {
		if reachable == (err != nil) {
			t.Errorf("reachable=%v with err=%v", reachable, err)
		}
		got = append(got, reachable)
	}
	ctx := t.Context()
	for _, step := range []struct {
		err  error
		want []bool
	}{{nil, nil}, {errRelayDown, []bool{false}}, {errRelayDown, []bool{false}}, {nil, []bool{false, true}}, {nil, []bool{false, true}}} {
		next = step.err
		w.once(ctx)
		if len(got) != len(step.want) {
			t.Fatalf("after check err=%v: events %v, want %v", step.err, got, step.want)
		}
	}
	gone, cancel := context.WithCancel(ctx)
	cancel()
	next = context.Canceled
	w.once(gone)
	if len(got) != 2 {
		t.Errorf("a check cut short by shutdown was reported: %v", got)
	}
}

// Only an error that proves the relay down marks it lost; a probe that could not run leaves the state as
// it was, whichever way it was.
func TestRelayWatchOnlyAProvenLossMarksItLost(t *testing.T) {
	var next error
	changes := 0
	w := watchFor(func(context.Context) error { return next })
	w.changed = func(context.Context, bool, error) { changes++ }
	ctx := t.Context()
	couldNotRun := errors.New("docker: probe relay: proxy image is not present locally")
	next = couldNotRun
	w.once(ctx)
	if w.lost || changes != 0 {
		t.Fatalf("a probe that could not run marked the relay lost: lost=%v changes=%d", w.lost, changes)
	}
	next = fmt.Errorf("%w (3 attempts)", errRelayDown)
	w.once(ctx)
	if !w.lost || changes != 1 {
		t.Fatalf("a proven loss: lost=%v changes=%d", w.lost, changes)
	}
	next = couldNotRun
	w.once(ctx)
	if !w.lost || changes != 1 {
		t.Errorf("a probe that could not run changed a lost relay: lost=%v changes=%d", w.lost, changes)
	}
}

// A report that fails stays pending and is sent at a later check whose state is unchanged, once; no
// change is recorded twice.
func TestRelayWatchRetriesAReportThatFailed(t *testing.T) {
	var next = errRelayDown
	changes, sends := 0, 0
	sendErr := errors.New("session ended")
	w := watchFor(func(context.Context) error { return next })
	w.changed = func(context.Context, bool, error) { changes++ }
	w.send = func(context.Context) error { sends++; return sendErr }
	ctx := t.Context()
	w.once(ctx)
	w.once(ctx)
	if changes != 1 || sends != 2 || !w.pending {
		t.Fatalf("while the send fails: changes=%d sends=%d pending=%v", changes, sends, w.pending)
	}
	sendErr = nil
	w.once(ctx)
	w.once(ctx)
	if changes != 1 || sends != 3 || w.pending {
		t.Errorf("after it succeeds: changes=%d sends=%d pending=%v, want 1, 3, false", changes, sends, w.pending)
	}
}

// A session coming up (kick) and the interval each run a check; nothing runs before either.
func TestRelayWatchChecksOnKickAndOnTheInterval(t *testing.T) {
	checks := make(chan struct{}, 8)
	w := watchFor(func(context.Context) error { checks <- struct{}{}; return nil })
	kick := make(chan struct{}, 1)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	w.every = time.Hour
	go w.run(ctx, kick)
	select {
	case <-checks:
		t.Fatal("a check ran before any kick or interval")
	case <-time.After(50 * time.Millisecond):
	}
	kick <- struct{}{}
	select {
	case <-checks:
	case <-time.After(5 * time.Second):
		t.Fatal("a session coming up did not re-check the relay")
	}
	cancel()
	w2 := watchFor(func(context.Context) error { checks <- struct{}{}; return nil })
	w2.every = 10 * time.Millisecond
	ctx2, cancel2 := context.WithCancel(t.Context())
	defer cancel2()
	go w2.run(ctx2, nil)
	select {
	case <-checks:
	case <-time.After(5 * time.Second):
		t.Fatal("the interval did not re-check the relay")
	}
}

// A session coming up tells the relay watch, without blocking the session hook.
func TestRelayOnlineKicksTheWatch(t *testing.T) {
	r := &localRelay{sessionUp: make(chan struct{}, 1)}
	r.Online(nil)
	r.Online(nil) // a second one with the first unread must not block
	select {
	case <-r.sessionUp:
	default:
		t.Fatal("Online did not tell the relay watch")
	}
}

// watchRelay runs the watch beside the runner's own loop and stops it with that loop's context.
func TestWatchRelayRunsBesideTheLoop(t *testing.T) {
	checked := make(chan struct{}, 1)
	w := watchFor(func(context.Context) error { checked <- struct{}{}; return nil })
	w.every = time.Hour
	kick := make(chan struct{}, 1)
	ctx, cancel := context.WithCancel(t.Context())
	loop := watchRelay(func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }, w, kick)
	done := make(chan error, 1)
	go func() { done <- loop(ctx) }()
	kick <- struct{}{}
	select {
	case <-checked:
	case <-time.After(5 * time.Second):
		t.Fatal("the watch never ran beside the loop")
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Errorf("loop = %v", err)
	}
}
