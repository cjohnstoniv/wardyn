// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/broker"
	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// storeReadFails is a secret store whose every Get, in every namespace, fails
// with err; List answers from the store it wraps.
type storeReadFails struct {
	secretstore.Store
	err error
}

func (s storeReadFails) For(owner string) secretstore.Store {
	return storeReadFails{s.Store.For(owner), s.err}
}
func (s storeReadFails) Get(context.Context, string) ([]byte, error) { return nil, s.err }

// storeReadFailures is every way a read of a person's stored model credential
// fails, in the shapes the stores return. The proxy's table
// (internal/egress/proxy TestInjector_StoreReadFailures) uses the same case
// names: only err_unavailable is the transient 503 the proxy rides out.
var storeReadFailures = []struct {
	name                 string
	err                  error
	keyReason, subReason string // the two arms' secret.read reasons
}{
	{"not_found", fmt.Errorf("pg secretstore: get alice/x: %w", errors.Join(secretstore.ErrNotFound, errors.New("no rows in result set"))),
		"own_key_absent", "resolve_failed"},
	{"pointer_extant_value_absent", errors.New("pg secretstore: alice/x refused: vault GET wardyn/data/x: 404"),
		"store_refused", "resolve_failed"},
	{"http_401", errors.New("pg secretstore: alice/x: vault GET wardyn/data/x: 401: permission denied"),
		"store_refused", "resolve_failed"},
	{"http_403", errors.New("pg secretstore: alice/x: key vault get: 403 Forbidden: caller is not authorized"),
		"store_refused", "resolve_failed"},
	{"disabled_or_retired_key", errors.New("pg secretstore: alice/x refused — its data key does not unwrap for this row " +
		"(moved, forged, corrupted, or its key version retired): transit decrypt: 400: key version is disabled"),
		"store_refused", "resolve_failed"},
	{"binding_mismatch", errors.New(`pg secretstore: alice/x: refused: the value's metadata wardyn-owner is "bob", but this row needs "alice"`),
		"store_refused", "resolve_failed"},
	{"err_unavailable", fmt.Errorf("pg secretstore: alice/x: vault GET wardyn/data/x: 503 sealed: %w", secretstore.ErrUnavailable),
		"store_unavailable", "store_unavailable"},
	{"unknown_definitive", errors.New("an error no classifier has seen"),
		"store_refused", "resolve_failed"},
}

// wantStoreReadStatus is what a sink answers a failed read: 503 only when the
// store did not answer, so the proxy rides out a blip but drops the header at
// once on a refusal.
func wantStoreReadStatus(err error) int {
	if errors.Is(err, secretstore.ErrUnavailable) {
		return http.StatusServiceUnavailable
	}
	return http.StatusFailedDependency
}

// keySinkGrant dispatches a key provider run and has the broker mint its grant.
func keySinkGrant(t *testing.T) (*harness, *bearerGuardStore, types.CredentialGrant) {
	t.Helper()
	p := mpKeyProvider("corp", "review-key", types.ModelProviderAnthropicAPIKey, types.ProviderHarness{Harness: "claude-code"})
	st := &bearerGuardStore{run: types.AgentRun{ID: uuid.New(), Agent: "claude-code", CreatedBy: mpOwner, ModelProviderID: p.ID},
		site: types.SiteConfig{ModelProviders: providerBlock(p)}}
	h := mpHarness(t, st, p)
	_, _, grants, ok := mpDispatch(t, h, st, &types.RunPolicySpec{}, nil)
	if !ok || len(grants) != 1 {
		t.Fatal("dispatch failed")
	}
	rule, err := injectionRuleFromScope(grants[0].Spec.Scope)
	if err != nil {
		t.Fatal(err)
	}
	h.broker.minted = broker.Minted{Kind: types.GrantAPIKey, JTI: "review", Injection: &rule}
	return h, st, grants[0]
}

// Found by the 0.8 release review (F03): an external store's definitive
// denial answered 503, so the proxy served the revoked key through its grace.
func TestProviderKeySink_StoreAccessRevocationIsDefinitive(t *testing.T) {
	h, st, g := keySinkGrant(t)
	h.srv.cfg.Secrets = storeReadFails{h.srv.cfg.Secrets, errors.New("external store 403 access revoked")}
	rr := do(t, h.srv, http.MethodGet, "/api/v1/internal/injection/"+g.ID.String(), h.mintRunToken(t, st.run.ID), "")
	if rr.Code == http.StatusServiceUnavailable {
		t.Fatalf("definitive external-store denial returned 503: proxy serves last-good key through grace; body=%s", rr.Body.String())
	}
}

func TestProviderKeySink_StoreReadFailures(t *testing.T) {
	for _, tc := range storeReadFailures {
		t.Run(tc.name, func(t *testing.T) {
			h, st, g := keySinkGrant(t)
			h.srv.cfg.Secrets = storeReadFails{h.srv.cfg.Secrets, tc.err}
			rr := do(t, h.srv, http.MethodGet, "/api/v1/internal/injection/"+g.ID.String(), h.mintRunToken(t, st.run.ID), "")
			if want := wantStoreReadStatus(tc.err); rr.Code != want {
				t.Fatalf("status = %d %s, want %d", rr.Code, rr.Body.String(), want)
			}
			var d struct{ Reason string }
			_ = json.Unmarshal(lastAuditEvent(t, h.audit.events, "secret.read").Data, &d)
			if d.Reason != tc.keyReason {
				t.Fatalf("secret.read reason = %q, want %q", d.Reason, tc.keyReason)
			}
		})
	}
}
