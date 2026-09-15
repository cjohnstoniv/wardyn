// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// newSSOUploadSrvAudited is newSSOUploadSrvWith with an audit sink the test can
// read the refusal rows back from.
func newSSOUploadSrvAudited(t *testing.T, events []types.AuditEvent, site types.SiteConfig, runID uuid.UUID) (*Server, *memAudit, string) {
	t.Helper()
	h := newHarness(t)
	audit := &memAudit{}
	st := ssoLoginRunStore{
		run:     types.AgentRun{ID: runID, Task: harnessLoginTask, Agent: awsSSOAgent},
		events:  events,
		siteCfg: site,
	}
	cfg := baseTestConfig(h, st)
	cfg.Secrets = &memSecrets{m: map[string][]byte{}}
	cfg.BedrockRegion = "us-west-2"
	cfg.Audit = audit
	srv := New(cfg)
	h.srv = srv
	return srv, audit, h.mintRunToken(t, runID)
}

// TestUploadSSOToken_BlobShapeNamesTheEmptyFields (lens-S S-8).
//
// The blob_shape sentence named a FIXED list (access_token/start_url/region/
// expires_at) that valid() outgrew: it also requires account_id and role_name,
// and since 0.7.3 the most common way a capture fails is pickAccountRole coming
// back blank on a portal miss — so the person on the login terminal read four
// field names, none of which was missing.
func TestUploadSSOToken_BlobShapeNamesTheEmptyFields(t *testing.T) {
	runID := uuid.New()
	srv, _, tok := newSSOUploadSrvAudited(t, ssoLoginStartedEvents(runID, ssoLaunchPortal), types.SiteConfig{}, runID)
	// A blob from a portal miss: everything present except the account/role pair.
	body := strings.NewReplacer(
		`"account_id": "123456789012"`, `"account_id": ""`,
		`"role_name": "WardynBedrockRole"`, `"role_name": ""`,
		`"start_url": "https://my-sso.awsapps.com/start"`, `"start_url": "`+ssoLaunchPortal+`"`,
	).Replace(validSSOBody)

	w := do(t, srv, http.MethodPut, "/api/v1/internal/sso-token/"+runID.String(), tok, body)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("code = %d, want 400; body=%s", w.Code, w.Body.String())
	}
	got := w.Body.String()
	for _, want := range []string{"account_id", "role_name"} {
		if !strings.Contains(got, want) {
			t.Errorf("refusal %s does not name the EMPTY field %q", got, want)
		}
	}
	if strings.Contains(got, "access_token") {
		t.Errorf("refusal %s names access_token, which the blob carries", got)
	}
}

// TestUploadSSOToken_RefusalAfterScopeCarriesOwner (lens-S S-9).
//
// harness.credential.refused carried {provider, reason} only, so on a per_user
// estate a refusal stream could not be grouped by person without joining each
// row's run_id back to its harness.login.started. Its success twin
// (harness.credential.captured) has carried owner + credential_source since the
// scope landed — "whose session" is the first question after an incident — and
// every refusal that happens AFTER loginRunScope has the scope in hand.
func TestUploadSSOToken_RefusalAfterScopeCarriesOwner(t *testing.T) {
	const owner = "alice@example.com"
	runID := uuid.New()
	srv, audit, tok := newSSOUploadSrvAudited(t,
		ssoLoginStartedPerUser(runID, ssoLaunchPortal, owner), types.SiteConfig{}, runID)
	body := strings.Replace(validSSOBody,
		`"start_url": "https://my-sso.awsapps.com/start"`, `"start_url": "`+ssoLaunchPortal+`"`, 1)

	// First capture lands; the SECOND is refused already_captured — a post-lock,
	// post-scope refusal, and the replay shape the row has to be groupable for.
	if w := do(t, srv, http.MethodPut, "/api/v1/internal/sso-token/"+runID.String(), tok, body); w.Code != http.StatusNoContent {
		t.Fatalf("first upload: code = %d, want 204; body=%s", w.Code, w.Body.String())
	}
	if w := do(t, srv, http.MethodPut, "/api/v1/internal/sso-token/"+runID.String(), tok, body); w.Code != http.StatusConflict {
		t.Fatalf("replay: code = %d, want 409; body=%s", w.Code, w.Body.String())
	}

	rows := audit.find("harness.credential.refused")
	if len(rows) != 1 {
		t.Fatalf("harness.credential.refused rows = %d, want 1", len(rows))
	}
	var data map[string]any
	if err := json.Unmarshal(rows[0].Data, &data); err != nil {
		t.Fatalf("decode refusal data: %v", err)
	}
	if data["owner"] != owner {
		t.Errorf("refusal data = %v, want owner %q", data, owner)
	}
	if data["credential_source"] != string(types.CredentialSourcePerUser) {
		t.Errorf("refusal data = %v, want credential_source %q", data, types.CredentialSourcePerUser)
	}
	if data["reason"] != refuseReasonAlreadyCaptured {
		t.Errorf("refusal data = %v, want reason %q", data, refuseReasonAlreadyCaptured)
	}
}
