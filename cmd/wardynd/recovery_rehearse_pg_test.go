// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

// The rehearsal end to end on throwaway databases: a live database that takes the audit rows and a
// target holding the restored copy (recovery-set.md §7, §8, §12.1 d and e). Guarded by WARDYN_TEST_PG.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/db"
	secretstorepg "github.com/cjohnstoniv/wardyn/internal/secretstore/pg"
	"github.com/cjohnstoniv/wardyn/internal/store"
)

const (
	canaryValue    = "STORED-VALUE-CANARY-7731"
	canaryLegacy   = "LEGACY-VALUE-CANARY-4410"
	canaryLivePW   = "LIVE-PW-CANARY-9142"
	canaryTargetPW = "TARGET-PW-CANARY-5583"
	canaryToken    = "VAULT-TOKEN-CANARY-2208"
)

type rehearseRig struct {
	t                    *testing.T
	liveDSN, targetDSN   string
	liveRole, targetRole string
	id                   *age.X25519Identity
	set                  rehearseSet
	sinkPath             string
	livePool             *pgxpool.Pool
}

// rehearseRig builds live and target as two databases on one server, a target holding every stored
// shape the rehearsal opens (a boot-style operator row, a person's row, a pre-envelope row and a
// pointer row) and a few audit rows, and the manifest figures that describe it.
func newRehearseRig(t *testing.T) *rehearseRig {
	t.Helper()
	ctx := context.Background()
	rg := &rehearseRig{t: t, id: mustAgeIdentity(t), sinkPath: filepath.Join(t.TempDir(), "sink.jsonl")}
	live, target := migratedDatabase(t), migratedDatabase(t)
	rg.liveRole, rg.liveDSN = roleDSN(t, live, canaryLivePW)
	rg.targetRole, rg.targetDSN = roleDSN(t, target, canaryTargetPW)
	rg.livePool = connect(t, live)
	targetPool, err := db.Connect(ctx, target)
	if err != nil {
		t.Fatal(err)
	}
	defer targetPool.Close() // the rehearsal refuses a target that has another client

	s, err := secretstorepg.New(targetPool, rg.id)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Put(ctx, "anthropic-api-key", []byte(canaryValue)); err != nil {
		t.Fatal(err)
	}
	if err := s.For("alice@corp.example").Put(ctx, "anthropic-api-key", []byte(canaryValue)); err != nil {
		t.Fatal(err)
	}
	seedLegacyRow(t, targetPool, rg.id, "legacy-credential", canaryLegacy)
	if _, err := targetPool.Exec(ctx, `INSERT INTO secrets (owned_by, name, enc_version, kek_id, wrapped_dek, ciphertext)
		VALUES ('bob', 'vault-cred', 2, 'vaultkv:ref', ''::bytea, ''::bytea)`); err != nil {
		t.Fatal(err)
	}
	rec := store.Recorder{Pool: targetPool}
	for i := range 3 {
		if err := rec.Record(ctx, maintenanceEvent("test", "test.event", "t", "success", map[string]any{"i": i})); err != nil {
			t.Fatal(err)
		}
	}
	rg.set = rehearseSet{
		ID: uuid.NewString(), Dir: t.TempDir(), Source: dsnIdentity{Host: "source.invalid", Port: 5432, Database: "wardyn"},
		AgeRecipient: rg.id.Recipient().String(), KeyClasses: keyClasses(t, targetPool), Spool: recoverySpool{Basename: "spool.jsonl"},
		Recordings: recoveryRecordings{Store: "pg"},
	}
	rg.set.Migration, err = db.NewestMigration()
	if err != nil {
		t.Fatal(err)
	}
	if err := targetPool.QueryRow(ctx, `SELECT max(seq), (SELECT row_hash FROM audit_events ORDER BY seq DESC LIMIT 1) FROM audit_events`).Scan(&rg.set.HeadSeq, &rg.set.HeadHash); err != nil {
		t.Fatal(err)
	}
	return rg
}

// keyClasses is the manifest's per-class row count, derived from the table independently of the
// code under test.
func keyClasses(t *testing.T, pool *pgxpool.Pool) map[string]int {
	rows, err := pool.Query(context.Background(), `SELECT CASE WHEN enc_version=0 THEN 'age' WHEN enc_version=2 THEN 'external:' || split_part(kek_id, ':', 1) ELSE kek_id END, count(*) FROM secrets GROUP BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var k string
		var n int
		if err := rows.Scan(&k, &n); err != nil {
			t.Fatal(err)
		}
		out[k] = n
	}
	return out
}

func connect(t *testing.T, dsn string) *pgxpool.Pool {
	t.Helper()
	p, err := db.Connect(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	return p
}

// roleDSN makes a superuser whose password is the canary and returns dsn reached as that role.
func roleDSN(t *testing.T, dsn, password string) (role, out string) {
	t.Helper()
	ctx := context.Background()
	admin, err := db.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	role = "rehearse_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	if _, err := admin.Exec(ctx, `CREATE ROLE `+role+` LOGIN SUPERUSER PASSWORD '`+password+`'`); err != nil {
		t.Fatal(err)
	}
	admin.Close() // the rehearsal refuses a database that has another client
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	u.User = url.UserPassword(role, password)
	return role, u.String()
}

func seedLegacyRow(t *testing.T, pool *pgxpool.Pool, to *age.X25519Identity, name, value string) {
	t.Helper()
	var buf bytes.Buffer
	w, err := age.Encrypt(&buf, to.Recipient())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte(value)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(context.Background(), `INSERT INTO secrets (owned_by, name, ciphertext) VALUES ('', $1, $2)`, name, buf.Bytes()); err != nil {
		t.Fatal(err)
	}
}

// flags resolves the deployment's boot flags the way main does: from the environment.
func (rg *rehearseRig) flags(ageKey string) *bootFlags { return rg.flagsArgs(ageKey) }

// flagsArgs is flags with command-line arguments.
func (rg *rehearseRig) flagsArgs(ageKey string, args ...string) *bootFlags {
	rg.t.Helper()
	tokenFile := filepath.Join(rg.t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte(canaryToken), 0o600); err != nil {
		rg.t.Fatal(err)
	}
	sinks, _ := json.Marshal(map[string]any{"file": map[string]any{"path": rg.sinkPath}})
	for k, v := range map[string]string{
		"WARDYN_PG_DSN": rg.liveDSN, "WARDYN_AGE_KEY": ageKey, "WARDYN_AUDIT_SINKS": string(sinks),
		"WARDYN_AUDIT_SPOOL": filepath.Join(rg.t.TempDir(), "spool.jsonl"), "WARDYN_RECORDING_DIR": rg.t.TempDir(),
		"WARDYN_VAULT_TOKEN_FILE": tokenFile,
	} {
		rg.t.Setenv(k, v)
	}
	resetFlags(rg.t)
	oldArgs := os.Args
	os.Args = append([]string{"wardynd-test"}, args...)
	rg.t.Cleanup(func() { os.Args = oldArgs })
	return parseBootFlags()
}

type rehearsal struct {
	out, errOut string
	err         error
	report      rehearseReport
}

func (rg *rehearseRig) run(ageKey string) rehearsal {
	rg.t.Helper()
	return rg.runWith(rg.flags(ageKey), rg.set, rg.targetDSN)
}

func (rg *rehearseRig) runWith(f *bootFlags, set rehearseSet, targetDSN string) rehearsal {
	rg.t.Helper()
	var out, errOut bytes.Buffer
	r := rehearsal{}
	r.err = rehearse(context.Background(), f, set, targetDSN, &out, &errOut)
	r.out, r.errOut = out.String(), errOut.String()
	_ = json.Unmarshal(out.Bytes(), &r.report)
	return r
}

func (r rehearsal) check(name string) (rehearseCheck, bool) {
	for _, c := range r.report.Checks {
		if c.Name == name {
			return c, true
		}
	}
	return rehearseCheck{}, false
}

// auditRows are the recovery.rehearse* rows on live, in order.
func (rg *rehearseRig) auditRows() []string {
	rows, err := rg.livePool.Query(context.Background(), `SELECT action || ' ' || outcome || ' ' || data::text FROM audit_events WHERE action LIKE 'recovery.rehearse%' ORDER BY seq`)
	if err != nil {
		rg.t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			rg.t.Fatal(err)
		}
		out = append(out, s)
	}
	return out
}

func TestRehearse_RightKeyPassesAndLeavesTheTargetUnchanged(t *testing.T) {
	rg := newRehearseRig(t)
	before := snapshot(t, rg.targetDSN)

	r := rg.run(rg.id.String())
	if r.err != nil {
		t.Fatalf("rehearse = %v\n%s", r.err, r.out)
	}
	if r.report.Verdict != resultPass || !r.report.VerdictRecorded {
		t.Fatalf("report = %+v; want pass with the verdict recorded", r.report)
	}
	use, _ := r.check("key use")
	if use.Counts["opened"] != 2 || use.Counts["converts_on_first_boot"] != 1 || use.Counts["external"] != 1 {
		t.Errorf("key use counts = %v; want 2 opened, 1 converts_on_first_boot, 1 external", use.Counts)
	}
	if after := snapshot(t, rg.targetDSN); after != before {
		t.Errorf("the target changed during a rehearsal:\nbefore %s\nafter  %s", before, after)
	}
	rows := rg.auditRows()
	if len(rows) != 2 || !strings.HasPrefix(rows[0], actionRehearseBeg+" success") || !strings.HasPrefix(rows[1], actionRehearseDone+" success") {
		t.Fatalf("audit rows on live = %v; want the begin row then the verdict row", rows)
	}
	sink, err := os.ReadFile(rg.sinkPath)
	if err != nil || !strings.Contains(string(sink), actionRehearseBeg) || !strings.Contains(string(sink), actionRehearseDone) {
		t.Errorf("the sink never saw both rows (err %v): %s", err, sink)
	}
}

// snapshot is a digest of everything a rehearsal could write on the target: every secrets row's
// bytes, the principal keys and the audit head.
func snapshot(t *testing.T, dsn string) string {
	t.Helper()
	pool, err := db.Connect(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	var s string
	err = pool.QueryRow(context.Background(), `SELECT
		(SELECT count(*) || ':' || coalesce(md5(string_agg(owned_by || name || enc_version || kek_id || encode(wrapped_dek,'hex') || encode(ciphertext,'hex'), ',' ORDER BY owned_by, name)), '') FROM secrets) || '/' ||
		(SELECT count(*) FROM principal_keys) || '/' ||
		(SELECT coalesce(max(seq), 0) FROM audit_events) || '/' || (SELECT count(*) FROM audit_events)`).Scan(&s)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestRehearse_AWrongAgeKeyFailsNamingTheClass(t *testing.T) {
	rg := newRehearseRig(t)
	r := rg.run(mustAgeIdentity(t).String())
	requireExit(t, r.err, exitRehearseFail)
	if r.report.Verdict != resultFail {
		t.Fatalf("verdict = %q, want fail", r.report.Verdict)
	}
	if len(r.report.Checks) == 0 || r.report.Checks[0].Name != "key fingerprint" || r.report.Checks[0].Result != resultFail {
		t.Errorf("the fingerprint mismatch must be reported first: %+v", r.report.Checks)
	}
	use, ok := r.check("key use")
	if !ok || use.Result != resultFail || !strings.Contains(strings.Join(use.Failing, "\n"), "local/cred:") {
		t.Errorf("key use = %+v; want a failure naming the local/cred class", use)
	}
	rows := strings.Join(rg.auditRows(), "\n")
	if strings.Contains(rows, "alice@corp.example") || strings.Contains(rows, "anthropic-api-key") || !strings.Contains(rows, `"failing": `) {
		t.Errorf("the verdict row must count failing rows and name none of them: %s", rows)
	}
}

func TestRehearse_NothingIsDecryptedBeforeTheBeginRowIsWritten(t *testing.T) {
	rg := newRehearseRig(t)
	if _, err := rg.livePool.Exec(context.Background(), `CREATE FUNCTION aaa_reject() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'rejected'; END $$`); err != nil {
		t.Fatal(err)
	}
	if _, err := rg.livePool.Exec(context.Background(), `CREATE TRIGGER aaa_reject BEFORE INSERT ON audit_events FOR EACH ROW WHEN (NEW.action = 'recovery.rehearse.begin') EXECUTE FUNCTION aaa_reject()`); err != nil {
		t.Fatal(err)
	}
	r := rg.run(rg.id.String())
	requireExit(t, r.err, exitMigrateRefused, "begin row")
	if _, ran := r.check("key use"); ran {
		t.Errorf("key use ran although the begin row could not be written: %s", r.out)
	}
	if rows := rg.auditRows(); len(rows) != 0 {
		t.Errorf("rows on live = %v after a refused run", rows)
	}
}

func TestRehearse_AFailedVerdictRowKeepsTheVerdictsExitCode(t *testing.T) {
	rg := newRehearseRig(t)
	if _, err := rg.livePool.Exec(context.Background(), `CREATE FUNCTION aaa_reject() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'rejected'; END $$`); err != nil {
		t.Fatal(err)
	}
	if _, err := rg.livePool.Exec(context.Background(), `CREATE TRIGGER aaa_reject BEFORE INSERT ON audit_events FOR EACH ROW WHEN (NEW.action = 'recovery.rehearse') EXECUTE FUNCTION aaa_reject()`); err != nil {
		t.Fatal(err)
	}
	r := rg.run(mustAgeIdentity(t).String())
	requireExit(t, r.err, exitRehearseFail)
	if r.report.VerdictRecorded || !strings.Contains(r.errOut, "verdict row") {
		t.Errorf("verdict_recorded = %v, stderr = %q; want false and the failure named", r.report.VerdictRecorded, r.errOut)
	}
	if rows := rg.auditRows(); len(rows) != 1 || !strings.HasPrefix(rows[0], actionRehearseBeg) {
		t.Errorf("rows on live = %v; want only the begin row", rows)
	}
}

func TestRehearse_APairingMismatchRefusesNamingTheAuditHeadAndWritesNothing(t *testing.T) {
	rg := newRehearseRig(t)
	rg.set.HeadSeq++
	r := rg.run(rg.id.String())
	requireExit(t, r.err, exitMigrateRefused, "audit head")
	if rows := rg.auditRows(); len(rows) != 0 {
		t.Errorf("a refused pairing wrote %v", rows)
	}
}

func TestRehearse_AChainBreakFailsWithBrokenSeq(t *testing.T) {
	rg := newRehearseRig(t)
	ctx := context.Background()
	pool, err := db.Connect(ctx, rg.targetDSN)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `SET LOCAL session_replication_role = replica`); err != nil {
		t.Fatal(err)
	}
	var seq int64
	if err := tx.QueryRow(ctx, `UPDATE audit_events SET actor = 'edited' WHERE seq = (SELECT min(seq) + 1 FROM audit_events) RETURNING seq`).Scan(&seq); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	pool.Close()
	r := rg.run(rg.id.String())
	requireExit(t, r.err, exitRehearseFail)
	chain, ok := r.check("audit chain")
	if !ok || chain.Result != resultFail || chain.BrokenSeq != seq {
		t.Errorf("audit chain = %+v; want fail with broken_seq %d", chain, seq)
	}
}

func TestRehearse_OnePostgresBackendOnTheTargetThroughoutARun(t *testing.T) {
	rg := newRehearseRig(t)
	f := rg.flags(rg.id.String())
	ctx, cancel := context.WithCancel(context.Background())
	var mu sync.Mutex
	most, seen := 0, 0
	done := make(chan struct{})
	go func() {
		defer close(done)
		for ctx.Err() == nil {
			var n int
			if err := rg.livePool.QueryRow(context.Background(), `SELECT count(*) FROM pg_stat_activity WHERE datname = $1 AND backend_type = 'client backend'`, rg.targetDBName()).Scan(&n); err == nil {
				mu.Lock()
				most, seen = max(most, n), seen+1
				mu.Unlock()
			}
			time.Sleep(time.Millisecond)
		}
	}()
	r := rg.runWith(f, rg.set, rg.targetDSN)
	cancel()
	<-done
	if r.err != nil {
		t.Fatalf("rehearse = %v", r.err)
	}
	mu.Lock()
	defer mu.Unlock()
	if seen < 10 || most != 1 {
		t.Fatalf("pg_stat_activity showed at most %d backends on the target over %d samples; want exactly 1", most, seen)
	}
}

func (rg *rehearseRig) targetDBName() string {
	u, err := url.Parse(rg.targetDSN)
	if err != nil {
		rg.t.Fatal(err)
	}
	return strings.TrimPrefix(u.Path, "/")
}

// TestRehearse_NoCredentialValueInAnyOutput searches everything a rehearsal emits for the canaries,
// on the pass, the fail and a DSN-embedding error path.
func TestRehearse_NoCredentialValueInAnyOutput(t *testing.T) {
	rg := newRehearseRig(t)
	wrong := mustAgeIdentity(t)
	runs := map[string]rehearsal{"pass": rg.run(rg.id.String()), "fail": rg.run(wrong.String())}

	bad := strings.Replace(rg.targetDSN, "/"+rg.targetDBName(), ":notaport/"+rg.targetDBName(), 1)
	runs["unparsable target"] = rg.runWith(rg.flags(rg.id.String()), rg.set, bad)

	badLive := rg.flags(rg.id.String())
	*badLive.dsn = "postgres://" + rg.liveRole + ":" + canaryLivePW + "@127.0.0.1:1/x?sslmode=disable&connect_timeout=2"
	runs["unreachable live"] = rg.runWith(badLive, rg.set, rg.targetDSN)

	canaries := []string{rg.id.String(), wrong.String(), canaryValue, canaryLegacy, canaryLivePW, canaryTargetPW, canaryToken}
	sink, _ := os.ReadFile(rg.sinkPath)
	var audit []string
	audit = append(audit, rg.auditRows()...)
	audit = append(audit, string(sink), rg.set.Dir)
	for name, r := range runs {
		texts := append([]string{r.out, r.errOut}, audit...)
		if r.err != nil {
			texts = append(texts, r.err.Error())
		}
		for _, text := range texts {
			for _, c := range canaries {
				if strings.Contains(text, c) {
					t.Errorf("run %q: canary %q appears in its output: %.300s", name, c, text)
				}
			}
		}
	}
	if r := runs["unparsable target"]; r.err == nil {
		t.Error("an unparsable target DSN must fail the run")
	}
	entries, _ := os.ReadDir(rg.set.Dir)
	if len(entries) != 0 {
		t.Errorf("the set directory gained %d entries", len(entries))
	}
}

func TestRehearse_UnknownKeyClassCountsFail(t *testing.T) {
	rg := newRehearseRig(t)
	rg.set.KeyClasses = map[string]int{"local/cred:other": 1}
	r := rg.run(rg.id.String())
	requireExit(t, r.err, exitRehearseFail)
	use, _ := r.check("key use")
	if !strings.Contains(strings.Join(use.Failing, "\n"), "key class") {
		t.Errorf("failing = %v; want the class named", use.Failing)
	}
}

func TestRehearse_ARowCountDifferingFromTheManifestFails(t *testing.T) {
	rg := newRehearseRig(t)
	classes := map[string]int{}
	for k, v := range rg.set.KeyClasses {
		classes[k] = v
	}
	for k := range classes {
		classes[k]++ // a row the manifest counted is missing from the target
		break
	}
	rg.set.KeyClasses = classes
	r := rg.run(rg.id.String())
	requireExit(t, r.err, exitRehearseFail)
	use, _ := r.check("key use")
	if !strings.Contains(strings.Join(use.Failing, "\n"), "in the target") {
		t.Errorf("failing = %v; want the class count mismatch named", use.Failing)
	}
}

func TestRehearse_AnIncompleteCheckGivesExitFive(t *testing.T) {
	rep := &rehearseReport{}
	rep.add(rehearseCheck{Name: "a", Result: resultPass})
	rep.add(rehearseCheck{Name: "drives", Result: resultIncomplete})
	if v, code := rep.verdictOf(); v != resultIncomplete || code != exitRehearseIncomplete {
		t.Errorf("verdict = %q, code %d", v, code)
	}
	rep.add(rehearseCheck{Name: "b", Result: resultFail})
	if v, code := rep.verdictOf(); v != resultFail || code != exitRehearseFail {
		t.Errorf("a failure outranks an incomplete: %q, %d", v, code)
	}
}
