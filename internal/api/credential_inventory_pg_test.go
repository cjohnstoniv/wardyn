// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	secretspg "github.com/cjohnstoniv/wardyn/internal/secretstore/pg"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// CS-6 against a real Postgres secret store, wrapped as wardynd wraps it
// (secretstore.Audited): the sink's last_used_at stamp, the admin inventory
// and each person's own metadata on provider_access. Guarded by
// WARDYN_TEST_PG (throwawayPGPool); skipped cleanly when unset.

func auditedPGSecrets(t *testing.T) (secretstore.Store, *pgxpool.Pool) {
	t.Helper()
	pool := throwawayPGPool(t)
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	sec, err := secretspg.New(pool, id)
	if err != nil {
		t.Fatal(err)
	}
	return secretstore.Audited(sec, &memAudit{}), pool
}

func rowLastUsed(t *testing.T, pool *pgxpool.Pool, owner, name string) *time.Time {
	t.Helper()
	var at *time.Time
	if err := pool.QueryRow(context.Background(),
		`SELECT last_used_at FROM secrets WHERE owned_by=$1 AND name=$2`, owner, name).Scan(&at); err != nil {
		t.Fatalf("read last_used_at: %v", err)
	}
	return at
}

// TestPG_ProviderKeySinkStampsLastUsed: resolving a run's provider key stamps
// the owner's row; a second resolve inside the minute leaves it as it was.
// RED with the throttle dropped from pg Store.MarkUsed.
func TestPG_ProviderKeySinkStampsLastUsed(t *testing.T) {
	h, st, g := keySinkGrant(t)
	sec, pool := auditedPGSecrets(t)
	name := h.broker.minted.Injection.SecretName
	if err := sec.For(mpOwner).Put(context.Background(), name, []byte(mpOwnerKey)); err != nil {
		t.Fatal(err)
	}
	h.srv.cfg.Secrets = sec
	resolve := func() {
		t.Helper()
		rr := do(t, h.srv, http.MethodGet, "/api/v1/internal/injection/"+g.ID.String(), h.mintRunToken(t, st.run.ID), "")
		if rr.Code != http.StatusOK {
			t.Fatalf("resolve = %d %s", rr.Code, rr.Body.String())
		}
	}
	if at := rowLastUsed(t, pool, mpOwner, name); at != nil {
		t.Fatalf("stamped before any use: %v", at)
	}
	resolve()
	first := rowLastUsed(t, pool, mpOwner, name)
	if first == nil {
		t.Fatal("the sink resolved the owner's key without stamping last_used_at")
	}
	time.Sleep(20 * time.Millisecond)
	resolve()
	if again := rowLastUsed(t, pool, mpOwner, name); again == nil || !again.Equal(*first) {
		t.Fatalf("a resolve inside the minute re-wrote last_used_at: %v -> %v", first, again)
	}
}

// TestPG_ProviderSubscriptionSinkStampsLastUsed: resolving a run's Claude
// subscription sign-in stamps the owner's row, same as the key sink. RED with
// the stamp dropped from the subscription sink.
func TestPG_ProviderSubscriptionSinkStampsLastUsed(t *testing.T) {
	p := subProvider("claude")
	h, st, _ := subHarness(t, p)
	if _, ok := dispatchSub(h, st, &types.RunPolicySpec{}, map[string]string{}, nil); !ok || len(st.grants) != 1 {
		t.Fatalf("dispatch: ok=%v grants=%d (failed: %q)", ok, len(st.grants), st.failed)
	}
	sec, pool := auditedPGSecrets(t)
	name := providerSecretName(p.UID, providerOAuthPart)
	if err := sec.For(subOwner).Put(context.Background(), name, subBlob(subOwnerToken)); err != nil {
		t.Fatal(err)
	}
	h.srv.cfg.Secrets = sec
	if code, body := resolveSub(t, h, st, "api.anthropic.com"); code != http.StatusOK {
		t.Fatalf("resolve = %d %s", code, body)
	}
	if rowLastUsed(t, pool, subOwner, name) == nil {
		t.Fatal("the subscription sink resolved the owner's sign-in without stamping last_used_at")
	}
}

// TestPG_AWSSSOSinkStampsLastUsed: resolving a run's AWS sign-in stamps the
// owner's row, same as the key sink. RED with the stamp dropped from the AWS
// sink.
func TestPG_AWSSSOSinkStampsLastUsed(t *testing.T) {
	f := newReauthFixture(t, nil)
	sec, pool := auditedPGSecrets(t)
	name := harnessCredSecretName(awsSSOProvider)
	raw, _ := json.Marshal(liveSSOBlob())
	if err := sec.For("alice@example.com").Put(context.Background(), name, raw); err != nil {
		t.Fatal(err)
	}
	f.srv.cfg.Secrets = sec
	if w := f.resolve(t); w.Code != http.StatusOK {
		t.Fatalf("resolve = %d %s", w.Code, w.Body.String())
	}
	if rowLastUsed(t, pool, "alice@example.com", name) == nil {
		t.Fatal("the AWS sign-in sink resolved the owner's session without stamping last_used_at")
	}
}

// inventoryFixture: four providers and the credentials three people and the
// operator hold for them; the values are what no response may carry.
type inventoryFixture struct {
	srv    *Server
	pool   *pgxpool.Pool
	key    types.ModelProvider
	values []string
}

const (
	invAlice, invBob, invCarol = "sub-alice", "sub-bob", "sub-carol"
	invAliceEmail              = "alice-directory@corp.example"
)

func newInventoryFixture(t *testing.T) inventoryFixture {
	t.Helper()
	key := paKeyProvider("anthropic", types.ModelProviderAnthropicAPIKey)
	sub := paKeyProvider("claude-sub", types.ModelProviderAnthropicSubscription)
	sso := paSSOProvider("bedrock")
	unused := paKeyProvider("openai", types.ModelProviderOpenAIAPIKey)
	site := types.SiteConfig{ModelProviders: providerBlock(key, sub, sso, unused),
		AgentProviders: agentBlock(types.AgentProvider{ID: "claude-code", Mechanism: types.AgentMechanismAnthropicAPIKey})}
	srv := modelProvidersStatusSrv(t, site, &capStore{})
	// review finding F9: a real directory entry for alice (an api_tokens
	// pairing) and one for bob (workspaces.owned_by, no email — the shape
	// resolveSecretOwner's own directory takes), so TestPG_CredentialInventory
	// proves email population end to end (the real knownPrincipals/
	// emailsByPrincipal wiring, not a fixed map) AND that a principal the
	// directory knows with NO paired email (bob) gets none invented from the
	// bare subject. Carol is in neither list, so her row proves the same for
	// a principal the directory doesn't know at all.
	if is, ok := srv.cfg.Store.(*integStore); ok {
		is.apiTokens = []types.APIToken{{Principal: invAlice, Email: invAliceEmail}}
		is.workspaces = []types.Workspace{{OwnedBy: invBob}}
	}
	sec, pool := auditedPGSecrets(t)
	srv.cfg.Secrets = sec
	ctx := context.Background()
	f := inventoryFixture{srv: srv, pool: pool, key: key}
	put := func(ctx context.Context, owner, name, value string) {
		t.Helper()
		if err := sec.For(owner).Put(ctx, name, []byte(value)); err != nil {
			t.Fatal(err)
		}
		f.values = append(f.values, value)
	}
	keyName := providerSecretName(key.UID, providerKeyPart)
	put(ctx, invAlice, keyName, "sk-ant-alice-inventory-0123456789")
	put(ctx, invBob, keyName, "sk-ant-bob-inventory-0123456789")
	put(ctx, "", keyName, "sk-ant-operator-inventory-0123456789")
	put(ctx, invAlice, providerSecretName(sub.UID, providerOAuthPart), `{"token":"sk-ant-oat-alice-inventory"}`)
	put(secretstore.WithExpiry(ctx, time.Now().Add(-time.Hour)), invCarol, providerSecretName(sso.UID, providerSSOPart),
		`{"access_token":"aws-carol-inventory-token"}`)
	if err := sec.For(invAlice).(secretstore.MetaStore).MarkUsed(ctx, keyName); err != nil {
		t.Fatal(err)
	}
	return f
}

// assertNoSecretMaterial fails if body carries any stored value, a stored
// credential's name, or a row's key reference.
func (f inventoryFixture) assertNoSecretMaterial(t *testing.T, body string) {
	t.Helper()
	for _, v := range append([]string{types.ModelProviderSecretPrefix, "local:", "ciphertext", "wrapped_dek", "kek_id"}, f.values...) {
		if strings.Contains(body, v) {
			t.Fatalf("response carries secret material %q: %s", v, body)
		}
	}
}

// TestPG_CredentialInventory: each person × provider with its state, store,
// added and last used, and the counts; never the operator's row, never a
// value. security_admin reads it too (K5-A); a member is refused.
func TestPG_CredentialInventory(t *testing.T) {
	f := newInventoryFixture(t)
	w := do(t, f.srv, http.MethodGet, "/api/v1/model-providers/credentials", adminToken, "")
	if w.Code != http.StatusOK {
		t.Fatalf("admin GET = %d %s", w.Code, w.Body.String())
	}
	f.assertNoSecretMaterial(t, w.Body.String())
	var inv credentialInventory
	if err := json.Unmarshal(w.Body.Bytes(), &inv); err != nil {
		t.Fatal(err)
	}
	type key struct{ person, provider, state string }
	got := map[key]credentialInventoryRow{}
	for _, r := range inv.Credentials {
		if r.Store != "pg" || r.AddedAt.IsZero() {
			t.Errorf("row %+v: want store pg and an added time", r)
		}
		got[key{r.Person, r.Provider, r.State}] = r
	}
	want := []key{
		{invAlice, "anthropic", credStateStored}, {invAlice, "claude-sub", credStateStored},
		{invBob, "anthropic", credStateStored}, {invCarol, "bedrock", credStateExpired},
	}
	if len(got) != len(want) || len(inv.Credentials) != len(want) {
		t.Fatalf("inventory = %+v, want exactly %v", inv.Credentials, want)
	}
	for _, k := range want {
		if _, ok := got[k]; !ok {
			t.Fatalf("inventory = %+v, missing %v", inv.Credentials, k)
		}
	}
	if r := got[key{invAlice, "anthropic", credStateStored}]; r.LastUsedAt == nil {
		t.Errorf("alice's used key has no last_used_at: %+v", r)
	}
	if r := got[key{invBob, "anthropic", credStateStored}]; r.LastUsedAt != nil {
		t.Errorf("bob's never-used key has last_used_at: %+v", r)
	}
	if r := got[key{invCarol, "bedrock", credStateExpired}]; r.ExpiresAt == nil {
		t.Errorf("carol's expired sign-in carries no expires_at: %+v", r)
	}
	// design F-2, review finding F9: email comes from the REAL directory
	// (knownPrincipals), end to end — not a client-visible field the handler
	// could fake. Alice is seeded into it above; bob and carol are not, so
	// they must get no email invented from their bare subject.
	if r := got[key{invAlice, "anthropic", credStateStored}]; r.Email != invAliceEmail {
		t.Errorf("alice's row email = %q, want %q from the directory", r.Email, invAliceEmail)
	}
	if r := got[key{invBob, "anthropic", credStateStored}]; r.Email != "" {
		t.Errorf("bob's row invented an email (%q) with no directory pairing", r.Email)
	}
	if r := got[key{invCarol, "bedrock", credStateExpired}]; r.Email != "" {
		t.Errorf("carol's row invented an email (%q) with no directory pairing", r.Email)
	}
	wantCounts := credentialInventoryCounts{People: 3, Credentials: 4,
		ByProvider: map[string]int{"anthropic": 2, "claude-sub": 1, "bedrock": 1, "openai": 0}}
	if gc, _ := json.Marshal(inv.Counts); string(gc) != string(mustJSON(wantCounts)) {
		t.Errorf("counts = %s, want %s", gc, mustJSON(wantCounts))
	}

	sec := ssoSession(t, "sub-sec", "sec@corp.example", oidc.RoleSecurityAdmin)
	if w := doSSO(t, f.srv, http.MethodGet, "/api/v1/model-providers/credentials", sec, ""); w.Code != http.StatusOK {
		t.Errorf("security_admin GET = %d, want 200 (K5-A: both admin tiers)", w.Code)
	}
	member := ssoSession(t, invAlice, "alice@corp.example", oidc.RoleUser)
	w = doSSO(t, f.srv, http.MethodGet, "/api/v1/model-providers/credentials", member, "")
	if w.Code != http.StatusForbidden {
		t.Fatalf("member GET = %d %s, want 403", w.Code, w.Body.String())
	}
	f.assertNoSecretMaterial(t, w.Body.String())
}

// TestPG_ProviderAccessMetadataIsTheCallersOwn: each person's provider_access
// row carries their OWN credential's added and last-used times, and a person
// with none gets none — never another person's. RED when the attach reads
// MetadataEverywhere (or the view's own scope is widened).
func TestPG_ProviderAccessMetadataIsTheCallersOwn(t *testing.T) {
	f := newInventoryFixture(t)
	keyName := providerSecretName(f.key.UID, providerKeyPart)
	bobUsed := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	if _, err := f.pool.Exec(context.Background(), `UPDATE secrets SET last_used_at=$3::timestamptz, created_at=$3::timestamptz - interval '1 day'
		WHERE owned_by=$1 AND name=$2`, invBob, keyName, bobUsed); err != nil {
		t.Fatal(err)
	}
	access := func(sub string) SetupProviderAccess {
		t.Helper()
		w := doSSO(t, f.srv, http.MethodGet, "/api/v1/setup/status", ssoSession(t, sub, sub+"@corp.example", oidc.RoleUser), "")
		if w.Code != http.StatusOK {
			t.Fatalf("GET /setup/status as %s = %d %s", sub, w.Code, w.Body.String())
		}
		f.assertNoSecretMaterial(t, w.Body.String())
		var st SetupStatus
		if err := json.Unmarshal(w.Body.Bytes(), &st); err != nil {
			t.Fatal(err)
		}
		for _, a := range st.ProviderAccess {
			if a.Provider == f.key.ID {
				return a
			}
		}
		t.Fatalf("%s: no provider_access row for %s: %+v", sub, f.key.ID, st.ProviderAccess)
		return SetupProviderAccess{}
	}

	alice := access(invAlice)
	if alice.AddedAt == nil || alice.LastUsedAt == nil || alice.LastUsedAt.Equal(bobUsed) || alice.AddedAt.Before(bobUsed) {
		t.Errorf("alice's own metadata = added %v, last used %v; want her own row's, never bob's (%v)", alice.AddedAt, alice.LastUsedAt, bobUsed)
	}
	bob := access(invBob)
	if bob.LastUsedAt == nil || !bob.LastUsedAt.Equal(bobUsed) || bob.AddedAt == nil || !bob.AddedAt.Equal(bobUsed.Add(-24*time.Hour)) {
		t.Errorf("bob's own metadata = added %v, last used %v; want his row's", bob.AddedAt, bob.LastUsedAt)
	}
	if carol := access(invCarol); carol.AddedAt != nil || carol.LastUsedAt != nil || carol.State != modelAccessNotConfigured {
		t.Errorf("carol stores no key, yet provider_access = %+v", carol)
	}
}
