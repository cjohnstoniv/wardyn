// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"net"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner/runnerio"
)

// A window hello from another uid is closed before runnerd reads a byte of it: the socket's uid check
// holds for a window as it does for a terminal, so no nonce is ever handed out.
func TestLocalWindowHelloFromAnotherUIDIsClosed(t *testing.T) {
	served := make(chan struct{}, 1)
	l := openTestSocket(t, "11111111-1111-1111-1111-111111111111", "https://org.example", func(c net.Conn) { served <- struct{}{}; _ = c.Close() })
	l.peerUID = func(net.Conn) (uint32, error) { return uint32(os.Geteuid()) + 1, nil }
	l.Set(true)
	defer l.Close()
	c, err := net.Dial("unix", readPointer(t, l).Socket)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	_ = runnerio.WriteLocalJSON(c, runnerio.LocalHello, runnerio.LocalHelloMsg{V: runnerio.LocalProtocol, RunID: uuid.New(), Kind: runnerio.LocalKindWindow})
	if n, err := c.Read(make([]byte, 1)); n != 0 || err == nil {
		t.Fatalf("another uid's window hello read %d bytes, %v", n, err)
	}
	select {
	case <-served:
		t.Fatal("another uid's window hello reached the protocol")
	default:
	}
}
