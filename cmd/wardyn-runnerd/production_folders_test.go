// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/runneridentity"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
)

// A folders runner that resumes a session skips Replay, so a failure recorded while it was down must go with
// the Online hook; a fresh session sent it from Replay already and Online sends nothing more.
func TestFoldersRunnerSendsItsDiagnosticOnResume(t *testing.T) {
	state := t.TempDir()
	log := newOfflineLog(state, nil)
	rt, err := foldersRunnerHooks(t.Context(), runneridentity.Identity{}, state, serveOptions{folderRoots: []string{t.TempDir()}, offline: log})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rt.close() })

	orgConn, runConn := runnerwire.Loopback()
	got := make(chan runnerwire.Caps, 4)
	org := runnerwire.NewPeer(orgConn, runnerwire.PeerConfig{Org: true, OnEventChecked: func(ev runnerwire.Event) error {
		var caps runnerwire.Caps
		if ev.Kind == runnerwire.EventCaps && runnerwire.Unmarshal(ev.Data, &caps) == nil {
			got <- caps
		}
		return nil
	}})
	run := runnerwire.NewPeer(runConn, runnerwire.PeerConfig{})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() { _ = org.Run(ctx) }()
	go func() { _ = run.Run(ctx) }()
	t.Cleanup(func() { org.Close(); run.Close() })

	log.connectFailed(errors.New("dial tcp: connection refused"))
	rt.hooks.Online(run, false)
	select {
	case caps := <-got:
		t.Fatalf("a fresh session sent caps from Online: %+v", caps)
	case <-time.After(200 * time.Millisecond):
	}
	rt.hooks.Online(run, true)
	select {
	case caps := <-got:
		if caps.OfflineSummary == nil || !caps.Folders {
			t.Fatalf("resumed caps = %+v, want the folders caps with the recorded diagnostic", caps)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a resumed folders runner never sent the diagnostic recorded while it was down")
	}
	for end := time.Now().Add(5 * time.Second); log.pending() != nil; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(end) {
			t.Fatal("the diagnostic stays pending after the organisation acknowledged it")
		}
	}
	rt.hooks.Online(run, true)
	select {
	case caps := <-got:
		t.Fatalf("a resume with nothing owed sent caps: %+v", caps)
	case <-time.After(200 * time.Millisecond):
	}
}
