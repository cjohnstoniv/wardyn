// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"fmt"
	"io"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// The sync past the per-run cap is acked and then ended with a sentence on stderr and exit status 1, the
// way a refused directory is: wardyn sync stops on a false subsystem reply before it reads stderr, so a
// refusal that replied false would hide the sentence from the only client.
func TestSSHSync_CapRefusalTellsTheClientWhy(t *testing.T) {
	f := newSSHSyncFixture(t)
	for i := 0; i < maxSSHSyncSessionsPerRun; i++ {
		_, in, out, err := f.openSync(t, nil)
		if err != nil {
			t.Fatalf("sync %d: %v", i, err)
		}
		syncEcho(t, in, out, "x")
	}
	ch, reqs, err := f.client.OpenChannel("session", nil)
	if err != nil {
		t.Fatalf("the third sync's channel: %v", err)
	}
	t.Cleanup(func() { _ = ch.Close() })
	stderr := make(chan string, 1)
	go func() { b, _ := io.ReadAll(ch.Stderr()); stderr <- string(b) }()
	status := make(chan int, 1)
	go func() {
		got := -1
		for r := range reqs {
			if r.Type == "exit-status" {
				var m struct{ Status uint32 }
				if ssh.Unmarshal(r.Payload, &m) == nil {
					got = int(m.Status)
				}
			}
			if r.WantReply {
				_ = r.Reply(false, nil)
			}
		}
		status <- got
	}()
	acked, err := ch.SendRequest("subsystem", true, ssh.Marshal(struct{ Name string }{sshSyncSubsystem}))
	if err != nil || !acked {
		t.Fatalf("the third sync's subsystem request acked = %v, %v; want it acked", acked, err)
	}
	want := fmt.Sprintf("wardyn: this run already has %d sync sessions (the most it allows); wait for one to end\n", maxSSHSyncSessionsPerRun)
	for _, wait := range []func(){
		func() {
			select {
			case got := <-stderr:
				if got != want {
					t.Errorf("stderr = %q, want %q", got, want)
				}
			case <-time.After(5 * time.Second):
				t.Error("the refused sync's channel never ended with a reason")
			}
		},
		func() {
			select {
			case got := <-status:
				if got != 1 {
					t.Errorf("exit status = %d, want 1", got)
				}
			case <-time.After(5 * time.Second):
				t.Error("the refused sync's channel never reported an exit status")
			}
		},
	} {
		wait()
	}
}
