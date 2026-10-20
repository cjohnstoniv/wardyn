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
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
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
	if got.ForTrust(placement.ClassAPIKey, types.RunnerTrustPersonal).Mode != placement.ModeViaOrg || got.ForTrust(placement.ClassGitHubToken, types.RunnerTrustPersonal).Mode != placement.ModeViaOrg ||
		got.ForTrust(placement.ClassEnvSecret, types.RunnerTrustPersonal).Mode != placement.ModeRefuse {
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
		d.After.ForTrust(placement.ClassAPIKey, types.RunnerTrustPersonal).Mode != placement.ModeViaOrg || d.ProposedBy != govSubAlice {
		t.Errorf("audit data = %s (%v), want before {} and after the approved policy", rows[0].Data, err)
	}
	var doc struct {
		Policy placement.DeliveryPolicy `json:"policy"`
	}
	if w := e.call(e.carol, http.MethodGet, credentialDeliveryPath, ""); w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &doc) != nil ||
		doc.Policy.ForTrust(placement.ClassGitHubToken, types.RunnerTrustPersonal).Mode != placement.ModeViaOrg {
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
	if got := stored(); got.ForTrust(placement.ClassAPIKey, types.RunnerTrustPersonal).Mode != placement.ModeRefuse || got.ForTrust(placement.ClassGitHubToken, types.RunnerTrustPersonal).Mode != placement.ModeRefuse {
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

// The ETag a direct save answers with is the one the next GET reports, so a console that sends it back as If-Match is
// never refused for a document nobody else touched (U3).
func TestCredentialDeliveryWrite_DirectSaveETagIsTheNextGETs(t *testing.T) {
	e := newGovEnv(t)
	const body = `{"classes":{"api_key":{"mode":"via_org"},"env_secret":{"mode":"runner_resident","require_posture":{"mdm_managed":true,"os_min":{"linux":"1.2.3"}}}}}`
	put := e.admin(http.MethodPut, credentialDeliveryPath, body)
	if put.Code != http.StatusOK {
		t.Fatalf("direct PUT = %d %s, want 200", put.Code, put.Body)
	}
	saved := put.Header().Get("ETag")
	got := e.call(e.alice, http.MethodGet, credentialDeliveryPath, "")
	if got.Code != http.StatusOK || saved == "" || got.Header().Get("ETag") != saved {
		t.Fatalf("PUT ETag %q, next GET %d ETag %q, want the same non-empty ETag", saved, got.Code, got.Header().Get("ETag"))
	}
}

// GET lists the modes each class can be delivered under today beside the policy, never inside it, so a
// client that sends GET's policy back is accepted by the strict PUT.
func TestCredentialDeliveryGET_BuildableModesBesidePolicy(t *testing.T) {
	e := newGovEnv(t)
	const body = `{"classes":{"ado_minted_pat":{"mode":"runner_resident"},"api_key":{"mode":"via_org"}}}`
	if put := e.admin(http.MethodPut, credentialDeliveryPath, body); put.Code != http.StatusOK {
		t.Fatalf("direct PUT = %d %s, want 200", put.Code, put.Body)
	}
	got := e.call(e.alice, http.MethodGet, credentialDeliveryPath, "")
	if got.Code != http.StatusOK {
		t.Fatalf("GET = %d %s", got.Code, got.Body)
	}
	var resp struct {
		Policy         json.RawMessage             `json:"policy"`
		BuildableModes map[string][]placement.Mode `json:"buildable_modes"`
	}
	if err := json.Unmarshal(got.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.BuildableModes) != len(placement.DeliveryClasses()) {
		t.Errorf("buildable_modes names %d classes, want every class (%d): %s", len(resp.BuildableModes), len(placement.DeliveryClasses()), got.Body)
	}
	for _, c := range placement.DeliveryClasses() {
		if m, ok := resp.BuildableModes[c]; !ok || !slices.Equal(m, placement.BuildableModes(c)) {
			t.Errorf("buildable_modes[%s] = %v, want %v", c, m, placement.BuildableModes(c))
		}
	}
	if strings.Contains(string(resp.Policy), "buildable_modes") {
		t.Errorf("buildable_modes is inside the policy: %s", resp.Policy)
	}
	if put := e.admin(http.MethodPut, credentialDeliveryPath, string(resp.Policy)); put.Code != http.StatusOK {
		t.Errorf("PUT of GET's policy = %d %s, want 200", put.Code, put.Body)
	}
}

// A direct save checked against a document that another write then moves is refused 412, not applied
// over it: the If-Match check and the write run under the target lock an approval takes.
func TestCredentialDeliveryWrite_DirectSaveStaleAgainstAConcurrentWrite(t *testing.T) {
	e := newGovEnv(t)
	etag := e.admin(http.MethodGet, credentialDeliveryPath, "").Header().Get("ETag")
	if etag == "" {
		t.Fatal("GET answered no ETag")
	}
	ctx := context.Background()
	tx, err := e.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // a no-op after Commit
	if err := store.LockGovernanceTarget(ctx, tx, govKindCredentialDelivery); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	var got *httptest.ResponseRecorder
	wg.Add(1)
	go func() {
		defer wg.Done()
		got = doWithHeaders(t, e.srv, http.MethodPut, credentialDeliveryPath, adminToken,
			`{"classes":{"api_key":{"mode":"via_org"}}}`, map[string]string{"If-Match": etag})
	}()
	time.Sleep(300 * time.Millisecond)
	if _, err := store.PutCredentialDeliveryPolicyQ(ctx, tx, mustJSON(placement.DeliveryPolicy{
		Classes: map[string]placement.ClassPolicy{"env_secret": {Mode: "via_org"}}}), "other", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	if got.Code != http.StatusPreconditionFailed || !strings.Contains(got.Body.String(), reasonCredentialDeliveryStale) {
		t.Fatalf("direct PUT checked before a concurrent write = %d %s, want 412 %s", got.Code, got.Body, reasonCredentialDeliveryStale)
	}
}
