// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k6_test

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// fakeKubectl puts a stand-in kubectl first on PATH. Each call appends a line to the returned count file
// and then runs body.
func fakeKubectl(t *testing.T, body string) (calls func() int) {
	t.Helper()
	dir := t.TempDir()
	count := filepath.Join(dir, "calls")
	script := "#!/bin/sh\necho x >> " + count + "\n" + body + "\n"
	if err := os.WriteFile(filepath.Join(dir, "kubectl"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return func() int {
		b, _ := os.ReadFile(count)
		return strings.Count(string(b), "x\n")
	}
}

// followWait polls cond for up to 10 s.
func followWait(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); !cond(); time.Sleep(50 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("never saw: %s", what)
		}
	}
}

// A pod that reads NotFound ends the follow with one line saying by when, and the follower is gone: it
// neither re-follows nor logs NotFound every 2 s, and it leaves no goroutine.
func TestFollowProxyLogStopsAtNotFound(t *testing.T) {
	calls := fakeKubectl(t, "echo 'Error from server (NotFound): pods \"wardyn-proxy-r1\" not found' >&2; exit 1")
	base := runtime.NumGoroutine()
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	l := (&orgEnv{adminKC: "kc", runsNS: "ns"}).followProxyLog(ctx, "r1")
	followWait(t, "the pod-gone line", func() bool { return strings.Contains(l.String(), "k6: proxy pod gone by") })
	followWait(t, "the follower to exit", func() bool { return runtime.NumGoroutine() <= base })
	time.Sleep(3 * time.Second) // longer than the 2 s retry pause, which a missing stop would use
	if got := l.String(); strings.Count(got, "k6: proxy pod gone by") != 1 || strings.Contains(got, "NotFound") || calls() != 1 {
		t.Fatalf("want one pod-gone line, one kubectl call and no NotFound, got %q after %d calls", got, calls())
	}
}

// A cancelled context stops the follower without the pod-gone line.
func TestFollowProxyLogStopsOnCancel(t *testing.T) {
	calls := fakeKubectl(t, "exec sleep 60")
	base := runtime.NumGoroutine()
	ctx, stop := context.WithCancel(context.Background())
	l := (&orgEnv{adminKC: "kc", runsNS: "ns"}).followProxyLog(ctx, "r1")
	followWait(t, "the first follow", func() bool { return calls() == 1 })
	stop()
	followWait(t, "the follower to exit", func() bool { return runtime.NumGoroutine() <= base })
	if got := l.String(); got != "" || calls() != 1 {
		t.Fatalf("want no output and no second follow after cancel, got %q after %d calls", got, calls())
	}
}

// Any error that is not NotFound is logged, and the follow is tried again after a pause.
func TestFollowProxyLogRetriesOtherErrors(t *testing.T) {
	calls := fakeKubectl(t, "echo 'Unable to connect to the server' >&2; exit 1")
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	l := (&orgEnv{adminKC: "kc", runsNS: "ns"}).followProxyLog(ctx, "r1")
	followWait(t, "a second follow", func() bool { return calls() >= 2 })
	if got := l.String(); !strings.Contains(got, "Unable to connect") || strings.Contains(got, "proxy pod gone") {
		t.Fatalf("want the error kept and no pod-gone line, got %q", got)
	}
}
