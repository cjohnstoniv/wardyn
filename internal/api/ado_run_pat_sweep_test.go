// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/adoscope"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A revoke Azure DevOps answered with a transient status (a rate limit above
// all) keeps the record open, so the sweep revokes the token once Azure
// DevOps recovers. A status that is a verdict closes it with the error.
func TestRunPATRevokeTransientStatusStaysRetryable(t *testing.T) {
	for _, tc := range []struct {
		status    int
		retryable bool
	}{
		{http.StatusRequestTimeout, true},
		{http.StatusTooEarly, true},
		{http.StatusTooManyRequests, true},
		{http.StatusInternalServerError, true},
		{http.StatusServiceUnavailable, true},
		{http.StatusBadRequest, false},
		{http.StatusForbidden, false},
	} {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			ctx := context.Background()
			fx := newADOPATFixture(t)
			first := fx.ok(t, "dev.azure.com", nil)
			fx.st.edit(fx.run.ID, func(r *types.AgentRun) { r.State = types.RunCompleted })
			fx.pats.revokeErr = &adoPATError{Status: tc.status}
			fx.srv.revokeRunPATs(ctx, fx.run.ID, adoPATRevokeRunEnd)
			open := fx.st.unrevoked(t)
			if tc.retryable && (len(open) != 1 || open[0].LastError == "") || !tc.retryable && len(open) != 0 {
				t.Fatalf("HTTP %d: %d rows open after the failed revoke, want retryable=%v", tc.status, len(open), tc.retryable)
			}
			fx.pats.revokeErr = nil
			fx.advance(adoRunPATSweepEvery + time.Second)
			if err := fx.srv.sweepRunPATs(ctx); err != nil {
				t.Fatal(err)
			}
			if _, live := fx.pats.live[first.JTI]; live == tc.retryable {
				t.Fatalf("HTTP %d: token live=%v after recovery and a sweep, want live=%v", tc.status, live, !tc.retryable)
			}
		})
	}
}

// A transient revoke failure on a run that stays live (a drift refusal) is
// retried by the sweep, not left until the run ends.
func TestRunPATSweepRetriesTransientRevokeOnLiveRun(t *testing.T) {
	ctx := context.Background()
	fx := newADOPATFixture(t)
	first := fx.ok(t, "dev.azure.com", nil)
	fx.pats.revokeErr = &adoPATError{Status: http.StatusServiceUnavailable}
	fx.st.site.WorkspaceProviders.Git[0].Entra.CapabilityCeiling = []adoscope.Capability{adoscope.CapCodeRead}
	if w, _ := fx.resolve(t, "dev.azure.com", nil); w.Code != http.StatusForbidden {
		t.Fatalf("drifted resolve = %d, want 403", w.Code)
	}
	if open := fx.st.unrevoked(t); len(open) != 1 || open[0].LastError == "" {
		t.Fatalf("failed drift revoke not recorded: %+v", open)
	}
	fx.pats.revokeErr = nil
	fx.advance(adoRunPATSweepEvery + time.Second)
	if err := fx.srv.sweepRunPATs(ctx); err != nil {
		t.Fatal(err)
	}
	if _, live := fx.pats.live[first.JTI]; live || len(fx.st.unrevoked(t)) != 0 {
		t.Fatalf("token live=%v open rows=%d after the sweep, want revoked and closed", live, len(fx.st.unrevoked(t)))
	}
	if run, _ := fx.st.GetRun(ctx, fx.run.ID); run.State != types.RunRunning {
		t.Fatalf("run state = %s, want it still RUNNING", run.State)
	}
}

// The sweep touches nothing of a live run's older tokens that carry no
// pending revoke failure: a renewal's predecessor lives to its validTo.
func TestRunPATSweepLeavesRetainedTokensOnLiveRun(t *testing.T) {
	ctx := context.Background()
	fx := newADOPATFixture(t)
	first := fx.ok(t, "dev.azure.com", nil)
	fx.setClock(time.UnixMilli(first.ExpiresAt).Add(-adoRunPATRenewWindow + time.Minute))
	renewed := fx.ok(t, "vssps.dev.azure.com", nil)
	if renewed.JTI == first.JTI || len(fx.st.unrevoked(t)) != 2 {
		t.Fatalf("renewal: JTI %s, %d open rows, want a second token beside the first", renewed.JTI, len(fx.st.unrevoked(t)))
	}
	fx.advance(adoRunPATSweepEvery + time.Second)
	if err := fx.srv.sweepRunPATs(ctx); err != nil {
		t.Fatal(err)
	}
	if n := len(fx.pats.revokedIDs()); n != 0 || len(fx.st.unrevoked(t)) != 2 || len(fx.pats.live) != 2 {
		t.Fatalf("sweep revoked %d, left %d open rows and %d live tokens, want 0, 2 and 2", n, len(fx.st.unrevoked(t)), len(fx.pats.live))
	}
}

// A create answered with a transient status is a 503 the proxy retries, not a
// 403 refusal.
func TestAdoRunPATRefusalTransientIs503(t *testing.T) {
	for _, status := range []int{http.StatusRequestTimeout, http.StatusTooEarly, http.StatusTooManyRequests, http.StatusBadGateway} {
		if got, _, _ := adoRunPATRefusal(&adoPATError{Status: status}); got != http.StatusServiceUnavailable {
			t.Errorf("HTTP %d answered %d, want 503", status, got)
		}
	}
	if got, _, _ := adoRunPATRefusal(&adoPATError{Status: http.StatusForbidden}); got != http.StatusForbidden {
		t.Errorf("HTTP 403 answered %d, want 403", got)
	}
}
