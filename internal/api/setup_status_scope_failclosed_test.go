// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// siteCfgBlipStore is the roster read FAILING — a rotated age key, a PG blip, a
// context deadline — with every other store method still answering. Distinct
// from "no roster stored", which is a legitimate zero document.
type siteCfgBlipStore struct{ *integStore }

func (siteCfgBlipStore) GetSiteConfig(context.Context) (types.SiteConfig, error) {
	return types.SiteConfig{}, errors.New("site config read failed")
}

// assertNoOperatorSession: this caller's /setup/status says nothing about a
// credential that is not theirs — no captured aws harness row, and no grading
// that could only have come from a working blob.
func assertNoOperatorSession(t *testing.T, when string, st SetupStatus) {
	t.Helper()
	for _, h := range st.Harness {
		if h.Provider == awsSSOProvider && h.Captured {
			t.Errorf("%s: harness carries a captured aws row %+v — that session is the operator's, not this caller's", when, h)
		}
	}
	if s := st.ModelAccess.State; s == modelAccessLive || s == modelAccessExpiring {
		t.Errorf("%s: model_access.state = %q — graded from the operator's blob", when, s)
	}
}

// TestSetupStatus_FailedRosterReadNeverResolvesToTheOperatorNamespace is R-4.
//
// Under a per_user row the status read is caller-scoped, so a member sees their
// own capture and nothing else. But the scope is resolved FROM the roster, and
// awsSSOScopeFor's fallback for "no row" is the OPERATOR namespace — so when the
// site-config read merely FAILS, siteConfigSnapshot hands the handler a zero
// document and every caller, member included, reads the admin's harness row and
// the admin's model_access for the duration of the blip. That is also the one
// window in which a forged capture marker in a member's own login sandbox can be
// corroborated by a session that is not theirs (S-13 / R-1).
//
// A READ has no reason to take the write path's legacy-open fallback: fail the
// scope closed instead, on the caller's own subject.
func TestSetupStatus_FailedRosterReadNeverResolvesToTheOperatorNamespace(t *testing.T) {
	srv, _ := perUserLoginSrv(t) // claude-code / bedrock_sso / per_user, OIDC configured
	srv.cfg.Now = func() time.Time { return awsSSOTestFixedNow }
	putAWSSSOBlob(t, srv, awsSSOTestFixedNow.Add(time.Hour)) // the OPERATOR namespace

	member := ssoSession(t, "sub-member", "member@corp.example", oidc.RoleMember)

	// Control: with the roster READABLE, per_user already binds the read to the
	// member's own namespace. If this fails the fixture proves nothing below.
	code, st := decodeSetupSSO(t, srv, member)
	if code != http.StatusOK {
		t.Fatalf("roster readable: code = %d, want 200", code)
	}
	assertNoOperatorSession(t, "roster readable", st)

	// The blip.
	srv.cfg.Store = siteCfgBlipStore{srv.cfg.Store.(*integStore)}
	code, st = decodeSetupSSO(t, srv, member)
	if code != http.StatusOK {
		t.Fatalf("roster read failing: code = %d, want 200 — a blip must degrade, not 500", code)
	}
	assertNoOperatorSession(t, "roster read failing", st)
}

// TestRedactSetupStatusForMember_SourceRunIDOnlyOnTheCallersOwnRow is R-1's
// server half.
//
// harness-login-pane.tsx corroborates a forgeable PTY success marker against
// /setup/status, and the only field that says THIS sign-in captured something
// — as opposed to "a credential is there", which is true of every reconnect
// over an existing row — is the server-stamped source_run_id. A member has to
// be able to read it for their OWN capture, or the check degrades to the
// presence predicate it was written to replace.
//
// It is their own only under per_user. Under a shared/legacy row the aws blob
// is the ADMIN's, and so is the run id on it.
func TestRedactSetupStatusForMember_SourceRunIDOnlyOnTheCallersOwnRow(t *testing.T) {
	const runID = "11111111-2222-3333-4444-555555555555"
	full := SetupStatus{Harness: []SetupHarness{
		{Provider: awsSSOProvider, Captured: true, SourceRunID: runID, CapturedAt: "2026-01-02T03:04:05Z", Renewable: true},
		{Provider: "anthropic", Captured: true, SourceRunID: runID, Aging: true},
	}}

	own := redactSetupStatusForMember(full, true)
	if own.Harness[0].SourceRunID != runID {
		t.Errorf("per_user aws row source_run_id = %q, want %q — a member cannot corroborate their own sign-in without it",
			own.Harness[0].SourceRunID, runID)
	}
	if own.Harness[1].SourceRunID != "" {
		t.Errorf("anthropic row kept source_run_id %q — the managed blob is the operator's whoever asks",
			own.Harness[1].SourceRunID)
	}
	if own.Harness[0].CapturedAt != "" || own.Harness[0].Renewable {
		t.Errorf("the rest of the lifecycle detail rode through: %+v", own.Harness[0])
	}

	shared := redactSetupStatusForMember(full, false)
	for _, h := range shared.Harness {
		if h.SourceRunID != "" {
			t.Errorf("shared/legacy row %q kept source_run_id %q — that is the ADMIN's login run", h.Provider, h.SourceRunID)
		}
	}
}
