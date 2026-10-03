// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestBrokerMintRefusesBrokeredPATGrants pins the raw-mint refusal for every id
// in BrokeredPATGrantIDs: a 403 and a brokered:mint deny with no control-plane
// call, while an id outside the set and an undecodable body still relay.
func TestBrokerMintRefusesBrokeredPATGrants(t *testing.T) {
	a, b := uuid.New(), uuid.New()
	var cpCalls atomic.Int32
	cp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cpCalls.Add(1)
		_, _ = w.Write([]byte(`{"kind":"git_pat","token":"t"}`))
	}))
	defer cp.Close()

	buf := &bytes.Buffer{}
	p := newProxy(Options{
		RunID:               uuid.New(),
		Policy:              CompilePolicy(types.RunPolicySpec{}),
		Sink:                &decisionSink{out: buf, ch: make(chan egress.DecisionLog, 64)},
		Resolver:            publicResolver{},
		Dial:                redirectDial(upstreamAddr(cp)),
		ControlPlaneURL:     "http://wardynd.test:8080",
		RunToken:            newTokenSource("RUNTOK"),
		BrokeredPATGrantIDs: []uuid.UUID{a, b},
	})
	mint := func(body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, mustLocalReq(t, http.MethodPost, routeMint, strings.NewReader(body)))
		return rec
	}

	for _, id := range []uuid.UUID{a, b} {
		before := cpCalls.Load()
		rec := mint(`{"grant_id":"` + id.String() + `"}`)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("brokered PAT grant %s: status = %d, want 403", id, rec.Code)
		}
		if cpCalls.Load() != before {
			t.Fatalf("brokered PAT grant %s reached the control plane", id)
		}
		if d := lastDecision(t, buf); d.RuleSource != ruleSourceMint || d.Decision != egress.Deny {
			t.Fatalf("decision = %+v, want %s deny", d, ruleSourceMint)
		}
	}

	if rec := mint(`{"grant_id":"` + uuid.New().String() + `"}`); rec.Code != http.StatusOK {
		t.Fatalf("an id outside the set: status = %d, want 200 (relayed)", rec.Code)
	}
	if rec := mint(`{"grant_id":`); rec.Code == http.StatusForbidden {
		t.Fatal("an undecodable body was refused; it must relay and let the control plane answer")
	}
	if got := cpCalls.Load(); got != 2 {
		t.Fatalf("control-plane calls = %d, want 2 (the outside id and the undecodable body)", got)
	}
}

// With the broker off the set is empty, and a legacy PAT grant still mints
// through the relay.
func TestBrokerMintRelaysPATGrantWhenSetIsEmpty(t *testing.T) {
	var cpCalls atomic.Int32
	cp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cpCalls.Add(1)
		_, _ = w.Write([]byte(`{"kind":"git_pat","token":"t"}`))
	}))
	defer cp.Close()
	p, _ := newLocalRouteProxy(t, "http://wardynd.test:8080", "RUNTOK", upstreamAddr(cp), nil, nil)

	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, mustLocalReq(t, http.MethodPost, routeMint, strings.NewReader(`{"grant_id":"`+uuid.New().String()+`"}`)))
	if rec.Code != http.StatusOK || cpCalls.Load() != 1 {
		t.Fatalf("status = %d, control-plane calls = %d, want a relayed 200", rec.Code, cpCalls.Load())
	}
}
