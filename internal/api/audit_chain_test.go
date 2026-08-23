// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/store"
)

// chainStore is a Store that CAN verify a chain. The embedded nil interface is
// the same trick capStore uses: only the methods under test are real.
type chainStore struct {
	store.Store
	st  store.AuditChainStatus
	err error
}

func (s *chainStore) VerifyAuditChain(context.Context) (store.AuditChainStatus, error) {
	return s.st, s.err
}

// plainStore is a Store that CANNOT verify a chain (no PG behind it) — the
// optional-interface miss the handler must answer 501 for.
type plainStore struct{ store.Store }

func chainServer(t *testing.T, st store.Store) *Server {
	t.Helper()
	cfg := baseTestConfig(newHarness(t), st)
	cfg.OIDC = &oidc.Authenticator{}
	return New(cfg)
}

// TestVerifyAuditChainRoute pins the three things the route promises: it is
// admin-only, a broken chain is a 200 finding rather than a 5xx, and a store
// with no chain says so instead of reporting a clean sweep it never ran.
func TestVerifyAuditChainRoute(t *testing.T) {
	const path = "/api/v1/audit/chain/verify"

	t.Run("member is refused", func(t *testing.T) {
		srv := chainServer(t, &chainStore{st: store.AuditChainStatus{OK: true}})
		if w := doSSO(t, srv, http.MethodGet, path, permMember(t), ""); w.Code != http.StatusForbidden {
			t.Errorf("member code = %d, want 403 (whole-deployment audit volume is an admin disclosure)", w.Code)
		}
	})

	t.Run("clean chain", func(t *testing.T) {
		srv := chainServer(t, &chainStore{st: store.AuditChainStatus{
			OK: true, Checked: 12, HeadSeq: 12, HeadHash: "deadbeef",
		}})
		w := doSSO(t, srv, http.MethodGet, path, permAdmin(t), "")
		if w.Code != http.StatusOK {
			t.Fatalf("code = %d, want 200: %s", w.Code, w.Body.String())
		}
		var got store.AuditChainStatus
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if !got.OK || got.HeadHash != "deadbeef" {
			t.Errorf("body = %+v, want ok with the head hash an operator compares off-box", got)
		}
	})

	t.Run("broken chain is a 200 finding, not a 5xx", func(t *testing.T) {
		srv := chainServer(t, &chainStore{st: store.AuditChainStatus{
			OK: false, Checked: 3, BrokenSeq: 42, Reason: "row_hash does not match",
		}})
		w := doSSO(t, srv, http.MethodGet, path, permAdmin(t), "")
		if w.Code != http.StatusOK {
			t.Fatalf("code = %d, want 200 — the sweep succeeded, it found something", w.Code)
		}
		var got store.AuditChainStatus
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if got.OK || got.BrokenSeq != 42 {
			t.Errorf("body = %+v, want ok=false naming seq 42", got)
		}
	})

	t.Run("store with no chain answers 501", func(t *testing.T) {
		srv := chainServer(t, &plainStore{})
		if w := doSSO(t, srv, http.MethodGet, path, permAdmin(t), ""); w.Code != http.StatusNotImplemented {
			t.Errorf("code = %d, want 501 (never report a clean sweep that did not run)", w.Code)
		}
	})
}
