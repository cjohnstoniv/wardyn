// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/adoscope"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A one-time approval for a capability the run was not granted widens its token to carry it, spends the
// approval on the request that used it, and records which approval and capability it was. The same
// approval cannot be used again: the second request is asked afresh, and no second token is created.
func TestMiscCovOnceApprovalWidensTheTokenAndIsSpentOnce(t *testing.T) {
	fx := newADOPATFixture(t)
	fx.ok(t, "dev.azure.com", nil)
	ask := url.Values{"capability": {string(adoscope.CapPR)}, "first_use": {string(types.FirstUseWaitForReview)},
		"method": {"POST"}, "path": {"/contoso/proj/_apis/git/repositories/app/pullrequests"}}
	w, _ := fx.resolve(t, "dev.azure.com", ask)
	id := pendingID(t, w, adoCapabilityPendingState)
	if _, err := fx.fa.Decide(context.Background(), id, types.ActorHuman,
		types.ApprovalDecision{State: types.ApprovalApproved, Scope: types.ScopeOnce}); err != nil {
		t.Fatal(err)
	}
	ask.Set("approval", id.String())

	got := fx.ok(t, "dev.azure.com", ask)
	wantScope, _ := adoscope.PATScope([]adoscope.Capability{adoscope.CapCodeRead, adoscope.CapCodeWrite, adoscope.CapPR})
	creates := miscCovCreates(fx)
	if len(creates) != 2 || creates[1].Scope != wantScope {
		t.Fatalf("creates %+v; want a second token scoped %q", creates, wantScope)
	}
	if got.JTI == "" {
		t.Error("no token was handed out")
	}

	var read map[string]any
	rows := fx.audit.find("secret.read")
	if len(rows) == 0 {
		t.Fatal("no secret.read row")
	}
	if err := json.Unmarshal(rows[len(rows)-1].Data, &read); err != nil {
		t.Fatal(err)
	}
	if read["capability"] != string(adoscope.CapPR) || read["approval_id"] != id.String() {
		t.Errorf("secret.read data = %v, want capability %q and approval_id %s", read, adoscope.CapPR, id)
	}

	again, _ := fx.resolve(t, "dev.azure.com", ask)
	if again.Code != http.StatusLocked {
		t.Errorf("replaying the spent approval = %d %s, want it asked afresh (423)", again.Code, again.Body)
	}
	if fx.pats.createCount() != 2 {
		t.Errorf("a replayed approval created a token: %d creates", fx.pats.createCount())
	}
}

// A stale token whose replacement cannot be created is answered 503 with the mint denied, and the run's
// in-memory forced-mint mark holds: a second refusal inside the minute does not try to create again, it
// serves the token the run still holds. (The write of that mark to the state store is not pinned here.)
func TestMiscCovForcedMintFailureLeavesTheInMemoryMarkInPlace(t *testing.T) {
	fx := newADOPATFixture(t)
	first := fx.ok(t, "dev.azure.com", nil)
	fx.pats.createErr = &adoPATError{Status: http.StatusServiceUnavailable}
	w, _ := fx.resolve(t, "dev.azure.com", stale(first.JTI))
	if w.Code != http.StatusServiceUnavailable || errorReason(w) != reasonADOPATMintRefused {
		t.Fatalf("a stale resolve whose create failed = %d %s (%s), want 503 %s", w.Code, errorReason(w), w.Body, reasonADOPATMintRefused)
	}
	if got := fx.auditReasons(adoPATAuditMintDenied); len(got) != 1 || got[0] != adoPATMintUpstream401 {
		t.Fatalf("mint.denied reasons = %v, want the forced mint's", got)
	}

	fx.pats.createErr = nil
	fx.advance(30 * time.Second)
	again := fx.ok(t, "dev.azure.com", stale(first.JTI))
	if again.JTI != first.JTI || fx.pats.createCount() != 1 {
		t.Errorf("inside the minute: JTI %s after %d creates, want the held token and no new attempt", again.JTI, fx.pats.createCount())
	}
	if got := fmt.Sprint(fx.auditReasons(adoPATAuditMint)); got != fmt.Sprint([]string{adoPATMintDispatch}) {
		t.Errorf("mint reasons = %s, want only the dispatch's", got)
	}
}
