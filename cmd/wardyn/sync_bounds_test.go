// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/foldersync"
	sdk "github.com/cjohnstoniv/wardyn/pkg/client"
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

// A ProxyCommand that connects and never speaks ends the handshake at the timeout too: its pipes take no deadline.
func TestDialSyncGateway_SilentProxyCommandTimesOut(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer, _ := ssh.NewSignerFromKey(priv)

	old := syncHandshakeTimeout
	syncHandshakeTimeout = 300 * time.Millisecond
	defer func() { syncHandshakeTimeout = old }()

	done := make(chan error, 1)
	go func() {
		_, err := dialSyncGateway(context.Background(), sshGateway{host: "gw.example", port: "22", proxy: "sleep 30", fingerprint: "SHA256:x"}, "run", signer)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "handshake") {
			t.Fatalf("err = %v, want a handshake error", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a silent ProxyCommand outlived syncHandshakeTimeout")
	}
}

// The gateway's stderr is read to the end so its channel drains, but only the first 64 KiB is kept.
func TestCappedBuffer_KeepsAtMost64KiBAndDrains(t *testing.T) {
	var b foldersync.CappedBuffer
	n, err := io.Copy(&b, strings.NewReader(strings.Repeat("x", 200<<10)))
	if err != nil || n != 200<<10 {
		t.Fatalf("copy = %d, %v; want all 200 KiB consumed", n, err)
	}
	if got := len(b.String()); got != 64<<10 {
		t.Errorf("kept %d bytes, want %d", got, 64<<10)
	}
}

// The run's pull cap bounds a pass when it is smaller than the flag's, and never raises it; no cap leaves the flag.
func TestSyncMaxPullBytesTakesTheRunCap(t *testing.T) {
	for _, tc := range []struct{ flag, run, want int64 }{
		{foldersync.DefaultMaxPullBytes, 10 << 20, 10 << 20},
		{5 << 20, 10 << 20, 5 << 20},
		{5 << 20, 0, 5 << 20},
	} {
		f := syncFlags{maxPullBytes: tc.flag, pull: true}
		if err := applySyncRules(&f, sdk.RunSyncRules{MaxPullBytes: tc.run}); err != nil || f.maxPullBytes != tc.want {
			t.Errorf("flag %d, run cap %d: %d, %v; want %d", tc.flag, tc.run, f.maxPullBytes, err, tc.want)
		}
	}
}

// A --pull sync of a run whose walls keep the sandbox's changes in exits with the server's sentence before it
// connects to the gateway; without --pull it goes on.
func TestSyncPullRefusedBeforeConnect(t *testing.T) {
	const text = "local_folder: bringing the sandbox's changes back to your computer is walled off by your organisation's boundary"
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	var dialed atomic.Int32
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			dialed.Add(1)
			_ = c.Close()
		}
	}()
	web := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/runs/") {
			_ = json.NewEncoder(w).Encode(map[string]any{"sync": map[string]any{"deny_pull": true, "deny_pull_text": text}})
			return
		}
		host, port, _ := net.SplitHostPort(ln.Addr().String())
		_ = json.NewEncoder(w).Encode(map[string]any{"ssh": map[string]any{
			"enabled": true, "advertise_addr": net.JoinHostPort(host, port), "host_key_fingerprint": "SHA256:none",
		}})
	}))
	t.Cleanup(web.Close)
	t.Setenv("HOME", t.TempDir())
	cmd := syncCmd(func() *sdk.Client { return &sdk.Client{BaseURL: web.URL, HTTPClient: web.Client()} })
	cmd.SetArgs([]string{testRunID, t.TempDir(), "--pull"})
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	if err := cmd.Execute(); err == nil || err.Error() != text {
		t.Fatalf("sync --pull = %v, want %q", err, text)
	}
	if n := dialed.Load(); n != 0 {
		t.Errorf("the gateway was dialed %d times before the refusal", n)
	}
	f := syncFlags{maxPullBytes: foldersync.DefaultMaxPullBytes}
	if err := applySyncRules(&f, sdk.RunSyncRules{DenyPull: true, DenyPullText: text}); err != nil {
		t.Errorf("a push-only sync was refused: %v", err)
	}
}
