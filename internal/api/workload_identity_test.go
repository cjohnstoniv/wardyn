// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/identity"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

var testRunWorkload = identity.Workload{Owner: "alice@example.com", Name: "web"}

// testWorkloadRunID is the SPIFFE ID the harness's provider gives runID as a run of testRunWorkload.
func testWorkloadRunID(t *testing.T, runID uuid.UUID) string {
	t.Helper()
	path, err := identity.RunPath(testRunWorkload, runID)
	if err != nil {
		t.Fatal(err)
	}
	return "spiffe://wardyn.local" + path
}

// A workload's launch carries the workload into the authority its queued run is stored with, through the
// stored JSON, so the start mints the run as the workload's; any other launch carries none.
func TestQueueAuthority_CarriesTheLaunchsWorkload(t *testing.T) {
	ctx := withLaunchWorkload(context.Background(), store.Workload{Owner: testRunWorkload.Owner, Name: testRunWorkload.Name})
	raw, err := json.Marshal(captureQueueAuthority(ctx))
	if err != nil {
		t.Fatal(err)
	}
	var back queueAuthority
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if got := back.workload(); got != testRunWorkload {
		t.Fatalf("stored authority's workload = %+v, want %+v", got, testRunWorkload)
	}
	if a := captureQueueAuthority(context.Background()); a.workload() != (identity.Workload{}) {
		t.Fatalf("a launch for no workload stored %+v", a.workload())
	}
}

// The workload is read back from a run's SPIFFE ID only when the ID is the run's own.
func TestRunWorkloadOf(t *testing.T) {
	runID := uuid.New()
	for name, tc := range map[string]struct {
		id   string
		want identity.Workload
	}{
		"a workload's run":     {testWorkloadRunID(t, runID), testRunWorkload},
		"an ad-hoc run":        {"spiffe://wardyn.local/agent-run/" + runID.String(), identity.Workload{}},
		"another run":          {testWorkloadRunID(t, uuid.New()), identity.Workload{}},
		"not a run's":          {"spiffe://wardyn.local/workload/alice/web", identity.Workload{}},
		"not a SPIFFE ID":      {"alice/web", identity.Workload{}},
		"no ID (a legacy run)": {"", identity.Workload{}},
	} {
		if got := runWorkloadOf(tc.id, runID); got != tc.want {
			t.Errorf("%s: runWorkloadOf(%q) = %+v, want %+v", name, tc.id, got, tc.want)
		}
	}
}

// narrowIdentity is a Provider that cannot mint for a workload's run.
type narrowIdentity struct{ identity.Provider }

// A workload's run is minted under the workload's SPIFFE ID; any other run under its own; and a provider
// that cannot name a workload's run refuses it rather than mint it as an ad-hoc run.
func TestMintRunIdentityNamesAWorkloadsRun(t *testing.T) {
	h := newHarness(t)
	ctx, runID := context.Background(), uuid.New()
	id, err := h.srv.mintRunIdentity(ctx, runID, testRunWorkload, "alice@example.com", "", false)
	if err != nil || id.SPIFFEID != testWorkloadRunID(t, runID) {
		t.Fatalf("a workload's run = %q, %v; want %q", id.SPIFFEID, err, testWorkloadRunID(t, runID))
	}
	adHoc := uuid.New()
	if id, err := h.srv.mintRunIdentity(ctx, adHoc, identity.Workload{}, "alice@example.com", "", false); err != nil || id.SPIFFEID != "spiffe://wardyn.local/agent-run/"+adHoc.String() {
		t.Fatalf("an ad-hoc run = %q, %v", id.SPIFFEID, err)
	}
	h.srv.cfg.Identity = narrowIdentity{h.srv.cfg.Identity}
	if id, err := h.srv.mintRunIdentity(ctx, uuid.New(), testRunWorkload, "alice@example.com", "", false); !errors.Is(err, errNoWorkloadMint) || id.Token != "" {
		t.Fatalf("a narrow provider = %+v, %v; want errNoWorkloadMint and no token", id, err)
	}
}

// A revive mints the run under the ID it started with.
func TestReviveMintKeepsAWorkloadsID(t *testing.T) {
	h := newHarness(t)
	run := types.AgentRun{ID: uuid.New(), CreatedBy: "alice@example.com", State: types.RunRunning}
	run.SPIFFEID = testWorkloadRunID(t, run.ID)
	id, rerr := h.srv.reviveMintIdentity(context.Background(), run)
	if rerr != nil || id.SPIFFEID != run.SPIFFEID {
		t.Fatalf("revive = %q, %v; want %q", id.SPIFFEID, rerr, run.SPIFFEID)
	}
}

// A renewal mints the run under the ID its token carries.
func TestTokenRenewKeepsAWorkloadsID(t *testing.T) {
	h, srv, _, runID := newRenewHarness(t)
	old, err := h.idp.MintWorkloadRunIdentity(context.Background(), runID, testRunWorkload, "alice@example.com", "", internalAudience, false)
	if err != nil {
		t.Fatal(err)
	}
	w := do(t, srv, http.MethodPost, "/api/v1/internal/token/renew", old.Token, "")
	if w.Code != http.StatusOK {
		t.Fatalf("renew = %d %s", w.Code, w.Body)
	}
	var got tokenRenewResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	claims, err := h.idp.Verify(context.Background(), got.Token, internalAudience)
	if err != nil || claims.SPIFFEID != old.SPIFFEID {
		t.Fatalf("renewed token's id = %q, %v; want %q", claims.SPIFFEID, err, old.SPIFFEID)
	}
}
