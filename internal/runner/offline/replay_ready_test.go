// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package offline

import (
	"context"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
)

func TestReplayActivationFollowsPeerStartup(t *testing.T) {
	m, _, _, org := managerFixture(t)
	old := m.peer
	if !m.online || m.replayReady {
		t.Fatal("admission must precede background replay")
	}
	m.Online(&runnerwire.Peer{})
	if m.replayReady {
		t.Fatal("foreign peer activated replay")
	}
	m.Online(old)
	if !m.replayReady {
		t.Fatal("started admitted peer did not activate replay")
	}
	if err := m.Clock(runnerwire.Hello{ServerTime: org}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if m.online || m.replayReady {
		t.Fatal("new handshake retained old admission")
	}
	m.Online(old)
	if m.replayReady {
		t.Fatal("stale callback activated new handshake")
	}
	next := &runnerwire.Peer{}
	if err := m.BeforeReady(context.Background(), next, true); err != nil {
		t.Fatal(err)
	}
	m.Online(old)
	if m.replayReady {
		t.Fatal("old peer activated resumed replay")
	}
	m.Online(next)
	if !m.replayReady {
		t.Fatal("resumed peer did not activate replay")
	}
	m.Offline(context.Background())
	m.Online(next)
	if m.online || m.replayReady {
		t.Fatal("offline peer activated replay")
	}
	if err := m.BeforeReady(context.Background(), next, false); err != nil {
		t.Fatal(err)
	}
	m.Online(next)
	if err := m.Revoked(context.Background()); err != nil {
		t.Fatal(err)
	}
	m.Online(next)
	if m.online || m.replayReady {
		t.Fatal("revoked peer activated replay")
	}
}
