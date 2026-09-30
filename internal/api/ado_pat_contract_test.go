// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// TestADOPATErrorReason pins how a refusal from the personal access token API
// maps to the reason the admin sees.
func TestADOPATErrorReason(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  adoPATError
		want string
	}{
		{"lifespan policy", adoPATError{Status: 400, PatTokenError: "patLifespanPolicyViolation"}, reasonADOPATLifespanPolicy},
		{"global policy", adoPATError{Status: 400, PatTokenError: "globalPatPolicyViolation"}, reasonADOPATPolicyBlocked},
		{"full-scope policy", adoPATError{Status: 400, PatTokenError: "fullScopePatPolicyViolation"}, reasonADOPATPolicyBlocked},
		{"access denied", adoPATError{Status: 403, PatTokenError: "accessDenied"}, reasonADOPATPolicyBlocked},
		{"a named error beats the status", adoPATError{Status: 401, PatTokenError: "patLifespanPolicyViolation"}, reasonADOPATLifespanPolicy},
		{"401 with no named error", adoPATError{Status: 401, ServiceError: "TF400813"}, reasonADOPATConsentNeeded},
		{"403 with no named error", adoPATError{Status: 403}, reasonADOPATConsentNeeded},
		{"the success value with a 401", adoPATError{Status: 401, PatTokenError: "none"}, reasonADOPATConsentNeeded},
		{"the success value with a 500", adoPATError{Status: 500, PatTokenError: "none"}, reasonADOPATMintRefused},
		{"an invalid scope", adoPATError{Status: 400, PatTokenError: "invalidScope"}, reasonADOPATMintRefused},
		{"an invalid expiry", adoPATError{Status: 400, PatTokenError: "invalidValidTo"}, reasonADOPATMintRefused},
		{"a 500", adoPATError{Status: 500}, reasonADOPATMintRefused},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.err.Reason(); got != tc.want {
				t.Errorf("Reason() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestADOPATErrorIsAnError: an *adoPATError travels as an error and is found
// through wrapping, and its text carries no token.
func TestADOPATErrorIsAnError(t *testing.T) {
	var err error = &adoPATError{Status: 401, ServiceError: "TF400813", PatTokenError: "accessDenied"}
	var got *adoPATError
	if !errors.As(err, &got) || got.Status != 401 {
		t.Fatalf("errors.As = %v", got)
	}
	for _, want := range []string{"401", "TF400813", "accessDenied"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Error() = %q, want it to contain %q", err.Error(), want)
		}
	}
}

// TestADOPATVocabulary pins the wire spellings other lanes and the audit log
// depend on, and that the client contract has no update.
func TestADOPATVocabulary(t *testing.T) {
	for name, pair := range map[string][2]string{
		"mint dispatch": {adoPATMintDispatch, "dispatch"}, "mint renewal": {adoPATMintRenewal, "renewal"},
		"mint widen": {adoPATMintWiden, "widen"}, "mint resume": {adoPATMintResume, "resume"},
		"mint restart":   {adoPATMintRestart, "restart"},
		"revoke run_end": {adoPATRevokeRunEnd, "run_end"}, "revoke kill": {adoPATRevokeKill, "kill"},
		"revoke pause": {adoPATRevokePause, "pause"}, "revoke drift": {adoPATRevokeDrift, "drift"},
		"revoke expired":    {adoPATRevokeExpired, "expired"},
		"revoke disconnect": {adoPATRevokeDisconnect, "disconnect"}, "revoke sweep": {adoPATRevokeSweep, "sweep"},
		"audit mint": {adoPATAuditMint, "ado_pat.mint"}, "audit revoke": {adoPATAuditRevoke, "ado_pat.revoke"},
		"audit mint denied":    {adoPATAuditMintDenied, "ado_pat.mint.denied"},
		"audit revoke failed":  {adoPATAuditRevokeFailed, "ado_pat.revoke.failed"},
		"audit org check":      {adoPATAuditOrgCheck, "ado_pat.org_check"},
		"audit own store":      {adoPATAuditOwnStore, "ado_pat.own.store"},
		"audit own delete":     {adoPATAuditOwnDelete, "ado_pat.own.delete"},
		"audit own mismatch":   {adoPATAuditOwnMismatch, "ado_pat.own.identity_mismatch"},
		"audit bearer refused": {adoBearerAuditRefusedMint, "ado_bearer.refused_mint_scopes"},
		"audit retire":         {adoSharedCredentialRetire, "ado_shared_credential.retire"},
		"reason policy":        {reasonADOPATPolicyBlocked, "ado_pat_policy_blocked"},
		"reason lifespan":      {reasonADOPATLifespanPolicy, "ado_pat_lifespan_policy"},
		"reason consent":       {reasonADOPATConsentNeeded, "ado_pat_consent_needed"},
		"reason refused":       {reasonADOPATMintRefused, "ado_pat_mint_refused"},
	} {
		if pair[0] != pair[1] {
			t.Errorf("%s = %q, want %q", name, pair[0], pair[1])
		}
	}
	var _ adoPATClient = fakeADOPATClient{}
	_ = adoPAT{}
	_ = adoPATRequest{}
}

// TestADOPATNeverPrintsTheToken: the secret does not survive %v, %+v, %#v-free
// formatting or a structured log line.
func TestADOPATNeverPrintsTheToken(t *testing.T) {
	p := adoPAT{AuthorizationID: "a-1", Token: "s3cret", Scope: "vso.code", ValidTo: time.Now().Add(time.Hour).UTC()}
	var buf strings.Builder
	slog.New(slog.NewTextHandler(&buf, nil)).Info("minted", "pat", p)
	for name, got := range map[string]string{
		"%v": fmt.Sprintf("%v", p), "%+v": fmt.Sprintf("%+v", p), "%s": fmt.Sprintf("%s", p), "slog": buf.String(),
	} {
		if strings.Contains(got, "s3cret") {
			t.Errorf("%s printed the token: %q", name, got)
		}
		if !strings.Contains(got, "vso.code") {
			t.Errorf("%s = %q, want the scope", name, got)
		}
	}
}

type fakeADOPATClient struct{ adoPATClient }
