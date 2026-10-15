// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Self-hosting a runner against a real Postgres (0.9.1): migration 0178 restricts the self_host_runner
// feature value, so a member mints a registration token only once an allow names it; an operator is
// exempt, the admin mint is not asked, and a token an admin mints for a person's email is that person's
// to claim, by their session or by their own wdn_ API token and never by the admin token.
//
// Guarded by WARDYN_TEST_PG (throwawayBarePGPool): skipped cleanly when unset, must PASS when set.

// enableRunners switches runners on: the token doors answer runners_disabled otherwise.
func (e *poolEnv) enableRunners() {
	e.t.Helper()
	if _, err := e.pg.PutSiteConfig(context.Background(), types.SiteConfig{Runners: &types.RunnerSettings{Enabled: true}}); err != nil {
		e.t.Fatal(err)
	}
}

func (e *poolEnv) mintSelf(c *http.Cookie) (code int, body []byte) {
	e.t.Helper()
	w := e.call(c, http.MethodPost, "/api/v1/me/runners/tokens", "{}")
	return w.Code, w.Body.Bytes()
}

func TestPG_SelfHostRunnerNeedsTheGrant(t *testing.T) {
	e := newPoolEnv(t, false)
	e.enableRunners()
	ctx := context.Background()
	restricted, err := e.pg.ListCapabilityRestrictions(ctx)
	if err != nil || !restricted[capFeature][featureSelfHostRunner] {
		t.Fatalf("capability_restrictions = %v (%v), want feature/self_host_runner restricted by migration 0178", restricted, err)
	}
	if code, body := e.mintSelf(e.person); code != http.StatusForbidden || !strings.Contains(string(body), "capability_feature") {
		t.Fatalf("a member with no grant minted a runner token, or was refused for another reason: %d %s", code, body)
	}
	if code, body := e.mintSelf(e.super); code != http.StatusCreated {
		t.Fatalf("an operator was asked for the grant: %d %s", code, body)
	}
	grant := func(value string) {
		t.Helper()
		if _, err := e.pg.UpsertCapabilityGrant(ctx, types.CapabilityGrant{SubjectType: types.CapabilitySubjectAll, Capability: capFeature, Value: value, Effect: types.CapabilityAllow}); err != nil {
			t.Fatal(err)
		}
	}
	grant(capWildcard)
	if code, body := e.mintSelf(e.person); code != http.StatusForbidden {
		t.Fatalf("a wildcard allow admitted a restricted value: %d %s", code, body)
	}
	grant(featureSelfHostRunner)
	if code, body := e.mintSelf(e.person); code != http.StatusCreated {
		t.Fatalf("a member with an all allow could not mint: %d %s", code, body)
	}
	if code, body := e.mintSelf(e.other); code != http.StatusCreated {
		t.Fatalf("an all allow did not reach a second member: %d %s", code, body)
	}
}

// replay0178 runs migration 0178 again over the database the harness already migrated: the harness has no
// "stop at 0177, seed, then apply 0178" step (internal/db's partial-schema test does that), so this
// replays the file's SQL over a seeded database. The SQL is idempotent, which is what a replay needs.
func (e *poolEnv) replay0178() {
	e.t.Helper()
	sql, err := os.ReadFile("../db/migrations/0178_self_host_runner_restriction.sql")
	if err != nil {
		e.t.Fatal(err)
	}
	if _, err := e.pool.Exec(context.Background(), string(sql)); err != nil {
		e.t.Fatalf("replay 0178: %v", err)
	}
}

// seedRunner stores a runner a person registered with a token they minted themself, in the given state.
func (e *poolEnv) seedRunner(owner, state string) {
	e.t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		e.t.Fatal(err)
	}
	if _, err := e.pool.Exec(context.Background(),
		`INSERT INTO runners (id, owner, public_key, key_fingerprint, state, max_runs, minted_by) VALUES (gen_random_uuid(), $1, $2, $3, $4, 2, $1)`,
		owner, []byte(pub), runnerwire.Fingerprint(pub), state); err != nil {
		e.t.Fatal(err)
	}
}

// An estate that already self-hosts keeps it open across the upgrade: 0.9.0 cannot stage the grant, so
// the migration adds the `all` allow when someone claimed a runner whose token they minted themself.
func TestPG_Migration0178_KeepsSelfHostingForAnEstateThatHasARunner(t *testing.T) {
	e := newPoolEnv(t, false)
	e.enableRunners()
	e.seedRunner(e.otherP, "revoked") // a revoked runner alone would not count; the claimed one below does
	e.seedRunner(e.otherP, "claimed")
	e.replay0178()
	e.replay0178() // idempotent
	if n := e.count(`SELECT count(*) FROM capability_grants WHERE subject_type = 'all' AND capability = 'feature' AND value = 'self_host_runner' AND effect = 'allow' AND created_by = 'migration:0178'`); n != 1 {
		t.Fatalf("the migration left %d all-allow rows for self_host_runner, want 1", n)
	}
	if code, body := e.mintSelf(e.person); code != http.StatusCreated {
		t.Fatalf("a member of an estate that already self-hosted was refused after the upgrade: %d %s", code, body)
	}
}

// An estate with no runner, or only revoked ones, starts closed.
func TestPG_Migration0178_StartsClosedWithoutARunner(t *testing.T) {
	e := newPoolEnv(t, false)
	e.enableRunners()
	e.seedRunner(e.otherP, "revoked")
	e.replay0178()
	if n := e.count(`SELECT count(*) FROM capability_grants WHERE capability = 'feature' AND value = 'self_host_runner'`); n != 0 {
		t.Fatalf("the migration added %d self_host_runner grants to an estate with no self-minted claim", n)
	}
	if code, body := e.mintSelf(e.person); code != http.StatusForbidden || !strings.Contains(string(body), "capability_feature") {
		t.Fatalf("a member of an estate with no runner minted, or was refused for another reason: %d %s", code, body)
	}
}

// register redeems token for a fresh key and returns the unclaimed runner it made.
func (e *poolEnv) register(token string) types.RunnerRegistration {
	e.t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		e.t.Fatal(err)
	}
	body, _ := json.Marshal(types.RunnerRegisterRequest{Token: token, PublicKey: pub, Name: "laptop", Limits: testRunnerLimits()})
	w := do(e.t, e.srv, http.MethodPost, "/api/v1/runners/register", "", string(body))
	if w.Code != http.StatusCreated {
		e.t.Fatalf("register = %d %s", w.Code, w.Body)
	}
	reg := decodeJSON[types.RunnerRegistration](e.t, w)
	if reg.Fingerprint != runnerwire.Fingerprint(pub) {
		e.t.Fatalf("registered fingerprint %q", reg.Fingerprint)
	}
	return reg
}

// An admin names the person by email, the token carries the principal that email resolves to, and that
// person claims the runner two ways; the admin token cannot, and an owner nobody knows is refused.
func TestPG_AdminMintForAnEmailIsClaimableByThatPerson(t *testing.T) {
	e := newPoolEnv(t, false)
	e.enableRunners()
	_, raw := e.mintToken(e.personP, "person@corp.example", "user")
	mint := func(owner string) (types.RunnerRegistrationToken, int) {
		w := e.admin(http.MethodPost, "/api/v1/runners/tokens", `{"owner":"`+owner+`"}`)
		var tok types.RunnerRegistrationToken
		_ = json.Unmarshal(w.Body.Bytes(), &tok)
		return tok, w.Code
	}
	if _, code := mint("nobody@corp.example"); code != http.StatusUnprocessableEntity {
		t.Fatalf("a mint for an email nobody is known by = %d, want 422", code)
	}
	tok, code := mint("person@corp.example")
	if code != http.StatusCreated || tok.Owner != e.personP {
		t.Fatalf("mint for an email = %d owner %q, want 201 owned by %q", code, tok.Owner, e.personP)
	}
	claim := func(runnerID, fingerprint string, session *http.Cookie, bearer string) int {
		body := `{"fingerprint":"` + fingerprint + `"}`
		if bearer != "" {
			return do(e.t, e.srv, http.MethodPost, "/api/v1/me/runners/"+runnerID+"/claim", bearer, body).Code
		}
		return e.call(session, http.MethodPost, "/api/v1/me/runners/"+runnerID+"/claim", body).Code
	}
	reg := e.register(tok.Token)
	if got := claim(reg.RunnerID.String(), reg.Fingerprint, e.other, ""); got != http.StatusForbidden {
		t.Errorf("another member claimed the runner: %d", got)
	}
	if got := claim(reg.RunnerID.String(), reg.Fingerprint, nil, adminToken); got != http.StatusForbidden {
		t.Errorf("the admin token claimed a person's runner: %d", got)
	}
	if got := claim(reg.RunnerID.String(), reg.Fingerprint, e.person, ""); got != http.StatusOK {
		t.Errorf("the owner's session could not claim: %d", got)
	}

	second, _ := mint("person@corp.example")
	again := e.register(second.Token)
	if got := claim(again.RunnerID.String(), again.Fingerprint, nil, raw); got != http.StatusOK {
		t.Errorf("the owner's own wdn_ API token could not claim: %d", got)
	}
}
