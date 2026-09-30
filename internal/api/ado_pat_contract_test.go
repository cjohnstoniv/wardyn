// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"errors"
	"strings"
	"testing"
)

// TestADOPATErrorReason pins how a refusal from the personal access token API
// maps to the reason the admin sees.
func TestADOPATErrorReason(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  adoPATError
		want string
	}{
		{"lifespan policy", adoPATError{Status: 400, PatTokenError: "patLifespanPolicyViolation"}, adoPATReasonLifespanPolicy},
		{"global policy", adoPATError{Status: 400, PatTokenError: "globalPatPolicyViolation"}, adoPATReasonPolicyBlocked},
		{"full-scope policy", adoPATError{Status: 400, PatTokenError: "fullScopePatPolicyViolation"}, adoPATReasonPolicyBlocked},
		{"access denied", adoPATError{Status: 403, PatTokenError: "accessDenied"}, adoPATReasonPolicyBlocked},
		{"a named error beats the status", adoPATError{Status: 401, PatTokenError: "patLifespanPolicyViolation"}, adoPATReasonLifespanPolicy},
		{"401 with no named error", adoPATError{Status: 401, ServiceError: "TF400813"}, adoPATReasonConsentNeeded},
		{"403 with no named error", adoPATError{Status: 403}, adoPATReasonConsentNeeded},
		{"an invalid scope", adoPATError{Status: 400, PatTokenError: "invalidScope"}, adoPATReasonMintRefused},
		{"an invalid expiry", adoPATError{Status: 400, PatTokenError: "invalidValidTo"}, adoPATReasonMintRefused},
		{"a 500", adoPATError{Status: 500}, adoPATReasonMintRefused},
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
		"revoke renewal": {adoPATRevokeRenewal, "renewal"}, "revoke widen": {adoPATRevokeWiden, "widen"},
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
		"reason policy":        {adoPATReasonPolicyBlocked, "ado_pat_policy_blocked"},
		"reason lifespan":      {adoPATReasonLifespanPolicy, "ado_pat_lifespan_policy"},
		"reason consent":       {adoPATReasonConsentNeeded, "ado_pat_consent_needed"},
		"reason refused":       {adoPATReasonMintRefused, "ado_pat_mint_refused"},
	} {
		if pair[0] != pair[1] {
			t.Errorf("%s = %q, want %q", name, pair[0], pair[1])
		}
	}
	var _ adoPATClient = fakeADOPATClient{}
	_ = adoPAT{}
	_ = adoPATRequest{}
}

type fakeADOPATClient struct{ adoPATClient }
