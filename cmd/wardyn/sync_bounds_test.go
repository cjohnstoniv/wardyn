// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// A gateway that accepts the connection and never speaks ends the handshake at the timeout.
func TestDialSyncGateway_SilentGatewayTimesOut(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			defer c.Close()
		}
	}()
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer, _ := ssh.NewSignerFromKey(priv)
	host, port, _ := net.SplitHostPort(ln.Addr().String())

	old := syncHandshakeTimeout
	syncHandshakeTimeout = 300 * time.Millisecond
	defer func() { syncHandshakeTimeout = old }()

	done := make(chan error, 1)
	go func() {
		_, err := dialSyncGateway(context.Background(), sshGateway{host: host, port: port, fingerprint: "SHA256:x"}, "run", signer)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "handshake") {
			t.Fatalf("err = %v, want a handshake error", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the handshake outlived syncHandshakeTimeout")
	}
}

// The gateway's stderr is read to the end so its channel drains, but only the first 64 KiB is kept.
func TestCappedBuffer_KeepsAtMost64KiBAndDrains(t *testing.T) {
	var b cappedBuffer
	n, err := io.Copy(&b, strings.NewReader(strings.Repeat("x", 200<<10)))
	if err != nil || n != 200<<10 {
		t.Fatalf("copy = %d, %v; want all 200 KiB consumed", n, err)
	}
	if got := len(b.String()); got != 64<<10 {
		t.Errorf("kept %d bytes, want %d", got, 64<<10)
	}
}
