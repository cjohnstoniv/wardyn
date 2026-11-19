// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// capturePause records each pause instead of sleeping, so the backoff is read, not waited for.
type capturePause struct{ got []time.Duration }

func (c *capturePause) pause(_ context.Context, d time.Duration) bool {
	c.got = append(c.got, d)
	return true
}

// logSink captures slog output for the test and restores the default.
func logSink(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })
	return &buf
}

// TestWaitForClient_WaitsForTheCA: the CA appears after N attempts (wardynd patches it into the Secret
// after the sensor starts). The ingest keeps trying, backs off up to a cap, logs the wait once, and
// continues when the CA is there.
func TestWaitForClient_WaitsForTheCA(t *testing.T) {
	logs := logSink(t)
	const missing = 7
	calls := 0
	build := func() (*http.Client, error) {
		calls++
		if calls <= missing {
			return nil, fmt.Errorf("%w: /ca does not exist", errCANotReady)
		}
		return &http.Client{}, nil
	}
	p := &capturePause{}
	c, err := waitForClient(context.Background(), build, 10*time.Minute, p.pause)
	if err != nil || c == nil {
		t.Fatalf("want a client once the CA is present, got %v, %v", c, err)
	}
	if calls != missing+1 || len(p.got) != missing {
		t.Fatalf("calls=%d pauses=%d, want %d calls and %d pauses", calls, len(p.got), missing+1, missing)
	}
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 15 * time.Second, 15 * time.Second, 15 * time.Second}
	for i, d := range want {
		if p.got[i] != d {
			t.Fatalf("pause %d = %s, want %s (doubling, capped at %s)", i, p.got[i], d, caWaitMax)
		}
	}
	if n := strings.Count(logs.String(), "waiting for wardynd to publish"); n != 1 {
		t.Fatalf("the wait was logged %d times, want once:\n%s", n, logs.String())
	}
	if n := strings.Count(logs.String(), "CA present"); n != 1 {
		t.Fatalf("the end of the wait was logged %d times, want once", n)
	}
}

// TestWaitForClient_OtherErrorsFailAtOnce: a bad CA, a bad URL or an unreadable file is a real error.
func TestWaitForClient_OtherErrorsFailAtOnce(t *testing.T) {
	calls := 0
	boom := errors.New("control-plane CA has no certificate")
	p := &capturePause{}
	_, err := waitForClient(context.Background(), func() (*http.Client, error) { calls++; return nil, boom }, time.Minute, p.pause)
	if !errors.Is(err, boom) || calls != 1 || len(p.got) != 0 {
		t.Fatalf("err=%v calls=%d pauses=%d, want the error from the first attempt and no pause", err, calls, len(p.got))
	}
}

// TestWaitForClient_GivesUpAfterTheBound: a Secret nothing ever fills is a crash, not a hang. It runs
// with the production bound and records the pauses instead of sleeping them.
func TestWaitForClient_GivesUpAfterTheBound(t *testing.T) {
	if caWaitBound != 10*time.Minute {
		t.Fatalf("caWaitBound = %s, want 10m", caWaitBound)
	}
	calls := 0
	build := func() (*http.Client, error) { calls++; return nil, fmt.Errorf("%w: /ca is empty", errCANotReady) }
	p := &capturePause{}
	_, err := waitForClient(context.Background(), build, caWaitBound, p.pause)
	if !errors.Is(err, errCANotReady) {
		t.Fatalf("want errCANotReady after the bound, got %v", err)
	}
	var total time.Duration
	for _, d := range p.got {
		total += d
	}
	if total != 600*time.Second || calls != len(p.got)+1 {
		t.Fatalf("paused %s over %d pauses and %d calls, want 600s", total, len(p.got), calls)
	}
}

// TestWaitForClient_StopsWhenTheProcessDoes: a SIGTERM during the wait ends it.
func TestWaitForClient_StopsWhenTheProcessDoes(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := waitForClient(ctx, func() (*http.Client, error) { return nil, errCANotReady }, time.Minute, waitPause)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
}

// TestControlPlaneClient_MissingOrEmptyCAIsNotReady: only those two states are retried, and a file that
// appears is accepted.
func TestControlPlaneClient_MissingOrEmptyCAIsNotReady(t *testing.T) {
	path := filepath.Join(t.TempDir(), "control-plane-ca.pem")
	if _, err := controlPlaneClient("https://wardynd:8443", path); !errors.Is(err, errCANotReady) {
		t.Fatalf("missing file: want errCANotReady, got %v", err)
	}
	if err := os.WriteFile(path, []byte("\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := controlPlaneClient("https://wardynd:8443", path); !errors.Is(err, errCANotReady) {
		t.Fatalf("empty file: want errCANotReady, got %v", err)
	}
	if err := os.WriteFile(path, []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := controlPlaneClient("https://wardynd:8443", path); err == nil || errors.Is(err, errCANotReady) {
		t.Fatalf("a file with no certificate is a real error, got %v", err)
	}
	if err := os.WriteFile(path, caFileBytes(t), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := controlPlaneClient("https://wardynd:8443", path); err != nil {
		t.Fatalf("a valid CA must be accepted: %v", err)
	}
}

func caFileBytes(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile(caFileFor(t, newCA(t)))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestStoppedDuringWait: a SIGTERM during the CA wait is a quiet shutdown (run returns nil, exit 0);
// giving up at the bound or any real error is still a refusal to start.
func TestStoppedDuringWait(t *testing.T) {
	stopped, cancel := context.WithCancel(context.Background())
	cancel()
	notReady := func() (*http.Client, error) { return nil, errCANotReady }

	_, err := waitForClient(stopped, notReady, time.Minute, waitPause)
	if !stoppedDuringWait(stopped, err) {
		t.Fatalf("a cancelled wait must read as a shutdown, got err=%v", err)
	}

	live := context.Background()
	p := &capturePause{}
	_, err = waitForClient(live, notReady, time.Minute, p.pause)
	if err == nil || stoppedDuringWait(live, err) {
		t.Fatalf("giving up at the bound must stay a failure, got %v", err)
	}
	if stoppedDuringWait(live, errors.New("control-plane CA has no certificate")) {
		t.Fatal("a real error must stay a failure")
	}
	if stoppedDuringWait(live, nil) {
		t.Fatal("no error is not a shutdown")
	}
}
