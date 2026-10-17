// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"fmt"
	"io"
	"strings"
	"testing"
	"time"
)

// The sync past the per-run cap is refused with a sentence on the channel's stderr, which the client
// prints, not a bare request failure.
func TestSSHSync_CapRefusalTellsTheClientWhy(t *testing.T) {
	f := newSSHSyncFixture(t)
	for i := 0; i < maxSSHSyncSessionsPerRun; i++ {
		_, in, out, err := f.openSync(t, nil)
		if err != nil {
			t.Fatalf("sync %d: %v", i, err)
		}
		syncEcho(t, in, out, "x")
	}
	sess, err := f.client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	stderr, err := sess.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	if sess.RequestSubsystem(sshSyncSubsystem) == nil {
		t.Fatal("a third sync was accepted")
	}
	read := make(chan []byte, 1)
	go func() { b, _ := io.ReadAll(stderr); read <- b }()
	var got []byte
	select {
	case got = <-read:
	case <-time.After(5 * time.Second):
		t.Fatal("the refused sync's channel was never closed with a reason")
	}
	want := fmt.Sprintf("wardyn: this run already has %d sync sessions (the most it allows); wait for one to end", maxSSHSyncSessionsPerRun)
	if !strings.Contains(string(got), want) {
		t.Errorf("stderr = %q, want %q", got, want)
	}
}
