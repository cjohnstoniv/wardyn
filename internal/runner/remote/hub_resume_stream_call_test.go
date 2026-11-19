// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package remote

import (
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
)

// A session whose replay still holds an exec_stream CALL the runner never received is not resumed: its
// caller already failed offline, and a fresh session sends no replay, so the exec never starts. A replayed
// lifecycle CALL keeps the resume.
func TestHubResumeStartsFreshOverAReplayedStreamCall(t *testing.T) {
	for _, tc := range []struct {
		method  string
		resumed bool
	}{{runnerwire.MethodExecStream, false}, {runnerwire.MethodStop, true}} {
		t.Run(tc.method, func(t *testing.T) {
			body, err := runnerwire.Marshal(runnerwire.Call{Method: tc.method})
			if err != nil {
				t.Fatal(err)
			}
			buf := runnerwire.NewReplayBuffer()
			if err := buf.Reserve(t.Context(), len(body)); err != nil {
				t.Fatal(err)
			}
			buf.Assign(runnerwire.Frame{Type: runnerwire.TypeCall, Stream: 2, Payload: body})
			org, _ := runnerwire.Loopback()
			peer := runnerwire.NewPeer(org, runnerwire.PeerConfig{Org: true, Replay: buf, NextID: 4, AbandonedCalls: []uint32{2}})
			go func() { _ = peer.Run(t.Context()) }()
			peer.Close()
			s := &hubSession{id: "session-1", peer: peer}
			s.readyID.Store(&s.id)
			cfg, resumed, err := (&Hub{}).resume(t.Context(), s, &runnerwire.Resume{SessionID: "session-1", LastSeq: 0})
			if err != nil || resumed != tc.resumed {
				t.Fatalf("resume = resumed %v, %v; want resumed %v", resumed, err, tc.resumed)
			}
			if !resumed && cfg.Replay != nil {
				t.Fatal("a fresh session carried the old replay")
			}
		})
	}
}
