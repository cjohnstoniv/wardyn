// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package main

import (
	"net"
	"testing"
	"time"
)

// On Linux SO_PEERCRED reports this process's real uid, so this user's own CLI is served.
func TestLocalAttachSocketServesThisUserThroughRealPeerCredentials(t *testing.T) {
	served := make(chan struct{}, 1)
	l := openTestSocket(t, "11111111-1111-1111-1111-111111111111", "https://org.example", func(c net.Conn) { served <- struct{}{}; _ = c.Close() })
	l.Set(true)
	defer l.Close()
	c, err := net.Dial("unix", readPointer(t, l).Socket)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	select {
	case <-served:
	case <-time.After(5 * time.Second):
		t.Fatal("this user's connection was not served")
	}
}
