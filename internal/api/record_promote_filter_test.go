// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/recordmode"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestPromoteEntryReject covers the ARMS record_probe_f6_test.go's end-to-end
// gaps do not reach on their own: the verify:<key> half of the confined refusal
// (an entry keyed as a replay but not flagged Confined — the two are written by
// different hops, so neither alone may be the gate), and the control that a
// plain settled open recording is still promotable.
func TestPromoteEntryReject(t *testing.T) {
	truncated := RecordTaskResult{Status: recordStatusRecorded, Caveats: []string{recordMaskingCaveat, captureAuditTruncatedNote}}
	for _, tc := range []struct {
		name    string
		key     string
		res     RecordTaskResult
		refused bool
	}{
		{"open, complete", "build", RecordTaskResult{Status: recordStatusRecorded, Caveats: []string{recordMaskingCaveat}}, false},
		{"confined flag", "build", RecordTaskResult{Status: recordStatusRecorded, Confined: true}, true},
		{"verify key, flag unset", recordVerifyKeyPrefix + "build", RecordTaskResult{Status: recordStatusRecorded}, true},
		{"truncated", "build", truncated, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			why := promoteEntryReject(tc.key, tc.res)
			if refused := why != ""; refused != tc.refused {
				t.Fatalf("promoteEntryReject(%q) = %q, want refused=%v", tc.key, why, tc.refused)
			}
		})
	}
}

// TestPromotableHosts_SkipEntryShapes pins the per-host half over the entry
// SHAPES a skip set really carries — a wildcard, a port-qualified entry, a
// trailing dot — plus the IP-literal refusal. The exact-map lookup this
// replaced matched only the third of those, which is how a wildcard ceiling
// offered its own model-provider host for promotion.
func TestPromotableHosts_SkipEntryShapes(t *testing.T) {
	obs := recordmode.Observations{Domains: []recordmode.DomainObservation{
		{Host: "api.stripe.com", AllowCount: 1},          // a real need
		{Host: "api.anthropic.com", AllowCount: 9},       // under *.anthropic.com
		{Host: "corp.example", AllowCount: 2},            // under corp.example:443
		{Host: "gitlab.internal", AllowCount: 2},         // under gitlab.internal.
		{Host: "93.184.216.34", AllowCount: 1},           // public IP literal
		{Host: "wardyn.example.internal", AllowCount: 4}, // the control plane
		{Host: "denied.example.com", AllowCount: 3},      // deny·always
		{Host: "never.example.com", PendingCount: 5},     // never allowed
	}}
	skip := map[string]struct{}{
		"*.anthropic.com":    {},
		"corp.example:443":   {},
		"gitlab.internal.":   {},
		"denied.example.com": {},
	}
	got := promotableHosts(&obs, "wardyn.example.internal", skip)
	if len(got) != 1 {
		t.Fatalf("promotableHosts = %v, want exactly {api.stripe.com}", got)
	}
	if _, ok := got["api.stripe.com"]; !ok {
		t.Fatalf("promotableHosts = %v, want api.stripe.com — the one observed host that is a genuine need", got)
	}
}

// TestPromoteRecordEgress_RequestHostIsAlsoSelf pins the documented fallback:
// the name the promote request ARRIVED on counts as "us" too, so a deployment
// dialled by an ingress/service name other than the configured ControlPlaneURL
// still cannot promote its own API host. Narrowing-only by construction — the
// header can suppress a promotion, never cause one.
func TestPromoteRecordEgress_RequestHostIsAlsoSelf(t *testing.T) {
	runID, wsID := uuid.New(), uuid.New()
	const arrivedOn = "wardyn.ingress.corp.example"
	obs := recordmode.Observations{Domains: []recordmode.DomainObservation{
		{Host: "api.stripe.com", AllowCount: 1},
		{Host: arrivedOn, AllowCount: 6},
	}}
	fake := &recordStore{importStateFake: importStateFake{ws: types.Workspace{
		ID:      wsID,
		Sources: []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeLocalDir, Path: "/w", Target: "/home/agent/work"}},
		Status:  types.WorkspaceScanned,
		RecordResults: mustJSON(map[string]RecordTaskResult{"build": {
			RunID: runID, Mode: recordModeInteractive, Status: recordStatusRecorded, Observations: &obs,
		}}),
	}}}
	srv := newTestSrv(t, fake)

	r := httptest.NewRequest(http.MethodPost, "/api/v1/workspaces/"+wsID.String()+"/record/build/promote-egress", nil)
	r.Header.Set("Authorization", "Bearer "+adminToken)
	r.Host = arrivedOn + ":8443" // the port must not defeat the match
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("promote: code = %d, want 200; body = %s", w.Code, w.Body.String())
	}
	if _, leaked := fake.ws.Requirements["egress:"+arrivedOn]; leaked {
		t.Fatalf("the host this request arrived on was promoted as a required row: %v", fake.ws.Requirements)
	}
	if len(fake.ws.Requirements) != 1 || fake.ws.Requirements["egress:api.stripe.com"].Level != "required" {
		t.Fatalf("requirements = %v, want exactly egress:api.stripe.com", fake.ws.Requirements)
	}
}
