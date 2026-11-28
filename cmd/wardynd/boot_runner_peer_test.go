// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/hoptls"
	"github.com/cjohnstoniv/wardyn/internal/runner/remote"
	"github.com/cjohnstoniv/wardyn/internal/secretstore"
)

// raceSecrets is one shared secret row two booting replicas read and write.
type raceSecrets struct {
	mu   sync.Mutex
	val  []byte
	puts int
}

func (s *raceSecrets) Get(context.Context, string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.val == nil {
		return nil, secretstore.ErrNotFound
	}
	return bytes.Clone(s.val), nil
}

func (s *raceSecrets) Put(_ context.Context, _ string, v []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.val, s.puts = bytes.Clone(v), s.puts+1
	return nil
}

// R2d: two replicas at the first HA boot end with one 32-byte peer key, and Put runs once.
func TestRunnerPeerKeyFirstBootRace(t *testing.T) {
	sec := &raceSecrets{}
	var create sync.Mutex
	store := bootKeyStore{sec, func(context.Context) (func(), error) { create.Lock(); return create.Unlock, nil }}
	ha, peerURL := true, "https://10.0.0.1:8443"
	f := &bootFlags{ha: &ha, peerURL: &peerURL}
	var wg sync.WaitGroup
	got := make([]*remote.PeerConfig, 2)
	errs := make([]error, 2)
	for i := range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got[i], errs[i] = loadRunnerPeer(t.Context(), f, store, nil, "https://wardynd:8443")
		}()
	}
	wg.Wait()
	for i := range 2 {
		if errs[i] != nil || got[i] == nil {
			t.Fatalf("replica %d: %v, %v", i, got[i], errs[i])
		}
	}
	if len(got[0].Key) != 32 || !bytes.Equal(got[0].Key, got[1].Key) {
		t.Fatalf("keys differ or are not 32 bytes: %d and %d bytes", len(got[0].Key), len(got[1].Key))
	}
	if sec.puts != 1 {
		t.Fatalf("Put ran %d times, want 1", sec.puts)
	}
	if got[0].ServerName != "wardynd" || got[0].URL != peerURL {
		t.Fatalf("config = server name %q, url %q", got[0].ServerName, got[0].URL)
	}
	off := false
	if p, err := loadRunnerPeer(t.Context(), &bootFlags{ha: &off, peerURL: &peerURL}, store, nil, "https://wardynd:8443"); p != nil || err != nil {
		t.Fatalf("without WARDYN_HA = %v, %v; want nil, nil", p, err)
	}
	empty := ""
	if p, err := loadRunnerPeer(t.Context(), &bootFlags{ha: &ha, peerURL: &empty}, store, nil, "https://wardynd:8443"); p != nil || err != nil {
		t.Fatalf("without WARDYN_PEER_URL = %v, %v; want nil, nil", p, err)
	}
}

// R2c: the peer client verifies the serving certificate by the control-plane URL's host while it dials the pod
// address; a certificate for another name, or from another CA, fails the handshake.
func TestPeerClientVerifiesControlPlaneSAN(t *testing.T) {
	newCA := func() *hoptls.CA {
		blob, err := hoptls.NewCA(time.Now())
		if err != nil {
			t.Fatal(err)
		}
		ca, err := hoptls.ParseCA(blob)
		if err != nil {
			t.Fatal(err)
		}
		return ca
	}
	ca, other := newCA(), newCA()
	serve := func(signer *hoptls.CA, host string) string {
		cert, err := signer.ServingCert(host, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		ts := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
		ts.TLS = &tls.Config{Certificates: []tls.Certificate{cert}, NextProtos: []string{"http/1.1"}, MinVersion: tls.VersionTLS13}
		ts.StartTLS()
		t.Cleanup(ts.Close)
		return ts.Listener.Addr().String()
	}
	cfg := remote.ClientTLS(remote.PeerConfig{Roots: hopRootsOf(string(ca.CertPEM)), ServerName: "wardynd.example"})
	dial := func(addr string) error {
		host, _, _ := net.SplitHostPort(addr)
		if host != "127.0.0.1" {
			t.Fatalf("dialled %s, want the pod-style address 127.0.0.1", addr)
		}
		c, err := tls.Dial("tcp", addr, cfg)
		if err == nil {
			_ = c.Close()
		}
		return err
	}
	if err := dial(serve(ca, "wardynd.example")); err != nil {
		t.Fatalf("the control-plane host's certificate was refused: %v", err)
	}
	if err := dial(serve(ca, "someone-else.example")); err == nil {
		t.Fatal("a certificate for another name was accepted")
	}
	if err := dial(serve(other, "wardynd.example")); err == nil {
		t.Fatal("a certificate from another CA was accepted")
	}
}
