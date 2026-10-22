// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/federation"
	"github.com/cjohnstoniv/wardyn/internal/runneridentity"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// stageFixture is a status writer fed by an offline log, as serve wires them, in a state directory.
func stageFixture(t *testing.T, dir string, now *time.Time) (*statusWriter, *offlineLog) {
	t.Helper()
	l := newOfflineLog(dir, nil)
	w := newStatusWriter(dir)
	w.now, l.now = func() time.Time { return *now }, func() time.Time { return *now }
	l.reportTo(w)
	return w, l
}

func dnsFailure() error {
	return &net.OpError{Op: "dial", Net: "tcp", Err: &net.DNSError{Err: "no such host", Name: "org.example"}}
}

// A failed connect writes the doctor stage, its time and the scrubbed reason; a connect that succeeds
// clears the stage and keeps the last error.
func TestStatusStageWrittenOnAFailedConnectAndClearedOnSuccess(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC().Truncate(time.Second)
	w, l := stageFixture(t, dir, &now)
	if st, _ := readStatus(t, dir); st.Stage != "" || st.StageAt != nil {
		t.Fatalf("a fresh runner has a stage: %+v", st)
	}
	w.linkChanged(false, "dial tcp: lookup org.example: no such host")
	l.connectFailed(dnsFailure())
	st, raw := readStatus(t, dir)
	if st.State != "offline" || st.Stage != types.RunnerStageDNS || st.StageAt == nil || !st.StageAt.Equal(now) || st.StageAt.Location() != time.UTC {
		t.Fatalf("after a failed connect: %+v", st)
	}
	if !strings.Contains(raw, `"stage":"dns"`) || !strings.Contains(raw, `"stage_at":"`) {
		t.Errorf("the file lacks the fields: %s", raw)
	}
	now = now.Add(time.Minute)
	w.linkChanged(true, "")
	if st, _ = readStatus(t, dir); st.State != "connected" || st.Stage != "" || st.StageAt != nil || st.LastError == "" {
		t.Errorf("once connected the stage is gone and the last error stays: %+v", st)
	}
}

// A runner's own shutdown records no stage and clears the one a failed connect left.
func TestStatusStageShutdownRecordsNone(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC().Truncate(time.Second)
	w, l := stageFixture(t, dir, &now)
	w.linkChanged(false, context.Canceled.Error())
	if st, _ := readStatus(t, dir); st.Stage != "" {
		t.Fatalf("an attempt ended by shutdown has stage %q", st.Stage)
	}
	l.connectFailed(dnsFailure())
	w.stopped()
	if st, _ := readStatus(t, dir); st.State != "stopped" || st.Stage != "" || st.StageAt != nil {
		t.Errorf("after a clean shutdown: %+v", st)
	}
}

func identityIn(t *testing.T, state, org string) {
	t.Helper()
	key, err := runneridentity.Generate(state)
	if err != nil {
		t.Fatal(err)
	}
	id := runneridentity.Identity{RunnerID: uuid.New(), PrivateKey: key, Fingerprint: runnerwire.Fingerprint(key.Public().(ed25519.PublicKey)),
		OrgURLSHA256: federation.OrgURLSHA256(org), ProxyImage: "ghcr.io/cjohnstoniv/wardyn-proxy:0.10.0"}
	if err := runneridentity.Save(state, id); err != nil {
		t.Fatal(err)
	}
}

func serveOnce(t *testing.T, state string, buildErr error) {
	t.Helper()
	cmd := newServeCommand(func(context.Context, runneridentity.Identity, string, serveOptions) (runnerRuntime, error) {
		return runnerRuntime{}, buildErr
	})
	cmd.SetArgs([]string{"--org", "https://org.example", "--state-dir", state})
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	if err := cmd.Execute(); err == nil || !errors.Is(err, buildErr) {
		t.Fatalf("serve returned %v, want %v", err, buildErr)
	}
}

// A refusal at start puts its stage in status.json before serve exits, and the next start keeps it
// until a connect succeeds.
func TestStatusStageStartupRefusalKeptAcrossARestart(t *testing.T) {
	t.Setenv("WARDYN_PROXY_IMAGE", "")
	state := filepath.Join(t.TempDir(), "runner")
	identityIn(t, state, "https://org.example")
	serveOnce(t, state, atStage(types.RunnerStageEngine, errors.New("engine: no socket")))
	st, _ := readStatus(t, state)
	if st.State != "stopped" || st.Stage != types.RunnerStageEngine || st.StageAt == nil || !strings.Contains(st.LastError, "no socket") {
		t.Fatalf("after a refusal at start: %+v", st)
	}
	serveOnce(t, state, errors.New("stop before any connect"))
	st, _ = readStatus(t, state)
	if st.Stage != types.RunnerStageEngine {
		t.Fatalf("the next start dropped the stage before a connect: %+v", st)
	}
	// The start after that connects: the stage is gone.
	w := newStatusWriter(state)
	newOfflineLog(state, nil).reportTo(w)
	if st, _ = readStatus(t, state); st.Stage != types.RunnerStageEngine {
		t.Fatalf("a third start before a connect: %+v", st)
	}
	w.linkChanged(true, "")
	if st, _ = readStatus(t, state); st.Stage != "" || st.StageAt != nil {
		t.Errorf("a connect that succeeds keeps the stage: %+v", st)
	}
}

// A refusal at start survives a failed connect and a clean stop that follow it: the file at rest still
// shows the refusal's stage, and only a successful connect clears it.
func TestStatusStageRefusalKeptThroughAFailedConnectAndStop(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC().Truncate(time.Second)
	l := newOfflineLog(dir, nil)
	l.now = func() time.Time { return now }
	l.startFailed(atStage(types.RunnerStageEngine, errors.New("engine: no socket")))
	w, l := stageFixture(t, dir, &now)
	l.connectFailed(dnsFailure())
	if st, _ := readStatus(t, dir); st.Stage != types.RunnerStageDNS {
		t.Fatalf("after the failed connect: %+v", st)
	}
	w.stopped()
	st, _ := readStatus(t, dir)
	if st.State != "stopped" || st.Stage != types.RunnerStageEngine || !strings.Contains(st.LastError, "no socket") {
		t.Fatalf("at rest after the stop: %+v, want the refusal's stage", st)
	}
	w2, _ := stageFixture(t, dir, &now)
	w2.linkChanged(true, "")
	w2.stopped()
	if st, _ = readStatus(t, dir); st.Stage != "" || st.StageAt != nil {
		t.Errorf("a successful connect keeps the stage: %+v", st)
	}
}

// relayWatch's state change reaches status.json once per transition, not on every probe.
func TestStatusRelayWrittenOncePerTransition(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC().Truncate(time.Second)
	w, l := stageFixture(t, dir, &now)
	w.now = func() time.Time { return now }
	if st, _ := readStatus(t, dir); st.Relay != "" {
		t.Fatalf("a runner that probes no relay has relay %q", st.Relay)
	}
	down := false
	rw := newRelayWatch(func(context.Context) error {
		if down {
			return errors.New("engine cannot reach the relay")
		}
		return nil
	}, nil)
	rw.onChange = l.relayChanged
	l.relayChanged(types.RelayReachable)
	check := func(wantRelay string, wantChange bool) {
		t.Helper()
		before, _ := readStatus(t, dir)
		now = now.Add(time.Minute)
		if got := rw.check(t.Context()); got != wantChange {
			t.Fatalf("check changed=%v, want %v", got, wantChange)
		}
		after, _ := readStatus(t, dir)
		if after.Relay != wantRelay || after.UpdatedAt.Equal(before.UpdatedAt) == wantChange {
			t.Fatalf("relay %q (updated %v -> %v), want %q written=%v", after.Relay, before.UpdatedAt, after.UpdatedAt, wantRelay, wantChange)
		}
	}
	check(types.RelayReachable, false)
	down = true
	check(types.RelayUnreachable, true)
	check(types.RelayUnreachable, false)
	down = false
	check(types.RelayReachable, true)
}

// A failure whose text quotes a proxy password writes neither it nor a secret-named field.
func TestStatusStageHoldsNoSecret(t *testing.T) {
	const proxyURL = "http://alice:hunter2pw@proxy.corp.example:3128"
	dir := t.TempDir()
	l := newOfflineLog(dir, urlSecrets(proxyURL))
	w := newStatusWriter(dir)
	l.reportTo(w)
	noSecret := func(when string) {
		t.Helper()
		_, raw := readStatus(t, dir)
		for _, bad := range []string{"hunter2pw", "alice"} {
			if strings.Contains(raw, bad) {
				t.Errorf("%s: status.json holds %q:\n%s", when, bad, raw)
			}
		}
		for _, k := range []string{"token", "key", "password", "secret"} {
			if strings.Contains(strings.ToLower(raw), `"`+k) {
				t.Errorf("%s: status.json has a field named like %q:\n%s", when, k, raw)
			}
		}
	}
	l.connectFailed(fmt.Errorf("proxyconnect tcp: dial %s: refused; password hunter2pw", proxyURL))
	noSecret("a failed connect")
	l.startFailed(atStage(types.RunnerStageEngine, fmt.Errorf("engine via %s: no socket", proxyURL)))
	l.relayChanged(types.RelayUnreachable)
	noSecret("a refusal at start")
	if st, _ := readStatus(t, dir); st.Stage != types.RunnerStageEngine || st.Relay != types.RelayUnreachable {
		t.Errorf("status: %+v", st)
	}
	if fi, err := os.Stat(filepath.Join(dir, statusFile)); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("status.json mode: %v %v", fi, err)
	}
}
