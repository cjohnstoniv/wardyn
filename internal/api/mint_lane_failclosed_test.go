// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/broker"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// erroringGrantsStore fails ListGrantsByRun the way a transient store hiccup
// does; every other method panics if reached (convention: grantsStore).
type erroringGrantsStore struct{ store.Store }

func (erroringGrantsStore) ListGrantsByRun(context.Context, uuid.UUID) ([]types.CredentialGrant, error) {
	return nil, errors.New("store: connection reset by peer")
}

// TestInternalMint_GrantListErrorFailsClosed pins F098. brokeredForgeMintKind is
// the mint-time residual check for a policy STORED BEFORE
// validateGrantLaneExclusivity: an ssh_key/git_pat grant row for a brokered forge
// that anyone who learns the grant id can still mint. For exactly that residual
// this check is the ONLY belt — the broker's own checks (ownership, approval,
// no-widening) do not cover it — and it used to fail OPEN on a ListGrantsByRun
// error, answering the mint with the raw credential for the length of a store
// hiccup. Requirement 6 of the round brief states it "fails CLOSED on a store
// error"; the code now agrees.
//
// RED on the base tree: 200 with a live credential in the body.
//
// The nil-Store row is here so the fix cannot be over-applied: a missing Store is
// not a failure, it is the configuration in which no persisted pre-exclusivity
// policy can exist, and every newHarness-based test runs that way.
func TestInternalMint_GrantListErrorFailsClosed(t *testing.T) {
	sshID, runID := uuid.New(), uuid.New()
	grant := types.CredentialGrant{ID: sshID, RunID: runID, Spec: types.GrantSpec{
		Kind:  types.GrantSSHKey,
		Scope: mustJSON(map[string]any{"host": "github.com", "key_secret_ref": "deploy-key"}),
	}}

	t.Run("a grant-list error refuses the mint", func(t *testing.T) {
		h := newHarness(t)
		h.srv.cfg.Store = erroringGrantsStore{}
		tok := h.mintRunToken(t, runID)
		w := do(t, h.srv, http.MethodPost, "/api/v1/internal/credentials/mint", tok,
			`{"grant_id":"`+sshID.String()+`"}`)
		if w.Code != http.StatusServiceUnavailable {
			t.Fatalf("mint with an unreadable grant list = %d, want 503; body=%s", w.Code, w.Body.String())
		}
		if strings.Contains(w.Body.String(), "github_token grant") {
			t.Errorf("unverifiable refusal reused the single-lane message, which asserts a grant this request never observed: %s", w.Body.String())
		}
		var denied bool
		for _, ev := range h.audit.events {
			if ev.Action == "credential.mint" && ev.Outcome == "denied" &&
				strings.Contains(string(ev.Data), "brokered_forge_single_lane_unverifiable") {
				denied = true
			}
		}
		if !denied {
			t.Error("no credential.mint/denied audit row naming the unverifiable refusal")
		}
	})

	t.Run("no Store still fails OPEN", func(t *testing.T) {
		h := newHarness(t) // newHarness configures no Store
		tok := h.mintRunToken(t, runID)
		w := do(t, h.srv, http.MethodPost, "/api/v1/internal/credentials/mint", tok,
			`{"grant_id":"`+sshID.String()+`"}`)
		if w.Code != http.StatusOK {
			t.Fatalf("mint with no Store = %d, want 200 (defence in depth, not a gate); body=%s", w.Code, w.Body.String())
		}
	})

	t.Run("a readable grant list still refuses the single lane", func(t *testing.T) {
		h := newHarness(t)
		h.srv.cfg.Store = grantsStore{grants: []types.CredentialGrant{grant, {
			ID: uuid.New(), RunID: runID, Spec: types.GrantSpec{
				Kind:  types.GrantGitHubToken,
				Scope: mustJSON(map[string]any{"repos": []string{"acme/widgets"}}),
			},
		}}}
		tok := h.mintRunToken(t, runID)
		w := do(t, h.srv, http.MethodPost, "/api/v1/internal/credentials/mint", tok,
			`{"grant_id":"`+sshID.String()+`"}`)
		if w.Code != http.StatusForbidden {
			t.Fatalf("co-granted brokered ssh_key = %d, want 403; body=%s", w.Code, w.Body.String())
		}
	})
}

// TestInternalMintConflictCodesAreTheWireContract pins F134. The four mint-409
// "code" values are the discriminator cmd/wardyn-git-helper switches on to name
// the real cause of a conflict. Every server-side assertion compared the decoded
// JSON against the SAME package constant the handler had written, so all four
// could be renamed with the suite green, and the literals appeared in no test,
// fixture or doc anywhere in the tree. This table asserts the BYTES, end to end,
// including the denied arm, which had no test at all (writeMintError's
// ErrApprovalDenied block showed count=0 in the coverage profile even though the
// helper branches on it).
//
// RED on the base tree only under a rename mutation — which is exactly the
// property it exists to restore. It is GREEN on both trees as written, and that
// is the point: it is the assertion the base tree was missing, not a bug report.
func TestInternalMintConflictCodesAreTheWireContract(t *testing.T) {
	apID := uuid.New()
	for _, tc := range []struct {
		name     string
		err      error
		wantCode string
		wantBody map[string]any
	}{
		{"pending", broker.ErrApprovalPending{ApprovalID: apID}, "pending",
			map[string]any{"approval_id": apID.String()}},
		{"denied", broker.ErrApprovalDenied{ApprovalID: apID, Reason: "not this repo"}, "denied",
			map[string]any{"approval_id": apID.String(), "denied": true, "reason": "not this repo"}},
		{"scope_mismatch", broker.ErrScopeMismatch, "scope_mismatch", nil},
		{"already_minted", broker.ErrAlreadyMinted, "already_minted", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			tok := h.mintRunToken(t, uuid.New())
			h.broker.mintErr = tc.err
			w := do(t, h.srv, http.MethodPost, "/api/v1/internal/credentials/mint", tok,
				`{"grant_id":"`+uuid.New().String()+`"}`)
			if w.Code != http.StatusConflict {
				t.Fatalf("status = %d, want 409; body=%s", w.Code, w.Body.String())
			}
			var resp map[string]any
			if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if resp["code"] != tc.wantCode {
				t.Errorf("code = %v, want %q (the literal on the wire, not the constant)", resp["code"], tc.wantCode)
			}
			for k, want := range tc.wantBody {
				if resp[k] != want {
					t.Errorf("%s = %v, want %v", k, resp[k], want)
				}
			}
		})
	}
}
