// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// ownListFaultSecrets answers the operator namespace from the wrapped store and
// fails every person's own List, counting the attempts: the transient fault an
// ownership proof must answer as itself (#3506).
type ownListFaultSecrets struct {
	secretstore.Store
	reads *atomic.Int64
	own   bool
}

func (f ownListFaultSecrets) For(owner string) secretstore.Store {
	f.own = owner != ""
	return f
}

func (f ownListFaultSecrets) List(ctx context.Context) ([]string, error) {
	if !f.own {
		return f.Store.List(ctx)
	}
	f.reads.Add(1)
	return nil, errors.New("backend unavailable")
}

func newOwnListFaultFixture(t *testing.T) (*componentFixture, *atomic.Int64) {
	t.Helper()
	f := newComponentFixture(t)
	reads := &atomic.Int64{}
	f.srv.cfg.Secrets = ownListFaultSecrets{Store: f.srv.cfg.Secrets, reads: reads}
	return f, reads
}

func wantOwnershipUnreadable(t *testing.T, what string, ref *runRefusal) {
	t.Helper()
	if ref == nil || ref.status != http.StatusServiceUnavailable || ref.body.Reason != reasonSinkStoreUnavailable || ref.body.Error != errSecretStoreUnavailable.Error() {
		t.Fatalf("%s: refusal %+v, want the fixed 503 %s", what, ref, reasonSinkStoreUnavailable)
	}
}

// A person's own grant whose ownership the store could not answer is the fixed
// retryable 503, never operator material, and the request's memo holds the
// fault: every later proof for that owner answers it with no second read.
func TestLocalGrantOriginStoreFaultIsRetryable503ReadOnce(t *testing.T) {
	f, reads := newOwnListFaultFixture(t)
	ctx := withOwnedSecretMemo(context.Background())
	ssh := types.GrantSpec{Kind: types.GrantSSHKey, Scope: mustJSON(map[string]any{"host": "ssh.example", "key_secret_ref": compOwnSecret})}
	for i := range 3 {
		origin, ref := f.srv.localGrantOrigin(ctx, capSub, ssh)
		wantOwnershipUnreadable(t, "grant", ref)
		if origin.Class == placement.ClassOperator || origin.OwnNamespace {
			t.Fatalf("ask %d: origin %+v, want no classification on a store fault", i, origin)
		}
	}
	own := adoEntraRun{rowID: "ado-row-1", owner: capSub, tokenMode: types.ADOTokenModeOwnPAT}
	if _, err := f.srv.localADOOrigin(ctx, capSub, own); !errors.Is(err, errSecretStoreUnavailable) {
		t.Fatalf("ADO own PAT under the same memo: err %v, want %v", err, errSecretStoreUnavailable)
	}
	if n := reads.Load(); n != 1 {
		t.Fatalf("own-namespace reads = %d, want 1 (the fault is memoized per owner, not retried per grant)", n)
	}
	if err := ownershipFault(ownershipUnreadable()); !errors.Is(err, errSecretStoreUnavailable) {
		t.Fatalf("ownershipFault(ownershipUnreadable()) = %v", err)
	}
	if err := ownershipFault(runError(placement.ReasonPlacementCredential.Status(), string(placement.ReasonPlacementCredential), "x")); err != nil {
		t.Fatalf("a credential refusal is not a store fault: %v", err)
	}
}

// The create-time provider and Azure DevOps intents carry the fault to the run
// door as the fixed 503, with no memo installed too.
func TestLocalCredentialIntentsStoreFaultIsRetryable503(t *testing.T) {
	f, _ := newOwnListFaultFixture(t)
	r := httptest.NewRequest(http.MethodPost, "/api/v1/runs", nil).WithContext(utCtx("user", "", nil))
	p := types.ModelProvider{ID: "selected", UID: "uid", Kind: types.ModelProviderAnthropicAPIKey}
	if _, known, err := f.srv.localProviderIntent(r, capSub, p); !known || !errors.Is(err, errSecretStoreUnavailable) {
		t.Fatalf("provider intent: known=%v err=%v, want %v", known, err, errSecretStoreUnavailable)
	}
	own := adoEntraRun{rowID: "ado-row-1", owner: capSub, tokenMode: types.ADOTokenModeOwnPAT}
	if _, err := f.srv.localADOOrigin(r.Context(), capSub, own); !errors.Is(err, errSecretStoreUnavailable) {
		t.Fatalf("ADO own PAT: err %v, want %v", err, errSecretStoreUnavailable)
	}
	// A minted token proves no ownership and so never asks the store.
	if _, err := f.srv.localADOOrigin(r.Context(), capSub, adoEntraRun{rowID: "ado-row-1", owner: capSub}); err != nil {
		t.Fatalf("minted token: %v", err)
	}
}
