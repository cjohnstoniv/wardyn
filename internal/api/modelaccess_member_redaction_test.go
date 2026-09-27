// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// anyRFC3339 finds ANY RFC3339 instant anywhere in a serialised body. The leak
// this pins was not a timestamp FIELD — redaction had already dropped
// SetupHarness.ExpiresAt, CapturedAt and Renewable — it was the same instant
// republished one field over, inside a composed English sentence
// (modelAccessExpiringAction). So the assertion is on the BYTES, not on a field
// list a future field can be added beside.
var anyRFC3339 = regexp.MustCompile(`\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}`)

// sharedExpiringBlob is the OPERATOR's captured session on a `shared`
// deployment, renewable but with its OIDC client registration lapsing inside
// modelAccessExpiringWindow — the one grading that composes a deadline into the
// action line.
func sharedExpiringBlob(now time.Time) awsSSOBlob {
	return awsSSOBlob{
		AccessToken: "operator-access-token", RefreshToken: "operator-refresh-token",
		StartURL: "https://acme.awsapps.com/start", Region: "us-east-1",
		AccountID: "123456789012", RoleName: "WardynBedrockRole",
		ExpiresAt:             now.Add(time.Hour),
		RegistrationExpiresAt: now.Add(6 * time.Hour),
		CapturedAt:            now.Add(-time.Hour),
		SourceRunID:           "11111111-1111-1111-1111-111111111111",
	}
}

// sharedRoster is a `shared` claude-code bedrock_sso row: ONE model credential
// for everybody, which is the operator's.
func sharedRoster() types.SiteConfig {
	return agentRoster(types.AgentProvider{ID: "claude-code", Mechanism: types.AgentMechanismBedrockSSO})
}

// memberStatusFor is the member's whole /setup/status body, serialised: the
// redaction applied to a status carrying the operator's own graded credential
// plus the harness row that credential produces.
func memberStatusFor(t *testing.T, ma SetupModelAccess, blob awsSSOBlob, now time.Time) (SetupStatus, string) {
	t.Helper()
	full := SetupStatus{
		ModelAccess: ma,
		Harness: []SetupHarness{{
			Provider: awsSSOProvider, Captured: true,
			CapturedAt:  blob.CapturedAt.Format(time.RFC3339),
			ExpiresAt:   blob.ExpiresAt.Format(time.RFC3339),
			Expired:     blob.expired(now),
			Renewable:   blob.renewable(now),
			SourceRunID: blob.SourceRunID,
		}},
		Secrets: SetupSecrets{Present: []string{"bedrock-api-key"}},
		Checks:  []SetupCheck{{ID: "harness_credential_aws", Detail: "operator detail"}},
	}
	out := redactSetupStatusForUser(full, false, false)
	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	return out, string(raw)
}

// TestMemberModelAccess_SharedNeverCarriesTheOperatorsDeadline is finding 1.
//
// Under a `shared` row awsSSOScopeFor resolves the zero scope for EVERY caller,
// so setupHarnessCreds grades the OPERATOR's blob and handed the member
// state:"expiring" with action:"Sign in again before <RFC3339>" — the
// operator's credential lifecycle, republished into a body redaction had just
// stripped it from, telling the member to do a thing POST /setup/harness-login
// then refuses them (harnessLoginNotPerUserRefusal).
//
// A member under `shared` owns nothing here, so there are only two answers: the
// admin's credential works, or it does not.
func TestMemberModelAccess_SharedNeverCarriesTheOperatorsDeadline(t *testing.T) {
	now := awsSSOTestFixedNow
	blob := sharedExpiringBlob(now)
	ma := setupModelAccess(sharedRoster(), blob, true, false, awsSSOScope{}, true, now)

	// Precondition: this IS the grading that leaked — the operator's own row
	// still says expiring, and still names the instant.
	if ma.State != modelAccessExpiring {
		t.Fatalf("operator grading = %q, want %q (fixture no longer exercises the leak)", ma.State, modelAccessExpiring)
	}
	deadline := blob.RegistrationExpiresAt.UTC().Format(time.RFC3339)
	if ma.Deadline != deadline || !strings.Contains(ma.Action, deadline) {
		t.Fatalf("the OPERATOR's own row must still carry the deadline: %+v, want %q", ma, deadline)
	}

	out, raw := memberStatusFor(t, ma, blob, now)
	if out.ModelAccess.State != modelAccessLive {
		t.Errorf("member state = %q, want %q — the admin's credential still works, and its lapse is not the member's to act on",
			out.ModelAccess.State, modelAccessLive)
	}
	if out.ModelAccess.Action != "" {
		t.Errorf("member action = %q, want none — there is nothing a member can do about a shared credential that still works", out.ModelAccess.Action)
	}
	if out.ModelAccess.Deadline != "" {
		t.Errorf("member Deadline = %q, want none", out.ModelAccess.Deadline)
	}
	if out.ModelAccess.Mechanism != string(types.AgentMechanismBedrockSSO) {
		t.Errorf("member mechanism = %q, want the lane to survive", out.ModelAccess.Mechanism)
	}
	// THE PIN: no instant, anywhere in the member's body.
	if got := anyRFC3339.FindString(raw); got != "" {
		t.Errorf("the member's body carries the timestamp %q: %s", got, raw)
	}
	for _, leak := range []string{"bedrock-api-key", "awsapps.com", "amazonaws.com", "operator-access-token", "operator detail", "11111111-1111"} {
		if strings.Contains(raw, leak) {
			t.Errorf("the member's body leaks %q: %s", leak, raw)
		}
	}
}

// TestMemberModelAccess_SharedAndDeadNamesTheAdmin: the other member-visible
// arm. A dead shared credential says so, names the admin, and still names no
// instant and offers no button the server would refuse.
func TestMemberModelAccess_SharedAndDeadNamesTheAdmin(t *testing.T) {
	now := awsSSOTestFixedNow
	blob := sharedExpiringBlob(now)
	blob.RefreshToken = ""                   // nothing can renew it
	blob.ExpiresAt = now.Add(-time.Minute)   // and it is already gone
	blob.RegistrationExpiresAt = time.Time{} // a legacy sso_start_url profile carries none

	ma := setupModelAccess(sharedRoster(), blob, true, false, awsSSOScope{}, true, now)
	if ma.State != modelAccessSharedExpired {
		t.Fatalf("grading = %q, want %q", ma.State, modelAccessSharedExpired)
	}
	out, raw := memberStatusFor(t, ma, blob, now)
	if out.ModelAccess.State != modelAccessSharedExpired {
		t.Errorf("member state = %q, want %q", out.ModelAccess.State, modelAccessSharedExpired)
	}
	if out.ModelAccess.Action != modelAccessSharedExpiredAction {
		t.Errorf("member action = %q, want the admin sentence %q", out.ModelAccess.Action, modelAccessSharedExpiredAction)
	}
	if got := anyRFC3339.FindString(raw); got != "" {
		t.Errorf("the shared_expired body carries the timestamp %q: %s", got, raw)
	}

	// Every shared-lane state a member can be shown collapses to one of exactly
	// two answers — never a sign-in action, never a deadline.
	for _, state := range []string{modelAccessExpiredSignin, modelAccessNotConfigured, modelAccessSharedExpired} {
		got := userModelAccess(SetupModelAccess{State: state, Action: modelAccessAction(state, "2026-01-01T00:00:00Z"), Deadline: "2026-01-01T00:00:00Z"})
		if got.State != modelAccessSharedExpired || got.Action != modelAccessSharedExpiredAction || got.Deadline != "" {
			t.Errorf("shared %q collapses to %+v, want shared_expired with the admin sentence and no deadline", state, got)
		}
		if got.Action == modelAccessSignInAction {
			t.Errorf("shared %q still offers a sign-in the server refuses", state)
		}
	}
}

// TestMemberModelAccess_PerUserKeepsItsOwnersDeadline is the other side of the
// rule: `expiring`, `expired_signin` and `not_configured` — with a timestamp or
// a sign-in action — are reserved for a principal who OWNS the credential. A
// member under `per_user` owns theirs, so nothing is collapsed for them.
func TestMemberModelAccess_PerUserKeepsItsOwnersDeadline(t *testing.T) {
	now := awsSSOTestFixedNow
	row := types.AgentProvider{
		ID: "claude-code", Mechanism: types.AgentMechanismBedrockSSO,
		CredentialSource: types.CredentialSourcePerUser, SSOStartURL: "https://acme.awsapps.com/start",
	}
	blob := sharedExpiringBlob(now)
	scope := awsSSOScope{perUser: true, owner: "member@corp.example"}
	ma := setupModelAccess(agentRoster(row), blob, true, false, scope, true, now)
	if !ma.PerUser {
		t.Fatal("a per_user grading must say so — userModelAccess reads it")
	}
	out := redactSetupStatusForUser(SetupStatus{ModelAccess: ma}, false, false)
	if out.ModelAccess != ma {
		t.Fatalf("a per_user member's own answer = %+v, want it kept verbatim (%+v)", out.ModelAccess, ma)
	}
	if !strings.Contains(out.ModelAccess.Action, blob.RegistrationExpiresAt.UTC().Format(time.RFC3339)) {
		t.Errorf("a member who owns the credential must still be told WHEN: %q", out.ModelAccess.Action)
	}
	// And with nothing captured they are still offered the sign-in they can
	// actually complete.
	none := setupModelAccess(agentRoster(row), awsSSOBlob{}, false, false, scope, true, now)
	if got := userModelAccess(none); got.State != modelAccessNotConfigured || got.Action != modelAccessSignInAction {
		t.Errorf("per_user first run = %+v, want not_configured + %q", got, modelAccessSignInAction)
	}
}

// TestModelAccessDeadline_OnTheWireForItsOWNER_NeverForASharedMember is the
// 0.7.6 half of the same rule.
//
// Deadline went ON the wire (json:"deadline,omitempty") because `expiring` is
// now rendered on EVERY screen for 24 hours, and the sentence the server
// composes carries an RFC3339 UTC stamp — which a member in another timezone
// misreads. The console re-composes that one line through the frozen template
// with the reader's own clock, and it needs the instant to do it.
//
// That is SAFE for exactly one reason, and this test is that reason written
// down: userModelAccess builds a FRESH struct for a member under a `shared`
// row, so the operator's deadline cannot ride along in a new field the way it
// once rode along inside a composed English sentence.
func TestModelAccessDeadline_OnTheWireForItsOWNER_NeverForASharedMember(t *testing.T) {
	now := awsSSOTestFixedNow
	blob := sharedExpiringBlob(now)
	deadline := blob.RegistrationExpiresAt.UTC().Format(time.RFC3339)

	// (a) THE OWNER — a member under a per_user row grading their own session.
	row := types.AgentProvider{
		ID: "claude-code", Mechanism: types.AgentMechanismBedrockSSO,
		CredentialSource: types.CredentialSourcePerUser, SSOStartURL: "https://acme.awsapps.com/start",
	}
	own := setupModelAccess(agentRoster(row), blob, true, false,
		awsSSOScope{perUser: true, owner: "member@corp.example"}, true, now)
	if own.State != modelAccessExpiring || own.Deadline != deadline {
		t.Fatalf("owner grading = %+v, want %q with deadline %q", own, modelAccessExpiring, deadline)
	}
	raw, err := json.Marshal(redactSetupStatusForUser(SetupStatus{ModelAccess: own}, false, false))
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		ModelAccess map[string]any `json:"model_access"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	if got, ok := wire.ModelAccess["deadline"]; !ok || got != deadline {
		t.Errorf("the credential's OWNER must be told WHEN on the wire: model_access = %v, want deadline %q",
			wire.ModelAccess, deadline)
	}

	// (b) A MEMBER UNDER A SHARED ROW — the credential is the operator's, and
	// nothing about its lifecycle is theirs to read. Not a field, not a
	// timestamp anywhere in the bytes.
	shared := setupModelAccess(sharedRoster(), blob, true, false, awsSSOScope{}, true, now)
	if shared.Deadline == "" {
		t.Fatal("the operator's own grading must still carry a deadline (fixture no longer exercises the leak)")
	}
	_, sharedRaw := memberStatusFor(t, shared, blob, now)
	if strings.Contains(sharedRaw, "deadline") {
		t.Errorf("a member under a shared row was sent a deadline field: %s", sharedRaw)
	}
	if got := anyRFC3339.FindString(sharedRaw); got != "" {
		t.Errorf("a member under a shared row was sent the instant %q: %s", got, sharedRaw)
	}

	// (c) …and the dead shared state the member CAN be shown carries none
	// either, in either direction: no field, no sentence.
	dead := userModelAccess(SetupModelAccess{
		State: modelAccessSharedExpired, Deadline: deadline,
		Action: modelAccessAction(modelAccessSharedExpired, deadline),
	})
	if dead.Deadline != "" {
		t.Errorf("shared_expired kept a deadline: %+v", dead)
	}
}
