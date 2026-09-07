// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/identity"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// mintSubjectRecorder wraps a real identity provider and records the humanSub
// every run identity was minted with — the claim that becomes claims.Sub, i.e.
// the SECRET-NAMESPACE selector for broker mints and proxy-side injection.
type mintSubjectRecorder struct {
	identity.Provider
	mu       sync.Mutex
	subjects []string
	sponsors []string
}

func (m *mintSubjectRecorder) MintRunIdentity(ctx context.Context, runID uuid.UUID, humanSub, sponsor, audience string) (identity.RunIdentity, error) {
	m.mu.Lock()
	m.subjects = append(m.subjects, humanSub)
	m.sponsors = append(m.sponsors, sponsor)
	m.mu.Unlock()
	return m.Provider.MintRunIdentity(ctx, runID, humanSub, sponsor, audience)
}

func (m *mintSubjectRecorder) seen() (subjects, sponsors []string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.subjects...), append([]string(nil), m.sponsors...)
}

// TestLocalPrincipalHeaderCannotSteerTheSecretNamespace is the pin for F099
// (Requirement 10).
//
// In LocalMode, actorFromRequest honors the DEV-ONLY X-Wardyn-Principal header,
// and handleCreateRun used that value as BOTH the run's attribution AND the run
// identity's Sub. Sub is the secret-namespace selector end to end: the broker
// resolves stored secrets as secrets.For(ownerOf(caller)) with ownerOf ==
// caller.Sub (internal/broker/broker_mint_kinds.go, mintGitPAT/mintSSHKey), the
// injection sink does Secrets.For(claims.Sub) (internal/api/injection.go), and
// pg.Store.Get selects `owned_by IN (”, $1) ORDER BY (owned_by = $1) DESC`
// (internal/secretstore/pg/pg.go) — so the header value IS the selector, and the
// named owner's own row WINS over the operator's. A deployment carrying
// member-owned rows from an SSO-configured era (or a shared database) that is
// later served in LocalMode therefore let any local caller mint another
// principal's stored PAT or SSH key by naming them in a header.
//
// The header keeps its documented job — attribution — so the assertions are
// paired: the SUBJECT must be the injected operator, and the ATTRIBUTION must
// still be the header value.
func TestLocalPrincipalHeaderCannotSteerTheSecretNamespace(t *testing.T) {
	const (
		operator = "local:operator"
		victim   = "sub-alice@corp.example" // another principal's OIDC sub
	)
	h := newHarness(t)
	rec := &mintSubjectRecorder{Provider: h.idp}
	srv := New(Config{
		Store:           newAuthzStore(),
		Identity:        rec,
		Approvals:       h.approvals,
		Broker:          h.broker,
		Audit:           h.audit,
		TrustDomain:     "wardyn.local",
		ControlPlaneURL: "http://wardynd:8080",
		LocalMode:       true,
		LocalOperator:   operator,
		DefaultPolicy: types.RunPolicySpec{
			AllowedDomains:      []string{"api.anthropic.com"},
			MinConfinementClass: types.CC2,
		},
	})

	r := httptest.NewRequest(http.MethodPost, "/api/v1/runs",
		strings.NewReader(`{"agent":"claude-code","task":"steal"}`))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Wardyn-Principal", victim)
	r.Host = "127.0.0.1"
	r.RemoteAddr = "127.0.0.1:54321"
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)

	subjects, sponsors := rec.seen()
	if len(subjects) == 0 {
		t.Fatalf("no run identity was minted (status %d): %s", w.Code, w.Body.String())
	}
	for _, sub := range subjects {
		if sub == victim {
			t.Errorf("the run identity was minted with Sub=%q, taken straight from the caller's "+
				"X-Wardyn-Principal header. Sub selects the secret namespace (broker ownerOf -> "+
				"secrets.For, injection.go Secrets.For, pg owned_by IN ('',$1) ORDER BY (owned_by=$1) "+
				"DESC), so this run mints THAT principal's stored git_pat / ssh_key rows in preference "+
				"to the operator's", sub)
		}
		if sub != operator {
			t.Errorf("run identity Sub = %q, want the INJECTED local operator %q — the namespace must "+
				"come from what humanOrAdminAuth injected, never from a request header", sub, operator)
		}
	}
	// ATTRIBUTION is unchanged: the DEV-ONLY header still names who acted.
	for _, sp := range sponsors {
		if sp != victim {
			t.Errorf("sponsor = %q, want the header value %q — the header keeps its documented "+
				"attribution job; only the namespace selector was taken away from it", sp, victim)
		}
	}
	if typ, name := actorFromRequest(r.WithContext(withLocalPrincipal(r.Context(), operator))); typ != types.ActorHuman || name != victim {
		t.Errorf("actorFromRequest = (%q,%q), want (human, %q): the attribution override must still work", typ, name, victim)
	}
}
