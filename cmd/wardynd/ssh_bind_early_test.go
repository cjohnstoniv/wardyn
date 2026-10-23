// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/api"
)

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func sshHealthzField(t *testing.T, srv *api.Server) any {
	t.Helper()
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("GET /healthz = %d: %s", w.Code, w.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode /healthz: %v", err)
	}
	return body["ssh"]
}

// WARDYN_SSH_BIND_EARLY (#2331, option B): on, a taken SSH port does not stop boot and /healthz stops advertising
// SSH; off, the bind failure stays the log-only line inside ServeSSHGateway and /healthz still advertises it.
func TestSSHBindEarly(t *testing.T) {
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer taken.Close()
	takenAddr := taken.Addr().String()
	flags := func(addr string, early bool) *bootFlags { return &bootFlags{sshListen: &addr, sshBindEarly: &early} }
	newSrv := func(addr string) *api.Server {
		return api.New(api.Config{SSHListenAddr: addr, SSHHostKey: key, SSHAdvertiseAddr: "ssh.example:2222", RunnerTarget: "none"})
	}

	t.Run("on, port taken: boot continues and /healthz omits ssh", func(t *testing.T) {
		srv := newSrv(takenAddr)
		if sshHealthzField(t, srv) == nil {
			t.Fatal("control: /healthz should advertise ssh before the bind is tried")
		}
		if ln := bindSSHGatewayEarly(flags(takenAddr, true), srv); ln != nil {
			_ = ln.Close()
			t.Fatal("bindSSHGatewayEarly returned a listener for a taken port")
		}
		if got := sshHealthzField(t, srv); got != nil {
			t.Errorf("/healthz ssh = %v after a failed early bind, want it omitted", got)
		}
		// Boot goes on: no accept loop is started for the failed bind, and nothing panics or returns an error.
		startSSHGateway(context.Background(), flags(takenAddr, true), srv, nil)
	})

	t.Run("on, port free: the listener is returned and /healthz keeps ssh", func(t *testing.T) {
		free, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		addr := free.Addr().String()
		_ = free.Close()
		srv := newSrv(addr)
		ln := bindSSHGatewayEarly(flags(addr, true), srv)
		if ln == nil {
			t.Fatal("bindSSHGatewayEarly bound nothing on a free port")
		}
		defer ln.Close()
		if sshHealthzField(t, srv) == nil {
			t.Error("/healthz dropped ssh although the bind worked")
		}
	})

	t.Run("off: nothing binds early, the failure is only logged and /healthz still advertises", func(t *testing.T) {
		var logs syncBuffer
		prev := slog.Default()
		slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
		defer slog.SetDefault(prev)

		srv := newSrv(takenAddr)
		f := flags(takenAddr, false)
		if ln := bindSSHGatewayEarly(f, srv); ln != nil {
			_ = ln.Close()
			t.Fatal("the switch is off, yet bindSSHGatewayEarly bound")
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		startSSHGateway(ctx, f, srv, nil)
		deadline := time.Now().Add(5 * time.Second)
		for !strings.Contains(logs.String(), "ssh gateway stopped") && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		if !strings.Contains(logs.String(), "ssh gateway stopped") {
			t.Errorf("no 'ssh gateway stopped' log for the taken port; logs: %s", logs.String())
		}
		if sshHealthzField(t, srv) == nil {
			t.Error("with the switch off /healthz must keep advertising ssh, as before")
		}
	})
}
