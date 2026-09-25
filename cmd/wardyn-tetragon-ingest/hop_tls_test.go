// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/hoptls"
)

// hopServer is wardynd's internal TLS listener in miniature: a server whose
// certificate the internal CA (caBlob) signed for 127.0.0.1. It counts every
// request that arrives, so a test can prove a refused handshake sent nothing.
func hopServer(t *testing.T, caBlob []byte) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	ca, err := hoptls.ParseCA(caBlob)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := ca.ServingCert("127.0.0.1", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	var hits atomic.Int64
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS13}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv, &hits
}

func newCA(t *testing.T) []byte {
	t.Helper()
	blob, err := hoptls.NewCA(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return blob
}

// caFileFor writes the public half of caBlob where wardynd would publish it.
func caFileFor(t *testing.T, caBlob []byte) string {
	t.Helper()
	ca, err := hoptls.ParseCA(caBlob)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "control-plane-ca.pem")
	if err := os.WriteFile(p, ca.CertPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// The host-sensor bearer must never leave for a non-loopback host in
// plaintext: the ingest refuses to start on such a URL, naming the fix.
func TestControlPlaneClient_RefusesPlaintextToNonLoopback(t *testing.T) {
	for _, u := range []string{"http://wardynd:8080", "http://10.0.0.7:8080", "http://wardyn.wardyn.svc.cluster.local:8080"} {
		if c, err := controlPlaneClient(u, ""); err == nil || !strings.Contains(err.Error(), "WARDYN_INTERNAL_LISTEN") {
			t.Errorf("%s: want a refusal naming the internal TLS listener, got client=%v err=%v", u, c, err)
		}
	}
	if _, err := controlPlaneClient("http://127.0.0.1:8080", ""); err != nil {
		t.Errorf("loopback http is the local install and must pass: %v", err)
	}
}

// https with no usable CA file refuses at boot rather than starting a sensor
// whose every POST would fail.
func TestControlPlaneClient_HTTPSNeedsTheInternalCA(t *testing.T) {
	empty := filepath.Join(t.TempDir(), "empty.pem")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for name, file := range map[string]string{"unset": "", "missing": filepath.Join(t.TempDir(), "nope.pem"), "empty": empty} {
		if _, err := controlPlaneClient("https://wardynd:8443", file); err == nil {
			t.Errorf("%s CA file: want a boot refusal", name)
		}
	}
}

// Pinned to the internal CA alone: a server signed by any other CA gets no
// request, so no bearer. The right CA is the positive control.
func TestControlPlaneClient_FailsClosedOnWrongCA(t *testing.T) {
	serverCA := newCA(t)
	srv, hits := hopServer(t, serverCA)

	wrong, err := controlPlaneClient(srv.URL, caFileFor(t, newCA(t)))
	if err != nil {
		t.Fatal(err)
	}
	sink := newEventSinkWithSource(srv.URL, nil, 1, 1, time.Second, wrong)
	status, postErr := sink.doPost([]byte(`{"events":[]}`), "gt-bearer")
	if status != 0 {
		t.Fatalf("a server outside the pinned CA answered %d; want a refused handshake", status)
	}
	// The batch-failed log carries this error, so an operator sees the cause.
	if postErr == nil || !strings.Contains(postErr.Error(), "certificate") {
		t.Fatalf("refused handshake error = %v; want the certificate failure", postErr)
	}
	if hits.Load() != 0 {
		t.Fatal("the bearer reached a server the internal CA did not sign")
	}

	right, err := controlPlaneClient(srv.URL, caFileFor(t, serverCA))
	if err != nil {
		t.Fatal(err)
	}
	sink = newEventSinkWithSource(srv.URL, nil, 1, 1, time.Second, right)
	if status, _ := sink.doPost([]byte(`{"events":[]}`), "gt-bearer"); status != http.StatusOK || hits.Load() != 1 {
		t.Fatalf("pinned to the right CA: status=%d hits=%d, want 200 and 1", status, hits.Load())
	}
}

// httptest's own certificate is not the internal CA's: a client that fell
// back to the system roots or skipped verification would reach it.
func TestControlPlaneClient_TrustsNoOtherRoot(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer srv.Close()
	c, err := controlPlaneClient(srv.URL, caFileFor(t, newCA(t)))
	if err != nil {
		t.Fatal(err)
	}
	if resp, err := c.Get(srv.URL); err == nil {
		resp.Body.Close()
		t.Fatal("the pinned client accepted a certificate outside the internal CA")
	}
	if hits.Load() != 0 {
		t.Fatal("a request reached a server outside the internal CA")
	}
}
