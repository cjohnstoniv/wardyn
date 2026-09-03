// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Per-principal secrets (0.7, migration 0050): a member manages their own
// row through the generic PUT/DELETE/GET /secrets surface, self-service,
// while an operator's own use of it stays byte-identical to pre-0050. These
// tests drive the REAL router with REAL SSO sessions (rbac_test.go's
// ssoSession/doSSO), the same shape TestAuthzMatrix/TestRequireOperator_*
// use, because the property under test — which NAMESPACE a request lands in
// — is a property of the whole authenticated request, not of a handler
// called directly.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// secretsRBACServer builds a Server with OIDC configured (so ssoSession/doSSO
// drive the real SSO branch of humanOrAdminAuth), the given secret store, and
// an optional ceiling of EligibleGrants (for the member-visible-names
// pairing test) — the /secrets specialization of rbac_test.go's rbacServer.
// cfg.Store is left nil like newSecretsHarness: none of the handlers under
// test here touch it except through capSeamAllowed's own nil-Store fast path
// (capSecret reads as unenforced, matching a 0.5-compat deployment).
func secretsRBACServer(t *testing.T, secrets *memSecrets, ceilingGrants ...types.GrantSpec) (*harness, *Server) {
	t.Helper()
	h := newHarness(t)
	h.srv.cfg.OIDC = &oidc.Authenticator{}
	h.srv.cfg.Secrets = secrets
	h.srv.cfg.DefaultPolicy.EligibleGrants = ceilingGrants
	h.srv.router = h.srv.routes() // re-mount with the secret surfaces enabled
	return h, h.srv
}

// auditDataField decodes one string field out of an audit event's Data.
func auditDataField(t *testing.T, ev types.AuditEvent, key string) (string, bool) {
	t.Helper()
	if len(ev.Data) == 0 {
		return "", false
	}
	var data map[string]any
	if err := json.Unmarshal(ev.Data, &data); err != nil {
		t.Fatalf("unmarshal audit Data for %q: %v", ev.Action, err)
	}
	v, ok := data[key]
	if !ok {
		return "", false
	}
	s, _ := v.(string)
	return s, true
}

// TestSecretOwnerFromRequest pins secretOwnerFromRequest's rule directly:
// "" for an operator (admin token, local mode regardless of the DEV-ONLY
// X-Wardyn-Principal override, or an admin-role SSO session), the caller's
// own sub for a member — and a wdn_ token and an SSO session for the SAME
// member resolve to the IDENTICAL sub, because both auth branches of
// humanOrAdminAuth converge on the same withHumanIdentity call (http.go).
func TestSecretOwnerFromRequest(t *testing.T) {
	s := &Server{}
	mkReq := func(ctx context.Context, xWardynPrincipal string) *http.Request {
		r := httptest.NewRequest(http.MethodPut, "/api/v1/secrets/x", nil)
		if xWardynPrincipal != "" {
			r.Header.Set("X-Wardyn-Principal", xWardynPrincipal)
		}
		return r.WithContext(ctx)
	}
	tests := []struct {
		name string
		r    *http.Request
		want string
	}{
		{"admin token: no session at all", mkReq(context.Background(), ""), ""},
		{
			"local mode: operator regardless of X-Wardyn-Principal",
			mkReq(withLocalPrincipal(context.Background(), "local:alice"), "someone-else"),
			"",
		},
		{
			"sso session, admin role: operator",
			mkReq(operatorCtx("sub-admin-1", "a1@corp.example", oidc.RoleAdmin), ""),
			"",
		},
		{
			"sso session, member role: the caller's own sub",
			mkReq(operatorCtx("sub-member-1", "m1@corp.example", oidc.RoleMember), ""),
			"sub-member-1",
		},
		{
			"wdn_ token, same sub+role as the SSO session above: the identical sub",
			mkReq(withHumanIdentity(context.Background(), "sub-member-1", "m1@corp.example", oidc.RoleMember, nil, false), ""),
			"sub-member-1",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := s.secretOwnerFromRequest(tc.r); got != tc.want {
				t.Fatalf("secretOwnerFromRequest = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestPutSecret_MemberStampsOwner_AuditSecretOwner: a member's PUT lands in
// their OWN namespace (never the operator's), and the write is audited with
// secret_owner naming them.
func TestPutSecret_MemberStampsOwner_AuditSecretOwner(t *testing.T) {
	sec := &memSecrets{m: map[string][]byte{}}
	h, srv := secretsRBACServer(t, sec)
	alice := ssoSession(t, "alice", "alice@corp.example", oidc.RoleMember)

	w := doSSO(t, srv, http.MethodPut, "/api/v1/secrets/anthropic-api-key", alice, `{"value":"sk-ant-member-owned-value"}`)
	if w.Code != http.StatusNoContent {
		t.Fatalf("PUT = %d, want 204: %s", w.Code, w.Body.String())
	}

	got, err := sec.For("alice").Get(context.Background(), "anthropic-api-key")
	if err != nil || string(got) != "sk-ant-member-owned-value" {
		t.Fatalf("alice's own row = (%q, %v), want the stored value", got, err)
	}
	// The operator's namespace stays untouched by a member's write.
	if _, err := sec.Get(context.Background(), "anthropic-api-key"); !errors.Is(err, secretstore.ErrNotFound) {
		t.Fatalf("operator namespace has a row after a MEMBER's PUT (err=%v); it must not", err)
	}

	ev := lastAuditEvent(t, h.audit.events, "secret.write")
	if owner, ok := auditDataField(t, ev, "secret_owner"); !ok || owner != "alice" {
		t.Errorf("audit secret_owner = (%q, present=%v), want alice", owner, ok)
	}
}

// TestDeleteSecret_MemberOtherOwner_204ByteIdenticalToMissing: a member's
// DELETE of a name owned by a DIFFERENT member is structurally unreachable
// (Store.For(owner) never resolves it) and answers the exact same 204 a
// never-set name gets — no existence oracle, and the foreign row survives.
func TestDeleteSecret_MemberOtherOwner_204ByteIdenticalToMissing(t *testing.T) {
	sec := &memSecrets{m: map[string][]byte{}}
	_, srv := secretsRBACServer(t, sec)
	alice := ssoSession(t, "alice", "alice@corp.example", oidc.RoleMember)
	bob := ssoSession(t, "bob", "bob@corp.example", oidc.RoleMember)

	if w := doSSO(t, srv, http.MethodPut, "/api/v1/secrets/shared-name", bob, `{"value":"bob-owns-this-value"}`); w.Code != http.StatusNoContent {
		t.Fatalf("seed bob's row: %d %s", w.Code, w.Body.String())
	}

	wMissing := doSSO(t, srv, http.MethodDelete, "/api/v1/secrets/never-set-at-all", alice, "")
	wForeign := doSSO(t, srv, http.MethodDelete, "/api/v1/secrets/shared-name", alice, "")

	if wMissing.Code != http.StatusNoContent {
		t.Fatalf("delete of a never-set name = %d, want 204: %s", wMissing.Code, wMissing.Body.String())
	}
	if wForeign.Code != wMissing.Code || wForeign.Body.String() != wMissing.Body.String() {
		t.Fatalf("delete of bob's name by alice = (%d,%q), want byte-identical to the never-set case (%d,%q)",
			wForeign.Code, wForeign.Body.String(), wMissing.Code, wMissing.Body.String())
	}

	if got, err := sec.For("bob").Get(context.Background(), "shared-name"); err != nil || string(got) != "bob-owns-this-value" {
		t.Fatalf("bob's row after alice's delete = (%q, %v), want it untouched", got, err)
	}
}

// TestListSecrets_MemberOmitsOthersAndUnpairedOperatorNames: a member's
// `names` is the operator-owned names an eligible ceiling grant actually
// pairs with a host — never another member's own name, and never an
// operator name the ceiling never offers.
func TestListSecrets_MemberOmitsOthersAndUnpairedOperatorNames(t *testing.T) {
	sec := &memSecrets{m: map[string][]byte{
		"anthropic-api-key":  []byte("op-value"),
		"unpaired-op-secret": []byte("op-value-2"),
	}}
	ceiling := apiKeyGrantSpec("api.anthropic.com", "anthropic-api-key")
	_, srv := secretsRBACServer(t, sec, ceiling)
	if err := sec.For("bob").Put(context.Background(), "bobs-personal-key", []byte("bob-value")); err != nil {
		t.Fatalf("seed bob's row: %v", err)
	}

	alice := ssoSession(t, "alice", "alice@corp.example", oidc.RoleMember)
	w := doSSO(t, srv, http.MethodGet, "/api/v1/secrets", alice, "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET /secrets = %d: %s", w.Code, w.Body.String())
	}
	var body struct {
		Names []string `json:"names"`
		Mine  []string `json:"mine"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !slices.Equal(body.Names, []string{"anthropic-api-key"}) {
		t.Errorf("names = %v, want [anthropic-api-key] only — the unpaired operator secret and bob's own name must both be omitted", body.Names)
	}
	if len(body.Mine) != 0 {
		t.Errorf("mine = %v, want empty — alice owns nothing", body.Mine)
	}
}

// TestListSecrets_AdminOwnerParam_Member403: ?owner= is admin-only on both
// the routes that accept it, with a constant 403 for a non-operator.
func TestListSecrets_AdminOwnerParam_Member403(t *testing.T) {
	_, srv := secretsRBACServer(t, &memSecrets{m: map[string][]byte{}})
	alice := ssoSession(t, "alice", "alice@corp.example", oidc.RoleMember)

	if w := doSSO(t, srv, http.MethodGet, "/api/v1/secrets?owner=bob", alice, ""); w.Code != http.StatusForbidden {
		t.Fatalf("GET /secrets?owner=bob as a member = %d, want 403: %s", w.Code, w.Body.String())
	}
	if w := doSSO(t, srv, http.MethodDelete, "/api/v1/secrets/anything?owner=bob", alice, ""); w.Code != http.StatusForbidden {
		t.Fatalf("DELETE /secrets/anything?owner=bob as a member = %d, want 403: %s", w.Code, w.Body.String())
	}
}

// TestPutSecret_MemberBedrockNames_403: all FOUR Bedrock/SigV4 credential
// names are refused for a non-operator PUT, even though sinkReservedSecret
// itself deliberately excludes bedrock-api-key (that exclusion is for the
// operator's own legitimate write). The negative control proves the refusal
// is member-specific, not a blanket name ban: an operator may still PUT it.
func TestPutSecret_MemberBedrockNames_403(t *testing.T) {
	names := []string{
		bedrockAccessKeyIDSecret,
		bedrockSecretAccessKeySecret,
		bedrockSessionTokenSecret,
		bedrockAPIKeySecret,
	}
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			_, srv := secretsRBACServer(t, &memSecrets{m: map[string][]byte{}})
			alice := ssoSession(t, "alice", "alice@corp.example", oidc.RoleMember)
			w := doSSO(t, srv, http.MethodPut, "/api/v1/secrets/"+name, alice, `{"value":"some-long-enough-value-000000"}`)
			if w.Code != http.StatusForbidden {
				t.Fatalf("member PUT %s = %d, want 403: %s", name, w.Code, w.Body.String())
			}
		})
	}
	t.Run("negative control: an operator may still PUT it", func(t *testing.T) {
		sec := &memSecrets{m: map[string][]byte{}}
		_, srv := secretsRBACServer(t, sec)
		admin := ssoSession(t, "admin-1", "admin@corp.example", oidc.RoleAdmin)
		w := doSSO(t, srv, http.MethodPut, "/api/v1/secrets/"+bedrockAPIKeySecret, admin, `{"value":"some-long-enough-value-000000"}`)
		if w.Code != http.StatusNoContent {
			t.Fatalf("operator PUT %s = %d, want 204: %s", bedrockAPIKeySecret, w.Code, w.Body.String())
		}
	})
}

// TestSecretsAPI_OperatorByteIdenticalPre0050 is the negative control for the
// whole 0050 rollout: an operator's PUT/GET-via-list/DELETE and the
// secret.write/secret.delete audit shape are UNCHANGED — the value lands in
// the "" namespace, `mine` equals `names`, and neither audit event carries
// secret_owner (its absence, not an empty string, is what "unchanged" means).
func TestSecretsAPI_OperatorByteIdenticalPre0050(t *testing.T) {
	sec := &memSecrets{m: map[string][]byte{}}
	h, srv := secretsRBACServer(t, sec)
	admin := ssoSession(t, "admin-1", "admin@corp.example", oidc.RoleAdmin)

	if w := doSSO(t, srv, http.MethodPut, "/api/v1/secrets/npm-token", admin, `{"value":"npm-token-value-0000000000"}`); w.Code != http.StatusNoContent {
		t.Fatalf("admin PUT = %d: %s", w.Code, w.Body.String())
	}
	if got, err := sec.Get(context.Background(), "npm-token"); err != nil || string(got) != "npm-token-value-0000000000" {
		t.Fatalf("plain (operator-namespace) Get = (%q, %v)", got, err)
	}
	if _, present := auditDataField(t, lastAuditEvent(t, h.audit.events, "secret.write"), "secret_owner"); present {
		t.Error("operator PUT's audit event carries secret_owner; it must be absent for the operator namespace")
	}

	w := doSSO(t, srv, http.MethodGet, "/api/v1/secrets", admin, "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET /secrets = %d: %s", w.Code, w.Body.String())
	}
	var body struct {
		Names []string `json:"names"`
		Mine  []string `json:"mine"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !slices.Equal(body.Names, body.Mine) {
		t.Fatalf("operator names=%v mine=%v, want equal", body.Names, body.Mine)
	}
	if !slices.Contains(body.Names, "npm-token") {
		t.Fatalf("names = %v, missing npm-token", body.Names)
	}

	if w := doSSO(t, srv, http.MethodDelete, "/api/v1/secrets/npm-token", admin, ""); w.Code != http.StatusNoContent {
		t.Fatalf("admin DELETE = %d: %s", w.Code, w.Body.String())
	}
	if _, err := sec.Get(context.Background(), "npm-token"); !errors.Is(err, secretstore.ErrNotFound) {
		t.Fatalf("Get after admin delete: err=%v, want ErrNotFound", err)
	}
	if _, present := auditDataField(t, lastAuditEvent(t, h.audit.events, "secret.delete"), "secret_owner"); present {
		t.Error("operator DELETE's audit event carries secret_owner; it must be absent for the operator namespace")
	}
}

// TestPutSecret_AdminOwnerParam_LandsInMemberNamespace: ?owner= on PUT is
// honoured exactly as on DELETE/GET — an admin's cross-write lands in the
// NAMED member's namespace and never in the operator's (which is the Get
// fallback for every member's runs, the one place a per-principal write must
// not land by accident). Negative control: a member naming ?owner= gets the
// same constant 403 the sibling endpoints give.
func TestPutSecret_AdminOwnerParam_LandsInMemberNamespace(t *testing.T) {
	sec := &memSecrets{m: map[string][]byte{}}
	h, srv := secretsRBACServer(t, sec)
	admin := ssoSession(t, "admin-1", "admin@corp.example", oidc.RoleAdmin)

	w := doSSO(t, srv, http.MethodPut, "/api/v1/secrets/anthropic-api-key?owner=bob", admin, `{"value":"sk-ant-bobs-key-value-0000"}`)
	if w.Code != http.StatusNoContent {
		t.Fatalf("admin PUT ?owner=bob = %d, want 204: %s", w.Code, w.Body.String())
	}
	if got, err := sec.For("bob").Get(context.Background(), "anthropic-api-key"); err != nil || string(got) != "sk-ant-bobs-key-value-0000" {
		t.Fatalf("bob's row = (%q, %v), want the cross-written value", got, err)
	}
	if _, err := sec.Get(context.Background(), "anthropic-api-key"); !errors.Is(err, secretstore.ErrNotFound) {
		t.Fatalf("operator namespace has a row after an admin's ?owner=bob PUT (err=%v); it must not", err)
	}
	ev := lastAuditEvent(t, h.audit.events, "secret.write")
	if owner, ok := auditDataField(t, ev, "secret_owner"); !ok || owner != "bob" {
		t.Errorf("audit secret_owner = (%q, present=%v), want bob", owner, ok)
	}

	t.Run("negative control: a member naming ?owner= is refused before any write", func(t *testing.T) {
		sec := &memSecrets{m: map[string][]byte{}}
		_, srv := secretsRBACServer(t, sec)
		alice := ssoSession(t, "alice", "alice@corp.example", oidc.RoleMember)
		w := doSSO(t, srv, http.MethodPut, "/api/v1/secrets/anthropic-api-key?owner=bob", alice, `{"value":"sk-ant-bobs-key-value-0000"}`)
		if w.Code != http.StatusForbidden {
			t.Fatalf("member PUT ?owner=bob = %d, want 403: %s", w.Code, w.Body.String())
		}
		if len(sec.m) != 0 {
			t.Fatalf("a refused PUT wrote %d row(s); it must write none", len(sec.m))
		}
	})
}

// TestDeleteSecret_MemberBedrockNames_403: the four Bedrock/SigV4 names are
// refused for a non-operator on DELETE as well as PUT (the guard lives in the
// shared writableSecretName), while an operator may still delete them.
func TestDeleteSecret_MemberBedrockNames_403(t *testing.T) {
	for _, name := range []string{bedrockAccessKeyIDSecret, bedrockSecretAccessKeySecret, bedrockSessionTokenSecret, bedrockAPIKeySecret} {
		t.Run(name, func(t *testing.T) {
			_, srv := secretsRBACServer(t, &memSecrets{m: map[string][]byte{}})
			alice := ssoSession(t, "alice", "alice@corp.example", oidc.RoleMember)
			if w := doSSO(t, srv, http.MethodDelete, "/api/v1/secrets/"+name, alice, ""); w.Code != http.StatusForbidden {
				t.Fatalf("member DELETE %s = %d, want 403: %s", name, w.Code, w.Body.String())
			}
		})
	}
	t.Run("negative control: an operator may still DELETE it", func(t *testing.T) {
		_, srv := secretsRBACServer(t, &memSecrets{m: map[string][]byte{}})
		admin := ssoSession(t, "admin-1", "admin@corp.example", oidc.RoleAdmin)
		if w := doSSO(t, srv, http.MethodDelete, "/api/v1/secrets/"+bedrockAPIKeySecret, admin, ""); w.Code != http.StatusNoContent {
			t.Fatalf("operator DELETE = %d, want 204: %s", w.Code, w.Body.String())
		}
	})
}

// TestSecretCountCap is secretsMaxPerOwner (PF-38): one namespace holds at most
// 100 secrets, refused 422 like both sibling per-principal caps.
//
// The THIRD leg is the one the cap exists to get right. An OVERWRITE at the cap
// must still succeed: `Put` is both "add" and "rotate", so a cap that counted
// rows without asking whether this name is already one of them would refuse an
// operator sitting at 100 the ability to replace an expiring key — a soft guard
// turned into an outage on the one day it matters. Counterfactual: drop
// admitSecretCount's slices.Contains and only that subtest goes red.
func TestSecretCountCap(t *testing.T) {
	const value = `{"value":"long-enough-to-be-masked-and-scanned"}`

	// fill returns a store already holding n operator-namespace rows.
	fill := func(n int) *memSecrets {
		m := map[string][]byte{}
		for i := range n {
			m[fmt.Sprintf("seeded-%03d", i)] = []byte("long-enough-to-be-masked")
		}
		return &memSecrets{m: m}
	}
	admin := func(t *testing.T) *http.Cookie {
		t.Helper()
		return ssoSession(t, "admin-1", "admin@corp.example", oidc.RoleAdmin)
	}

	t.Run("the 100th name is stored", func(t *testing.T) {
		sec := fill(secretsMaxPerOwner - 1)
		_, srv := secretsRBACServer(t, sec)
		if w := doSSO(t, srv, http.MethodPut, "/api/v1/secrets/one-more", admin(t), value); w.Code != http.StatusNoContent {
			t.Fatalf("PUT at %d rows = %d, want 204: %s", secretsMaxPerOwner-1, w.Code, w.Body.String())
		}
		if len(sec.m) != secretsMaxPerOwner {
			t.Errorf("rows = %d, want %d", len(sec.m), secretsMaxPerOwner)
		}
	})

	t.Run("the 101st is refused 422, and nothing is written", func(t *testing.T) {
		sec := fill(secretsMaxPerOwner)
		_, srv := secretsRBACServer(t, sec)
		w := doSSO(t, srv, http.MethodPut, "/api/v1/secrets/one-too-many", admin(t), value)
		if w.Code != http.StatusUnprocessableEntity {
			t.Fatalf("PUT at the cap = %d, want 422: %s", w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), "delete one first") {
			t.Errorf("body = %s, want the sibling caps' remove-one-first shape", w.Body.String())
		}
		if _, ok := sec.m["one-too-many"]; ok {
			t.Error("the refused name was stored anyway; the check must precede the Put")
		}
	})

	t.Run("THE LEG: an overwrite AT the cap still rotates", func(t *testing.T) {
		sec := fill(secretsMaxPerOwner)
		_, srv := secretsRBACServer(t, sec)
		w := doSSO(t, srv, http.MethodPut, "/api/v1/secrets/seeded-000", admin(t), `{"value":"rotated-value-long-enough"}`)
		if w.Code != http.StatusNoContent {
			t.Fatalf("overwrite at the cap = %d, want 204 — an operator at the cap must still be able to rotate a key: %s",
				w.Code, w.Body.String())
		}
		if got := string(sec.m["seeded-000"]); got != "rotated-value-long-enough" {
			t.Errorf("stored value = %q, want the rotated one", got)
		}
	})

	t.Run("the cap is PER NAMESPACE, not deployment-wide", func(t *testing.T) {
		// A member's own namespace is counted on its own rows: the operator
		// sitting at the cap must not lock every member out of their first
		// secret (For(owner).List is own-rows-only).
		sec := fill(secretsMaxPerOwner)
		_, srv := secretsRBACServer(t, sec)
		alice := ssoSession(t, "alice", "alice@corp.example", oidc.RoleMember)
		if w := doSSO(t, srv, http.MethodPut, "/api/v1/secrets/mine", alice, value); w.Code != http.StatusNoContent {
			t.Fatalf("member PUT with a FULL operator namespace = %d, want 204: %s", w.Code, w.Body.String())
		}
	})
}

// TestReservedSecret_HarnessBlobSealedByPattern pins the reason the managed
// harness blob no longer needs a static reservedSecretNames row: reservedSecret's
// wardyn-harness-<provider>-oauth PATTERN already covers it. The literal name is
// spelled out on purpose — the entry it replaces was a literal, and this must
// fail if the pattern is ever narrowed, independently of the harness catalog.
func TestReservedSecret_HarnessBlobSealedByPattern(t *testing.T) {
	const blob = "wardyn-harness-anthropic-oauth"
	if reservedSecretNames[blob] {
		t.Fatalf("%s is back in the static set — this test is meant to prove the PATTERN seals it", blob)
	}
	if blob != harnessCredSecretName("anthropic") {
		t.Fatalf("harnessCredSecretName(\"anthropic\") = %q, want %q", harnessCredSecretName("anthropic"), blob)
	}
	// All three guards that build on reservedSecret must still refuse it: the
	// generic secrets API (Put/Delete/List), the credential sinks, and the
	// cross-package platform check cmd/wardynd asserts through.
	if !reservedSecret(blob) {
		t.Errorf("reservedSecret(%q) = false — the generic secrets API would list and clobber the managed OAuth blob", blob)
	}
	if !secretsAPIReserved(blob) {
		t.Errorf("secretsAPIReserved(%q) = false", blob)
	}
	if !sinkReservedSecret(blob) {
		t.Errorf("sinkReservedSecret(%q) = false — an api_key grant naming it would resolve the raw blob", blob)
	}
	if !ReservedPlatformSecret(blob) {
		t.Errorf("ReservedPlatformSecret(%q) = false", blob)
	}
}

// ─── group C: the member seam's two ends ─────────────────────────────────────

// TestListSecrets_CrossUserReadIsAudited pins the ASYMMETRY, which is the
// finding rather than "a read was unlogged". `?owner=` is admin-only, and every
// WRITE through it stamps secret_owner — secret.write and secret.delete both
// do. The read was the one verb on that surface that recorded nothing, so an
// admin could enumerate another human's secret namespace and leave an
// investigator nothing to find, on the same query parameter whose writes are
// attributable.
//
// The row records an ENUMERATION, not a disclosure: this handler returns
// store.List, i.e. names only — no value is read, decrypted or returned (value
// resolution is secret.read, at injection time, a different action carrying
// grant_id/jti). The assertions below pin that wording by pinning the shape:
// secret_owner plus a COUNT, never the names themselves.
//
// Counterfactual: delete the recordAudit block and the cross-user read leaves
// zero events while the sibling PUT still stamps secret_owner — the exact
// asymmetry.
func TestListSecrets_CrossUserReadIsAudited(t *testing.T) {
	sec := &memSecrets{m: map[string][]byte{}}
	h, srv := secretsRBACServer(t, sec)
	admin := ssoSession(t, "admin-1", "admin@corp.example", oidc.RoleAdmin)

	// Two secrets in bob's namespace, so the count is not trivially 0 or 1.
	for _, n := range []string{"anthropic-api-key", "openai-api-key"} {
		if err := sec.For("sub-bob").Put(context.Background(), n, []byte("v-"+n)); err != nil {
			t.Fatalf("seed %s: %v", n, err)
		}
	}

	w := doSSO(t, srv, http.MethodGet, "/api/v1/secrets?owner=sub-bob", admin, "")
	if w.Code != http.StatusOK {
		t.Fatalf("admin GET ?owner=sub-bob = %d, want 200: %s", w.Code, w.Body.String())
	}
	// The fixture must actually return bob's namespace, or the audit assertion
	// below is about a read that saw nothing.
	if !strings.Contains(w.Body.String(), "anthropic-api-key") {
		t.Fatalf("the cross-user read returned %s — it did not reach bob's namespace, so this test proves nothing",
			w.Body.String())
	}

	ev := lastAuditEvent(t, h.audit.events, "secret.list")
	if owner, ok := auditDataField(t, ev, "secret_owner"); !ok || owner != "sub-bob" {
		t.Errorf("secret.list secret_owner = (%q, present=%v), want sub-bob — the same marker the WRITE path stamps "+
			"on this same ?owner= surface", owner, ok)
	}
	if ev.Target != "sub-bob" {
		t.Errorf("target = %q, want the namespace read", ev.Target)
	}
	// Content-free: a count, never the names.
	body, _ := json.Marshal(ev.Data)
	for _, name := range []string{"anthropic-api-key", "openai-api-key"} {
		if strings.Contains(string(body), name) {
			t.Errorf("audit data carries the secret NAME %q; the row is count/shape-only by design: %s", name, body)
		}
	}
	// A JSON number, not a string — auditDataField only reads strings, so this
	// one is decoded directly rather than pretending the count is text.
	var data map[string]any
	if err := json.Unmarshal(ev.Data, &data); err != nil {
		t.Fatalf("unmarshal audit Data: %v", err)
	}
	if n, ok := data["names"].(float64); !ok || int(n) != 2 {
		t.Errorf("names count = %v (%T), want 2 — the row records HOW MUCH was enumerated", data["names"], data["names"])
	}
}

// TestListSecrets_OwnNamespaceReadIsNotAudited is the bound. A member listing
// their own namespace is the ordinary console poll on every page load; auditing
// it would bury the cross-user reads the row above exists for. Without this,
// "audit the read" quietly becomes "audit every page load".
func TestListSecrets_OwnNamespaceReadIsNotAudited(t *testing.T) {
	sec := &memSecrets{m: map[string][]byte{}}
	h, srv := secretsRBACServer(t, sec)
	alice := ssoSession(t, "alice", "alice@corp.example", oidc.RoleMember)

	if w := doSSO(t, srv, http.MethodGet, "/api/v1/secrets", alice, ""); w.Code != http.StatusOK {
		t.Fatalf("member GET /secrets = %d, want 200: %s", w.Code, w.Body.String())
	}
	for _, ev := range h.audit.events {
		if ev.Action == "secret.list" {
			t.Fatalf("a member's own-namespace list emitted secret.list (%+v) — the row is for the admin-only "+
				"?owner= surface, not for every console poll", ev)
		}
	}
}
