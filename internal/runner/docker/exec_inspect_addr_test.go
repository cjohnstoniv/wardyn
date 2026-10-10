// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/moby/moby/client"
)

// execDaemon answers _ping and one exec-inspect body, recording the Host
// header of every exec-inspect request so a test can assert what the raw read
// put on the wire.
type execDaemon struct {
	*httptest.Server
	mu    sync.Mutex
	hosts []string
}

func newExecDaemon(ln net.Listener) *execDaemon {
	d := &execDaemon{}
	d.Server = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/_ping") {
			w.Header().Set("Api-Version", "1.44")
			return
		}
		if strings.Contains(r.URL.Path, "/exec/") {
			d.mu.Lock()
			d.hosts = append(d.hosts, r.Host)
			d.mu.Unlock()
		}
		w.Header().Set("Content-Type", "application/json")
		// Not running with exit code 0 is the ONE answer the client's own
		// inspect flattens ambiguously, so it is the only one that reaches
		// the raw read at all.
		_, _ = io.WriteString(w, `{"ID":"x","Running":false,"ExitCode":0,"Pid":7}`)
	}))
	d.Server.Listener.Close()
	d.Server.Listener = ln
	d.Server.Start()
	return d
}

func (d *execDaemon) seen() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.hosts...)
}

// TestExecInspectRaw_AddressesTheClientDaemon proves the raw read reaches the
// daemon at the address the CLIENT is configured with, the way the SDK's
// buildRequest sets it: the daemon's own host:port over TCP, and DummyHost as
// the Host header on a socket transport (a socket path is not a hostname).
// It used to send the literal "docker", which is a different endpoint behind
// any daemon fronted by a name-based proxy.
func TestExecInspectRaw_AddressesTheClientDaemon(t *testing.T) {
	tcpLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen tcp: %v", err)
	}
	unixLn, err := net.Listen("unix", filepath.Join(t.TempDir(), "d.sock"))
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	for _, tc := range []struct {
		name string
		ln   net.Listener
		host string
		want string
	}{
		{name: "tcp", ln: tcpLn, host: "tcp://" + tcpLn.Addr().String(), want: tcpLn.Addr().String()},
		{name: "unix socket", ln: unixLn, host: "unix://" + unixLn.Addr().String(), want: client.DummyHost},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := newExecDaemon(tc.ln)
			defer d.Close()

			cli, err := client.New(client.WithHost(tc.host))
			if err != nil {
				t.Fatalf("client.New: %v", err)
			}
			defer cli.Close()
			before := len(d.seen())

			insp, err := newEngineClient(cli).ExecInspectRaw(context.Background(), "x")
			if err != nil {
				t.Fatalf("ExecInspectRaw: %v", err)
			}
			if code, exited := insp.exited(); !exited || code != 0 {
				t.Fatalf("exited() = %d, %v; want 0, true — the raw read did not run, so it never put a Host on the wire", code, exited)
			}
			hosts := d.seen()[before:]
			if len(hosts) == 0 {
				t.Fatal("no exec-inspect request reached the daemon")
			}
			for _, h := range hosts {
				if h != tc.want {
					t.Errorf("exec-inspect Host = %q, want %q", h, tc.want)
				}
			}
		})
	}
}

// TestEngineClient_RawTransportReleasesIdleConnections pins the idle bound.
// Without it wardynd — long-lived, polling execs on every tick — holds a
// socket per poll forever, the leak the SDK's own default transport sets this
// to prevent (moby/moby#45539).
func TestEngineClient_RawTransportReleasesIdleConnections(t *testing.T) {
	cli, err := client.New(client.WithHost("tcp://127.0.0.1:1"))
	if err != nil {
		t.Fatalf("client.New: %v", err)
	}
	defer cli.Close()
	raw := newEngineClient(cli).raw
	tr, ok := raw.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("raw transport is %T, want *http.Transport", raw.Transport)
	}
	if tr.IdleConnTimeout != 30*time.Second {
		t.Errorf("raw transport IdleConnTimeout = %v, want 30s (the SDK's own default; 0 never releases an idle connection)", tr.IdleConnTimeout)
	}
}