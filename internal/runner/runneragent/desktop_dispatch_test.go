// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runneragent

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire/runnertest"
)

// dial_desktop is a byte-stream verb: it goes to the stream adapter, which checks the runner holds the
// sandbox, and never to the ref verbs.
func TestDispatcherRoutesDialDesktopToTheStreamAdapter(t *testing.T) {
	var got string
	d, err := newTestDispatcher(Config{RunnerID: uuid.NewString(), Substrate: runnertest.NewFake(),
		BeforeCreate: func(context.Context, runner.SandboxSpec) error { return nil },
		Created:      func(context.Context, runner.SandboxSpec, string) error { return nil },
		Streams: func(_ context.Context, _ *runnerwire.Peer, _ uint32, method string, _ json.RawMessage) (any, error) {
			got = method
			return runnerwire.StreamResult{Stream: 9}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	a, b := runnerwire.Loopback()
	defer b.Close()
	peer := runnerwire.NewPeer(a, runnerwire.PeerConfig{})
	defer peer.Close()
	data, _ := runnerwire.Marshal(runnerwire.DialDesktopArgs{Ref: "runner:x/y"})
	if _, err := d.Call(t.Context(), peer, 1, runnerwire.MethodDialDesktop, data); err != nil || got != runnerwire.MethodDialDesktop {
		t.Fatalf("dial_desktop reached %q (err %v), want the stream adapter", got, err)
	}
}
