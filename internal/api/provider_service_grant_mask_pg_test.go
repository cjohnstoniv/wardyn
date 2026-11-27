// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// The service grant's token in the shared masking registry, against a real Postgres: the grant's record sealed
// in the Postgres secret store, the token sealed in the run's mask rows, and a restarted process that masks the
// run's output from those rows alone. Guarded by WARDYN_TEST_PG (throwawayPGPool); skipped cleanly when unset.

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/cjohnstoniv/wardyn/internal/adorunpat"
	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/identity"
	"github.com/cjohnstoniv/wardyn/internal/maskmanifest"
	"github.com/cjohnstoniv/wardyn/internal/maskstore"
	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	secretspg "github.com/cjohnstoniv/wardyn/internal/secretstore/pg"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// svcReplica is one wardynd of the lab with the service grant wired: the lab's Postgres secret store, masking
// manifests and shared registry, the scripted identity provider, and the test endpoints allowed.
func (l *maskLab) svcReplica(idp *svcIdP) replica {
	l.t.Helper()
	sec, err := secretspg.New(l.pool, l.id)
	if err != nil {
		l.t.Fatal(err)
	}
	reg := secretmask.NewRegistry()
	rs := &fakeRecordingStore{}
	fr := &holderTestRunner{}
	cfg := baseTestConfig(l.h, store.NewPG(l.pool))
	cfg.Audit = l.rec
	cfg.Runner = fr
	cfg.OIDC = &oidc.Authenticator{}
	cfg.Secrets = sec
	cfg.RecordingStore = rs
	cfg.MaskRegistry = reg
	cfg.RunnerAuditPrepare = svcPrepare(reg, new(atomic.Int64))
	cfg.MaskManifests = maskmanifest.New(l.pool, sec.SubjectKeys(), reg)
	cfg.ADORunPATs = adorunpat.New(l.pool, sec.SubjectKeys())
	cfg.ServiceGrantTransport = idp
	cfg.AllowTestEndpoints = true
	cfg.HostResolver = func(string) ([]net.IP, error) { return []net.IP{net.ParseIP("203.0.113.9")}, nil }
	st := maskstore.New(l.pool, sec.SubjectKeys(), reg)
	return replica{srv: testServer(l.t, cfg), reg: reg, rec: rs, fr: fr, sec: sec, st: st}
}

// svcRun persists a RUNNING run with a sandbox, owned by the grant's service identity.
func (l *maskLab) svcRun() types.AgentRun {
	l.t.Helper()
	now := time.Now().UTC()
	id := uuid.New()
	r := types.AgentRun{
		ID: id, CreatedAt: now, UpdatedAt: now, CreatedBy: svcOwner, OperatorOwned: true,
		Agent: "claude-code", Task: "service grant mask", ConfinementClass: types.CC2, State: types.RunRunning,
		SPIFFEID: "spiffe://wardyn.test/agent-run/" + id.String(), RunnerTarget: "docker", SandboxRef: "sbx-1",
	}
	pg := store.NewPG(l.pool)
	created, err := pg.CreateRun(context.Background(), r)
	if err != nil {
		l.t.Fatalf("create run: %v", err)
	}
	if err := pg.SetSandboxRef(context.Background(), id, "sbx-1"); err != nil {
		l.t.Fatal(err)
	}
	created.SandboxRef = "sbx-1"
	return created
}

// svcCaptureLogs routes the default logger, at every level, into the returned buffer for the test.
func svcCaptureLogs(t *testing.T) *lockedBuffer {
	t.Helper()
	var logs lockedBuffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &logs
}

// svcColumnsHolding names every text, character, JSON, array and bytea column of the schema that holds any of
// values, as table.column.
func svcColumnsHolding(t *testing.T, l *maskLab, values ...string) []string {
	t.Helper()
	ctx := context.Background()
	rows, err := l.pool.Query(ctx, `SELECT table_name::text, column_name::text, data_type::text FROM information_schema.columns
		WHERE table_schema = current_schema() AND data_type IN ('text','character varying','character','json','jsonb','bytea','ARRAY')`)
	if err != nil {
		t.Fatal(err)
	}
	type col struct{ table, name, kind string }
	var cols []col
	for rows.Next() {
		var c col
		if err := rows.Scan(&c.table, &c.name, &c.kind); err != nil {
			t.Fatal(err)
		}
		cols = append(cols, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(cols) == 0 {
		t.Fatal("the column scan found no columns to read")
	}
	var hits []string
	for _, c := range cols {
		ident := pgx.Identifier{c.name}.Sanitize()
		match := `position($1::text in ` + ident + `::text) > 0`
		if c.kind == "bytea" {
			match = `position(convert_to($1::text, 'UTF8') in ` + ident + `) > 0`
		}
		for _, v := range values {
			var n int
			q := `SELECT count(*) FROM ` + pgx.Identifier{c.table}.Sanitize() + ` WHERE ` + match
			if err := l.pool.QueryRow(ctx, q, v).Scan(&n); err != nil {
				t.Fatalf("scan %s.%s: %v", c.table, c.name, err)
			}
			if n > 0 {
				hits = append(hits, c.table+"."+c.name)
			}
		}
	}
	return hits
}

// A run's output carrying the token is masked by a restarted process from the run's sealed mask rows: the
// process starts with an empty registry, reads the shared corpus as wardynd's boot does (maskstore.New, then
// Fresh), never mints, and still masks the token in a recording upload. No text or bytea column holds the token
// or the client secret in clear, and no log line or metric label carries either.
func TestServiceGrant_MaskCopySealedAndMasksAfterRestart_PG(t *testing.T) {
	logs := svcCaptureLogs(t)
	l := newMaskLab(t)
	ctx := context.Background()
	idp := &svcIdP{}
	a := l.svcReplica(idp)
	if _, err := store.NewPG(l.pool).PutSiteConfig(ctx, types.SiteConfig{}); err != nil {
		t.Fatal(err)
	}
	if _, err := l.pool.Exec(ctx,
		`UPDATE site_config SET config = config || jsonb_build_object($1::text, to_jsonb(now()::text)) WHERE singleton`,
		store.PortalIssuersActivatedKey); err != nil {
		t.Fatal(err)
	}
	if err := a.sec.Put(ctx, oidcSecretRef, []byte(oidcClientSecret)); err != nil {
		t.Fatal(err)
	}
	admin := ssoSession(t, "sub-admin", "admin@corp.example", oidc.RoleAdmin)
	put := func(path string, body string) {
		t.Helper()
		if w := doSSO(t, a.srv, http.MethodPut, "/api/v1"+path, admin, body); w.Code/100 != 2 {
			t.Fatalf("PUT %s: %d %s", path, w.Code, w.Body.String())
		}
	}
	row := svcPGRow("corp-a", true)
	put("/model-providers", string(mustJSON(types.ModelProviders{Providers: []types.ModelProvider{row}})))
	put("/model-providers/corp-a/service-grant/secret", string(mustJSON(putServiceGrantSecretRequest{
		Value: svcClientSecret, TokenURL: row.OIDC.ServiceGrant.TokenURL, ClientID: row.OIDC.ServiceGrant.ClientID, BaseURL: row.BaseURL})))
	sc, err := store.NewPG(l.pool).GetSiteConfig(ctx)
	if err != nil || sc.ModelProviders == nil {
		t.Fatalf("read the stored providers: %v", err)
	}
	live, found := modelProviderByID(sc.ModelProviders, "corp-a")
	if !found || live.OIDC == nil || live.OIDC.ServiceGrant == nil {
		t.Fatalf("the stored row %+v carries no grant", live)
	}

	// Dispatch's manifest, then the token through the resolve's own masking door before it is handed out.
	run := l.svcRun()
	a.dispatch(t, run)
	tok, err := a.srv.serviceGrantToken(ctx, live, serviceGrantDigest(live), serviceGrantTriggerResolve)
	if err != nil || tok.token != svcToken(1) {
		t.Fatalf("mint = %q, %v; want the first token", tok.token, err)
	}
	r := httptest.NewRequest(http.MethodGet, "/api/v1/internal/injection/"+uuid.NewString(), nil)
	claims := &identity.Claims{RunID: run.ID, Sub: svcOwner, SPIFFEID: run.SPIFFEID}
	if a.srv.refuseUnmasked(httptest.NewRecorder(), r, claims, "injection.resolve", []byte(tok.token), []byte("Bearer "+tok.token)) {
		t.Fatal("the token could not be put on record for masking")
	}
	var sealed int
	if err := l.pool.QueryRow(ctx, `SELECT count(*) FROM mask_values WHERE bucket = 'per_run' AND run_id = $1 AND NOT tombstone`,
		run.ID).Scan(&sealed); err != nil || sealed == 0 {
		t.Fatalf("sealed per-run mask rows for the run = %d (%v), want the token's copy", sealed, err)
	}

	// A new process: an empty registry, the boot's read of the shared corpus, no mint of its own.
	idp2 := &svcIdP{}
	restarted := l.svcReplica(idp2)
	if svcMasked(restarted.srv, tok.token) {
		t.Fatal("setup: a fresh process already holds the token")
	}
	if err := restarted.st.Fresh(ctx, time.Now()); err != nil {
		t.Fatalf("the boot read of the shared corpus: %v", err)
	}
	if out := string(restarted.reg.Masker(run.ID).Mask([]byte("x " + tok.token + " y"))); strings.Contains(out, tok.token) {
		t.Fatalf("the restarted registry leaves the token in the run's output: %q", out)
	}
	upload := do(t, restarted.srv, http.MethodPut, "/api/v1/internal/recordings/"+run.ID.String(),
		mintRunTokenAs(t, l.h, run.ID, svcOwner), "out: Authorization: Bearer "+tok.token+"\n")
	if w := upload; w.Code != http.StatusNoContent {
		t.Fatalf("upload on the restarted process: %d %s", w.Code, w.Body.String())
	}
	if saved := string(restarted.rec.saved); strings.Contains(saved, tok.token) || saved == "" {
		t.Fatalf("the restarted process persisted %q, want the token masked", saved)
	}
	if idp2.count() != 0 || svcMasked(restarted.srv, tok.token) {
		t.Fatalf("the restarted process minted %d times or holds the token process-wide: the mask must come from the run's rows", idp2.count())
	}

	if hits := svcColumnsHolding(t, l, tok.token, svcClientSecret); len(hits) != 0 {
		t.Errorf("columns holding the token or the client secret in clear: %v", hits)
	}
	for name, s := range map[string]*Server{"the first process": a.srv, "the restarted process": restarted.srv} {
		if m := string(s.MetricsText(ctx)); strings.Contains(m, tok.token) || strings.Contains(m, svcClientSecret) {
			t.Errorf("%s's metrics carry the token or the client secret", name)
		}
	}
	for _, ev := range l.rec.snapshot() {
		if b := string(mustJSON(ev)); strings.Contains(b, tok.token) || strings.Contains(b, svcClientSecret) {
			t.Errorf("audit row %s carries the token or the client secret", ev.Action)
		}
	}
	if got := logs.String(); strings.Contains(got, tok.token) || strings.Contains(got, svcClientSecret) {
		t.Errorf("a log line carries the token or the client secret: %q", got)
	}
}
