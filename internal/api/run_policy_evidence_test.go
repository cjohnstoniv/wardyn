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

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// policyEvidenceStore is pvPagedStore whose first audit read, or whose restart read, fails.
type policyEvidenceStore struct {
	pvPagedStore
	failEvents, failRevives bool
}

func (s policyEvidenceStore) QueryAuditEvents(ctx context.Context, runID uuid.UUID, limit int) ([]types.AuditEvent, error) {
	if s.failEvents {
		return nil, errors.New("audit read failed")
	}
	return s.pvPagedStore.QueryAuditEvents(ctx, runID, limit)
}

func (s policyEvidenceStore) QueryAuditEventsFilteredPage(ctx context.Context, runID *uuid.UUID, f store.AuditFilter, p store.Page) ([]types.AuditEvent, error) {
	if s.failRevives && f.ActionPrefix == "run.revive" {
		return nil, errors.New("restart read failed")
	}
	return s.pvPagedStore.QueryAuditEventsFilteredPage(ctx, runID, f, p)
}

// policyEvidenceSpec is the run's launch spec and envelope in both tests.
var policyEvidenceSpec = types.RunPolicySpec{AllowedDomains: []string{"api.anthropic.com"}, DataOut: types.TransferAllowed}

// TestRunPolicyViewServedDataOutFollowsTheRunRow: a run row whose data_out differs from the envelope's serves the
// row's rule on the policy view, and runPolicyServed gives the row's rule, or the envelope's for a row with none.
func TestRunPolicyViewServedDataOutFollowsTheRunRow(t *testing.T) {
	srv, st := pvFixture(t, &capStore{})
	blocked := types.TransferBlocked
	run := st.seedRun(pvOwner, types.RunRunning)
	run.DataOut = &blocked
	st.mu.Lock()
	st.runs[run.ID] = run
	st.mu.Unlock()
	st.rows(run.ID, pvCreate(nil, pvSource(policyKindInline, nil, "", false, policyEvidenceSpec)), pvResolve(policyEvidenceSpec, false))

	w, resp := pvMember(t, pvOwner).get(t, srv, run.ID)
	if w.Code != http.StatusOK {
		t.Fatalf("policy view: %d %s", w.Code, w.Body.String())
	}
	if resp.Spec.DataOut != types.TransferBlocked {
		t.Errorf("policy view data_out = %q, want the run row's %q", resp.Spec.DataOut, types.TransferBlocked)
	}

	ctx := context.Background()
	pe, err := srv.readRunPolicyEvidence(ctx, run.ID)
	if err != nil || pe.Create.Source == nil {
		t.Fatalf("readRunPolicyEvidence: %+v, %v", pe.Create, err)
	}
	envelope := effectivePolicyDatum{RunPolicySpec: policyEvidenceSpec}
	for _, tc := range []struct {
		row  *types.TransferRule
		want types.TransferRule
	}{{&blocked, types.TransferBlocked}, {nil, types.TransferAllowed}} {
		run.DataOut = tc.row
		_, served, err := srv.runPolicyServed(ctx, run, pe, envelope)
		if err != nil || served.DataOut != tc.want {
			t.Errorf("runPolicyServed with run row %v: data_out %q, %v; want %q", tc.row, served.DataOut, err, tc.want)
		}
	}
}

// TestRunPolicyViewEvidenceReadErrors: a failing first audit read answers 500 "read run policy", and a failing
// restart read answers 500 "read run restarts".
func TestRunPolicyViewEvidenceReadErrors(t *testing.T) {
	for _, tc := range []struct {
		name                    string
		failEvents, failRevives bool
		want                    string
	}{
		{"the first audit read", true, false, "read run policy"},
		{"the restart read", false, true, "read run restarts"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, st := pvFixture(t, &capStore{})
			srv.cfg.Store = policyEvidenceStore{pvPagedStore: pvPagedStore{pvStore: st}, failEvents: tc.failEvents, failRevives: tc.failRevives}
			run := st.seedRun(pvOwner, types.RunRunning)
			st.rows(run.ID, pvCreate(nil, pvSource(policyKindInline, nil, "", false, policyEvidenceSpec)), pvResolve(policyEvidenceSpec, false))
			w, _ := pvMember(t, pvOwner).get(t, srv, run.ID)
			var body errorBody
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode: %v (%s)", err, w.Body.String())
			}
			if w.Code != http.StatusInternalServerError || body.Error != tc.want || body.Reason != reasonInternalError {
				t.Errorf("%d %+v, want 500 %q %s", w.Code, body, tc.want, reasonInternalError)
			}
		})
	}
}
