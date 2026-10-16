// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// PUT /governance/credential-delivery against a real Postgres: a security admin's replacement is held
// for a second human, applied on approval with credential.delivery.write before/after, refused 400 when
// placement.DeliveryPolicy.Validate refuses it, and refused 403 to a member.
//
// Guarded by WARDYN_TEST_PG (throwawayPGPool): skipped cleanly when unset, must PASS when set.

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/placement"
)

const credentialDeliveryPath = "/api/v1/governance/credential-delivery"

func TestCredentialDeliveryWrite_HeldValidatedAudited(t *testing.T) {
	e := newGovEnv(t)
	stored := func() placement.DeliveryPolicy {
		doc, err := e.pg.GetCredentialDeliveryPolicy(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		p, err := deliveryPolicyOf(doc.Policy)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}

	if w := e.call(e.user, http.MethodPut, credentialDeliveryPath, `{"classes":{"api_key":{"mode":"via_org"}}}`); w.Code != http.StatusForbidden {
		t.Fatalf("member PUT = %d %s, want 403", w.Code, w.Body)
	}
	if w := e.call(e.user, http.MethodGet, credentialDeliveryPath, ""); w.Code != http.StatusForbidden {
		t.Fatalf("member GET = %d, want 403", w.Code)
	}
	for _, bad := range []string{
		`{"classes":{"github_token":{"mode":"runner_resident"}}}`, // outside AllowedModes
		`{"classes":{"no_such_class":{"mode":"via_org"}}}`,        // unknown class
		`{"classes":{"cloud_sts":{"mode":"refuse"}}}`,             // fixed class
		`{"classes":{"api_key":{"mode":"via_org","require_posture":{"mdm_managed":true}}}}`,
	} {
		if w := e.call(e.alice, http.MethodPut, credentialDeliveryPath, bad); w.Code != http.StatusBadRequest {
			t.Errorf("PUT %s = %d %s, want 400", bad, w.Code, w.Body)
		}
	}
	if n := e.pendingCount(); n != 0 {
		t.Fatalf("%d changes held for refused bodies", n)
	}

	const body = `{"classes":{"api_key":{"mode":"via_org"},"github_token":{"mode":"via_org"}}}`
	held := e.pending(e.call(e.alice, http.MethodPut, credentialDeliveryPath, body))
	if held.TargetKind != govKindCredentialDelivery || held.Op != "replace" || held.TargetKey != govCredentialDeliveryKey {
		t.Errorf("held = %s %s %s", held.TargetKind, held.Op, held.TargetKey)
	}
	if got := stored(); len(got.Classes) != 0 {
		t.Fatalf("a held replacement changed the stored policy: %+v", got)
	}
	if w := e.call(e.alice, http.MethodPost, approvePath(held.ID), ""); w.Code != http.StatusForbidden {
		t.Errorf("self-approval = %d, want 403", w.Code)
	}
	e.approve(held.ID)
	got := stored()
	if got.For(placement.ClassAPIKey).Mode != placement.ModeViaOrg || got.For(placement.ClassGitHubToken).Mode != placement.ModeViaOrg ||
		got.For(placement.ClassEnvSecret).Mode != placement.ModeRefuse {
		t.Fatalf("stored policy after approval = %+v", got)
	}
	rows := e.audits(placement.ActionCredentialDeliveryWrite)
	if len(rows) != 1 || rows[0].Actor != govSubBob {
		t.Fatalf("audit rows = %+v, want one by the approver", rows)
	}
	var d struct {
		Before     placement.DeliveryPolicy `json:"before"`
		After      placement.DeliveryPolicy `json:"after"`
		ProposedBy string                   `json:"proposed_by"`
	}
	if err := json.Unmarshal(rows[0].Data, &d); err != nil || len(d.Before.Classes) != 0 ||
		d.After.For(placement.ClassAPIKey).Mode != placement.ModeViaOrg || d.ProposedBy != govSubAlice {
		t.Errorf("audit data = %s (%v), want before {} and after the approved policy", rows[0].Data, err)
	}
	var doc struct {
		Policy placement.DeliveryPolicy `json:"policy"`
	}
	if w := e.call(e.carol, http.MethodGet, credentialDeliveryPath, ""); w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &doc) != nil ||
		doc.Policy.For(placement.ClassGitHubToken).Mode != placement.ModeViaOrg {
		t.Errorf("GET = %d %s, want the stored policy", w.Code, w.Body)
	}

	// A change proposed against one policy goes stale when the policy moves first.
	stale := e.pending(e.call(e.alice, http.MethodPut, credentialDeliveryPath, `{"classes":{}}`))
	if _, err := e.pg.PutCredentialDeliveryPolicy(context.Background(), json.RawMessage(`{"classes":{"api_key":{"mode":"refuse"}}}`), "seed", time.Now()); err != nil {
		t.Fatal(err)
	}
	if w := e.call(e.bob, http.MethodPost, approvePath(stale.ID), ""); w.Code == http.StatusOK {
		t.Fatalf("approving a stale change = %d, want a refusal", w.Code)
	}
	if got := stored(); got.For(placement.ClassAPIKey).Mode != placement.ModeRefuse || got.For(placement.ClassGitHubToken).Mode != placement.ModeRefuse {
		t.Errorf("stored after a stale approval = %+v", got)
	}
}

// TestCredentialDeliveryWrite_IfMatch: GET carries the policy's ETag, a stale If-Match is a 412 before
// anything is held, and the current one is accepted (and held, with the second person on).
func TestCredentialDeliveryWrite_IfMatch(t *testing.T) {
	e := newGovEnv(t)
	const body = `{"classes":{"api_key":{"mode":"via_org"}}}`
	got := e.call(e.alice, http.MethodGet, credentialDeliveryPath, "")
	etag := got.Header().Get("ETag")
	if got.Code != http.StatusOK || etag == "" {
		t.Fatalf("GET = %d, ETag %q, want 200 with an ETag", got.Code, etag)
	}
	stale := doSSOIfMatch(t, e.srv, http.MethodPut, credentialDeliveryPath, e.alice, `"0000"`, body)
	if stale.Code != http.StatusPreconditionFailed || !strings.Contains(stale.Body.String(), reasonCredentialDeliveryStale) {
		t.Fatalf("stale If-Match = %d %s, want 412 %s", stale.Code, stale.Body, reasonCredentialDeliveryStale)
	}
	if n := e.pendingCount(); n != 0 {
		t.Fatalf("%d changes held for a stale write", n)
	}
	if w := doSSOIfMatch(t, e.srv, http.MethodPut, credentialDeliveryPath, e.alice, etag, body); w.Code != http.StatusAccepted {
		t.Fatalf("current If-Match = %d %s, want 202 (held)", w.Code, w.Body)
	}
}
