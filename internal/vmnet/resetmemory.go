// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package vmnet

import (
	"sync"
	"time"

	"gvisor.dev/gvisor/pkg/tcpip/stack"
)

// resetWindow is how long the relay answers a SYN whose exact 4-tuple it just reset with another reset,
// and with nothing else. The forwarder forgets a SYN when the relay completes it
// (tcp.ForwarderRequest.Complete), so a retransmit the guest sent before the reset reached it would
// otherwise arrive as a new flow: a second ask to the proxy, and an opaque slot held for it, which at
// the bound refuses a real flow as over the tunnel limit (#3932). A guest that connects again uses a new
// source port, which this never matches.
const resetWindow = 10 * time.Second

// maxResets bounds the memory. Full, it records nothing new, and a retransmit is a new flow, as before.
const maxResets = 4096

// resetMemory is the SYNs the relay reset within resetWindow, by 4-tuple.
type resetMemory struct {
	now func() time.Time
	mu  sync.Mutex
	at  map[stack.TransportEndpointID]time.Time
}

func newResetMemory(now func() time.Time) *resetMemory {
	return &resetMemory{now: now, at: map[stack.TransportEndpointID]time.Time{}}
}

// add records that id's SYN is being answered with a reset. Call it before Complete(true).
func (m *resetMemory) add(id stack.TransportEndpointID) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	if len(m.at) >= maxResets {
		for k, t := range m.at {
			if now.Sub(t) >= resetWindow {
				delete(m.at, k)
			}
		}
		if len(m.at) >= maxResets {
			return
		}
	}
	m.at[id] = now
}

// recent reports whether id's SYN was reset within resetWindow.
func (m *resetMemory) recent(id stack.TransportEndpointID) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.at[id]
	if ok && m.now().Sub(t) >= resetWindow {
		delete(m.at, id)
		return false
	}
	return ok
}
