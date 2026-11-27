// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package spire_test

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"sync"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"

	"github.com/cjohnstoniv/wardyn/internal/identity"
	"github.com/cjohnstoniv/wardyn/internal/identity/identitytest"
	"github.com/cjohnstoniv/wardyn/internal/identity/spire"
	"github.com/cjohnstoniv/wardyn/internal/identity/spire/spiretest"
	"github.com/cjohnstoniv/wardyn/internal/identity/svidsource"
	"github.com/cjohnstoniv/wardyn/internal/types"

	"github.com/spiffe/go-spiffe/v2/spiffeid"
)

const (
	td  = "wardyn.local"
	aud = "wardyn-internal"
)

// goodEnv is a config every check accepts: runs namespace "runs", proxy account "wardyn-proxy",
// wardynd's account "wardyn" in namespace "wardyn".
func goodEnv() map[string]string {
	return map[string]string{
		"WARDYN_SPIRE_AGENT_SOCKET":               "unix:///run/spire/agent.sock",
		"WARDYN_SPIRE_SERVER_ADDRESS":             "spire-server.spire:8081",
		"WARDYN_SPIRE_PARENT_ID":                  "spiffe://wardyn.local/k8s-cluster/test",
		"WARDYN_SPIRE_SELECTORS":                  "k8s:ns:runs,k8s:sa:wardyn-proxy,k8s:pod-label:wardyn.component:proxy,k8s:pod-label:wardyn.run-id:{run_id}",
		"WARDYN_POD_SERVICE_ACCOUNT":              "wardyn",
		"WARDYN_K8S_NAMESPACE":                    "runs",
		"WARDYN_K8S_SPIFFE_PROXY_SERVICE_ACCOUNT": "wardyn-proxy",
	}
}

func controlPlaneEntry(sels ...string) spire.Entry {
	e := spire.Entry{ID: "cp-1", SPIFFEID: spiffeid.RequireFromString("spiffe://wardyn.local/wardyn/control-plane")}
	for _, s := range sels {
		typ, val := s[:3], s[4:]
		e.Selectors = append(e.Selectors, spire.Selector{Type: typ, Value: val})
	}
	return e
}

// recorder keeps every audit event.
type recorder struct {
	mu  sync.Mutex
	evs []types.AuditEvent
}

func (r *recorder) Record(_ context.Context, ev types.AuditEvent) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.evs = append(r.evs, ev)
	return nil
}

func (r *recorder) events() []types.AuditEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]types.AuditEvent(nil), r.evs...)
}

// clock is a settable provider clock.
type clock struct {
	mu  sync.Mutex
	off time.Duration
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return time.Now().Add(c.off) }
func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	c.off += d
	c.mu.Unlock()
}

// harness is one provider over a fake server, an in-memory binding store and a denylist.
type harness struct {
	p        identity.Provider
	srv      *spiretest.Server
	bindings *identitytest.MemBindingStore
	rec      *recorder
	clock    *clock
}

type option func(*identity.Deps, *spire.TestSeams)

func withBindings(b identity.TokenBindingStore) option {
	return func(d *identity.Deps, _ *spire.TestSeams) { d.Bindings = b }
}

func newHarness(t *testing.T, rev identity.RevocationStore, alg jose.SignatureAlgorithm, opts ...option) *harness {
	t.Helper()
	srv, err := spiretest.NewServer(td, alg)
	if err != nil {
		t.Fatal(err)
	}
	srv.ControlPlane = []spire.Entry{controlPlaneEntry("k8s:ns:wardyn", "k8s:sa:wardyn")}
	h := &harness{srv: srv, bindings: identitytest.NewMemBindingStore(), rec: &recorder{}, clock: &clock{}}
	d := identity.Deps{TrustDomain: td, Revocations: rev, Audit: h.rec, Bindings: h.bindings}
	s := spire.TestSeams{Env: goodEnv(), Namespace: "wardyn", Server: srv, Now: h.clock.now,
		Open: func(context.Context, string, spiffeid.ID) (svidsource.Source, error) { return srv.Source(), nil }}
	for _, o := range opts {
		o(&d, &s)
	}
	h.p, err = spire.ConstructForTest(d, s)
	if err != nil {
		t.Fatalf("construct: %v", err)
	}
	return h
}

func jtiOf(token string) string {
	sum := sha256.Sum256([]byte(token))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}
