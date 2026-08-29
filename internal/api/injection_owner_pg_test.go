// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"

	"filippo.io/age"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/broker"
	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/egress"
	secretspg "github.com/cjohnstoniv/wardyn/internal/secretstore/pg"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// throwawayPGPool creates a fresh, empty database on the WARDYN_TEST_PG
// server (dropped on cleanup) and returns a fully-migrated pool connected to
// it — this test's OWN copy of internal/store's throwawayDatabase (an
// unexported _test.go helper, not importable from this package): connecting
// straight to the shared WARDYN_TEST_PG target database instead would leak
// rows this test writes into the NEXT run of it, silently masking exactly
// the owner-namespace bug this test exists to catch (a stale operator row
// from a prior run stays "present" regardless of what this run's code does).
// Skips cleanly when WARDYN_TEST_PG is unset.
func throwawayPGPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("WARDYN_TEST_PG")
	if dsn == "" {
		t.Skip("WARDYN_TEST_PG not set; skipping Postgres-backed test")
	}
	ctx := context.Background()
	admin, err := db.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect admin: %v", err)
	}
	name := "wardyn_inj_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := admin.Exec(ctx, `CREATE DATABASE `+name); err != nil {
		admin.Close()
		t.Fatalf("create throwaway database %s: %v", name, err)
	}
	t.Cleanup(func() {
		cctx := context.Background()
		_, _ = admin.Exec(cctx, `SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname=$1 AND pid <> pg_backend_pid()`, name)
		_, _ = admin.Exec(cctx, `DROP DATABASE IF EXISTS `+name)
		admin.Close()
	})
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse WARDYN_TEST_PG: %v", err)
	}
	u.Path = "/" + name
	pool, err := db.Connect(ctx, u.String())
	if err != nil {
		t.Fatalf("connect to throwaway database %s: %v", name, err)
	}
	t.Cleanup(pool.Close)
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return pool
}

// newRunOwnerPGHarness builds newHarness's usual in-memory identity/broker/
// audit trio, but backs Config.Store with the REAL Postgres AgentRun store
// and Config.Secrets with a REAL secretstore/pg.Store (age-encrypted) instead
// of the in-memory memSecrets fake every other secrets test in this package
// uses — invariant 1 (0.7, migration 0050, member BYOK) needs to hold against
// the actual backing store, not only a fake that could quietly diverge from
// it. Guarded by WARDYN_TEST_PG (via throwawayPGPool); skipped cleanly when
// unset.
func newRunOwnerPGHarness(t *testing.T) (*harness, *secretspg.Store) {
	t.Helper()
	pool := throwawayPGPool(t)

	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatalf("generate age identity: %v", err)
	}
	sec, err := secretspg.New(pool, id)
	if err != nil {
		t.Fatalf("secretstore/pg.New: %v", err)
	}

	h := newHarness(t)
	h.srv.cfg.Store = store.NewPG(pool)
	h.srv.cfg.Secrets = sec
	h.srv.cfg.OIDC = &oidc.Authenticator{}
	h.srv.cfg.DefaultPolicy = types.RunPolicySpec{
		AllowedDomains:      []string{"api.anthropic.com"},
		MinConfinementClass: types.CC2,
		// A bare api_key ceiling entry: composer.Clamp keeps a proposed grant
		// only by KIND before filterMemberGrants' own-key arm ever runs — see
		// TestIntegrations_MemberKeySynthesisesRow_NoWarning.
		EligibleGrants: []types.GrantSpec{{Kind: types.GrantAPIKey}},
	}
	h.srv.router = h.srv.routes() // re-mount with the secret surfaces + OIDC enabled
	return h, sec
}

// TestInvariant1_PGBacked_MemberOwnRowWinsOverOperator_NoWarning is the
// plan's pg-backed end-to-end proof of invariant 1 (Verification item 12),
// against a REAL secretstore/pg store: a member's PUT /secrets write lands
// in her own namespace; a run she creates with a hand-authored inline
// api_key grant naming that secret gets no false "no model access" warning
// (the reviewer's #3 fix — handleCreateRun's model-access check now calls
// presentSecretNamesFor, not the operator-only presentSecretNames); and the
// run's injection resolves HER
// row — never an operator row seeded under the SAME name with a DIFFERENT
// value.
func TestInvariant1_PGBacked_MemberOwnRowWinsOverOperator_NoWarning(t *testing.T) {
	h, sec := newRunOwnerPGHarness(t)
	ctx := context.Background()

	alice := ssoSession(t, "alice", "alice@corp.example", oidc.RoleMember)

	// The member writes her OWN row through the real PUT /secrets handler
	// (secretOwnerFromRequest stamps it under her own namespace, persisted to
	// the real pg-backed secretstore).
	w := doSSO(t, h.srv, http.MethodPut, "/api/v1/secrets/anthropic-api-key", alice,
		`{"value":"sk-ant-alice-fake-00000000"}`)
	if w.Code != http.StatusNoContent {
		t.Fatalf("PUT /secrets: %d, want 204: %s", w.Code, w.Body.String())
	}

	// She creates a run with a hand-authored inline api_key grant naming her
	// own secret — the filterMemberGrants own-key lane (6c) — through the
	// real POST /runs handler (real create, real grant persistence).
	body := `{"agent":"claude-code","task":"t","inline_policy":{"min_confinement_class":"CC2",` +
		`"allowed_domains":["api.anthropic.com"],` +
		`"eligible_grants":[{"kind":"api_key","scope":{"host":"api.anthropic.com","secret_name":"anthropic-api-key"}}]}}`
	w = doSSO(t, h.srv, http.MethodPost, "/api/v1/runs", alice, body)
	if w.Code != http.StatusCreated {
		t.Fatalf("create run: %d, want 201: %s", w.Code, w.Body.String())
	}
	var created createRunResponse
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create-run response: %v; body=%s", err, w.Body.String())
	}
	for _, warn := range created.Warnings {
		if strings.Contains(warn, "no model credential resolves") {
			t.Fatalf("alice's own key must satisfy model access with no warning, got: %v", created.Warnings)
		}
	}

	// ONLY NOW does the operator gain a row of the SAME name with a
	// DIFFERENT value — seeded after create, so it can never leak into the
	// warning check above (which must fire on OWNERSHIP, not name presence)
	// while still proving the resolution-time negative control below: even
	// with an operator row of this exact name now in play, alice's run must
	// resolve HER value, never fall through to the operator's.
	if err := sec.Put(ctx, "anthropic-api-key", []byte("sk-ant-operator-fake-0000000000")); err != nil {
		t.Fatalf("seed operator row: %v", err)
	}

	// Resolve the run's own injection exactly as the proxy does
	// (handleInternalInjection): claims.Sub (the run's owner, "alice",
	// minted below onto THIS run's real id) resolves her row — never the
	// operator's differently-valued row of the same name.
	h.broker.minted = broker.Minted{
		Kind: types.GrantAPIKey,
		JTI:  "jti-invariant1-pg",
		Injection: &egress.InjectionRule{
			Host: "api.anthropic.com", Header: "x-api-key",
			SecretName: "anthropic-api-key", Format: "%s",
		},
	}
	token := mintRunTokenAs(t, h, created.ID, "alice")
	rr := do(t, h.srv, http.MethodGet, "/api/v1/internal/injection/"+uuid.NewString(), token, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("resolve injection: %d, want 200: %s", rr.Code, rr.Body.String())
	}
	var resp injectionResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode injection response: %v", err)
	}
	if resp.Value != "sk-ant-alice-fake-00000000" {
		t.Fatalf("resolved %q, want alice's own row (never the operator's differently-valued row)", resp.Value)
	}
}
