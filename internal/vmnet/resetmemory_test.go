// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package vmnet

import (
	"testing"
	"time"

	"gvisor.dev/gvisor/pkg/tcpip/stack"
)

func TestResetMemoryForgetsAfterItsWindowAndStaysBounded(t *testing.T) {
	now := time.Unix(1000, 0)
	m := newResetMemory(func() time.Time { return now })
	id := func(port uint16) stack.TransportEndpointID {
		return stack.TransportEndpointID{LocalPort: 2000, RemotePort: port}
	}
	a, b := id(40000), id(40001)

	m.add(a)
	if !m.recent(a) || m.recent(b) {
		t.Fatalf("recent(a) = %v, recent(b) = %v, want true and false", m.recent(a), m.recent(b))
	}
	now = now.Add(resetWindow)
	if m.recent(a) {
		t.Fatal("a reset is remembered past its window")
	}
	if _, ok := m.at[a]; ok {
		t.Fatal("an expired reset stays in the map")
	}

	for i := 0; i < maxResets; i++ {
		m.add(id(uint16(i + 1)))
	}
	x := id(65535)
	m.add(x)
	if m.recent(x) || len(m.at) != maxResets {
		t.Fatalf("a full memory recorded a new reset: recent %v, len %d", m.recent(x), len(m.at))
	}
	now = now.Add(resetWindow)
	m.add(x)
	if !m.recent(x) || len(m.at) != 1 {
		t.Fatalf("after the window the memory did not sweep: recent %v, len %d", m.recent(x), len(m.at))
	}
}
