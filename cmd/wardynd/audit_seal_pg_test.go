// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

// WARDYN_AUDIT_SEAL through the real audit chain, a real Postgres and the real
// spool: a sealed field reaches the store, the spool and a sink only as
// ciphertext; a cold key lookup waits in the spool under the pending key and is
// re-sealed by the drain; and erasing the person makes it read erased on a
// replica whose key cache was warm while the chain still verifies. Guarded by
// WARDYN_TEST_PG (subjectkeytest.ThrowawayDB); skipped cleanly when unset.

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/api"
	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/kek"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/subjectkey"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/subjectkey/subjectkeytest"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// flakyKeys is the subject keys with an outage switch.
type flakyKeys struct {
	audit.SealKeys
	down atomic.Bool
}

func (f *flakyKeys) Current(ctx context.Context, owner, purpose string) (int, []byte, error) {
	if f.down.Load() {
		return 0, nil, context.DeadlineExceeded
	}
	return f.SealKeys.Current(ctx, owner, purpose)
}

type sealRig struct {
	t         *testing.T
	pool      *pgxpool.Pool
	pg        store.PG
	flaky     *flakyKeys
	rec       audit.Recorder
	drain     audit.Recorder
	spool     *api.AuditSpool
	spoolPath string
	sinkPath  string
	// a and b are two wardynds over the database: each its own key cache.
	a, b http.Handler
}

const sealRigToken = "seal-rig-admin-token"

func newSealRig(t *testing.T) *sealRig {
	t.Helper()
	pool := subjectkeytest.ThrowawayDB(t)
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	newKeys := func() *subjectkey.Manager {
		k, err := kek.NewLocalPurpose(id, kek.PurposeCred)
		if err != nil {
			t.Fatal(err)
		}
		return subjectkeytest.Manager(pool, k)
	}
	pending := make([]byte, 32)
	_, _ = rand.Read(pending)
	dir := t.TempDir()
	rg := &sealRig{t: t, pool: pool, pg: store.NewPG(pool), spoolPath: filepath.Join(dir, "spool.jsonl"), sinkPath: filepath.Join(dir, "sink.jsonl")}

	keysA := newKeys()
	rg.flaky = &flakyKeys{SealKeys: subjectSealKeys{keysA}}
	sealer := newAuditSealer(rg.flaky, pool, pending)
	src := newAuditSealSource(audit.SealFields)
	src.arm(sealer)
	sinks, _ := json.Marshal(map[string]any{"file": map[string]any{"path": rg.sinkPath}})
	rec, f, spool, drain, err := buildAuditChain(t.Context(), string(sinks), rg.spoolPath, "", pool, secretmask.NewRegistry(), src)
	if err != nil {
		t.Fatal(err)
	}
	if f != nil {
		t.Cleanup(func() { _ = f.Close() })
	}
	rg.rec, rg.spool, rg.drain = rec, spool, drain

	serve := func(keys *subjectkey.Manager, unseal *audit.Sealer) http.Handler {
		return api.New(api.Config{
			Store: rg.pg, AdminToken: sealRigToken, Audit: rec, TrustDomain: "wardyn.local",
			AuditUnsealer: unseal, SubjectKeys: keys, BaseCtx: t.Context(),
		}).Handler()
	}
	keysB := newKeys()
	rg.a = serve(keysA, sealer)
	rg.b = serve(keysB, newAuditSealer(subjectSealKeys{keysB}, pool, pending))
	return rg
}

func (rg *sealRig) call(h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	rg.t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
	}
	r.Header.Set("Authorization", "Bearer "+sealRigToken)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func (rg *sealRig) decide(actor, reason string) {
	rg.t.Helper()
	data, _ := json.Marshal(map[string]any{"approval_id": "ap-" + actor, "decision": "APPROVED", "reason": reason})
	err := rg.rec.Record(rg.t.Context(), types.AuditEvent{ID: uuid.New(), Time: time.Now().UTC(), ActorType: types.ActorHuman,
		Actor: actor, Action: "approval.decide", Target: "ap-" + actor, Outcome: "success", Data: data})
	if err != nil {
		rg.t.Fatalf("record %s's decision: %v", actor, err)
	}
}

// reasons is every approval.decide reason a reader sees through h, by actor.
func (rg *sealRig) reasons(h http.Handler) map[string]string {
	rg.t.Helper()
	w := rg.call(h, http.MethodGet, "/api/v1/audit?action=approval.decide", "")
	if w.Code != http.StatusOK {
		rg.t.Fatalf("audit read = %d %s", w.Code, w.Body)
	}
	var evs []types.AuditEvent
	if err := json.Unmarshal(w.Body.Bytes(), &evs); err != nil {
		rg.t.Fatal(err)
	}
	out := map[string]string{}
	for _, ev := range evs {
		var d map[string]any
		_ = json.Unmarshal(ev.Data, &d)
		out[ev.Actor], _ = d["reason"].(string)
	}
	return out
}

// stored is the audit rows' data as Postgres holds it.
func (rg *sealRig) stored() string {
	rg.t.Helper()
	var all string
	if err := rg.pool.QueryRow(rg.t.Context(), `SELECT COALESCE(string_agg(data::text, E'\n'), '') FROM audit_events WHERE action='approval.decide'`).Scan(&all); err != nil {
		rg.t.Fatal(err)
	}
	return all
}

func readFileOrEmpty(path string) string {
	b, _ := os.ReadFile(path)
	return string(b)
}

func (rg *sealRig) noPlaintext(where string, words ...string) {
	rg.t.Helper()
	for name, content := range map[string]string{"the store": rg.stored(), "the sink": readFileOrEmpty(rg.sinkPath), "the spool": readFileOrEmpty(rg.spoolPath)} {
		if where != "" && name != where {
			continue
		}
		for _, w := range words {
			if strings.Contains(content, w) {
				rg.t.Errorf("%s holds %q in the clear", name, w)
			}
		}
	}
}

func TestPG_AuditSeal_FieldsAreCiphertextEverywhereAndErasureShredsThem(t *testing.T) {
	rg := newSealRig(t)
	ctx := t.Context()

	rg.decide("alice", "alice typed these words")
	rg.decide("bob", "bob typed other words")
	rg.noPlaintext("", "alice typed", "bob typed")
	if !strings.Contains(rg.stored(), "seal1.1.") || !strings.Contains(readFileOrEmpty(rg.sinkPath), "seal1.1.") {
		t.Fatalf("a sealed field is not in the store and the sink as ciphertext:\nstore %s\nsink %s", rg.stored(), readFileOrEmpty(rg.sinkPath))
	}
	// A refusal code is a machine value, never sealed (action- and path-aware).
	data, _ := json.Marshal(map[string]any{"reason": "superseded_by_new_login"})
	if err := rg.rec.Record(ctx, types.AuditEvent{ID: uuid.New(), Time: time.Now().UTC(), ActorType: types.ActorSystem, Actor: "wardynd", Action: "run.kill", Outcome: "success", Data: data}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(readFileOrEmpty(rg.sinkPath), "superseded_by_new_login") {
		t.Error("a machine reason on another action was sealed")
	}

	// Both wardynds open the fields; B's key cache is warm from here.
	for name, h := range map[string]http.Handler{"A": rg.a, "B": rg.b} {
		if got := rg.reasons(h); got["alice"] != "alice typed these words" || got["bob"] != "bob typed other words" {
			t.Fatalf("wardynd %s reads %v, want both opened", name, got)
		}
	}
	if st, err := rg.pg.VerifyAuditChain(ctx); err != nil || !st.OK {
		t.Fatalf("chain before the erasure = %+v, %v", st, err)
	}

	// Erase alice through A. B holds her key warm and must stop at its next read.
	w := rg.call(rg.a, http.MethodPost, "/api/v1/people/alice/erasure", `{"scopes":["audit_personal_fields"]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("erasure = %d %s", w.Code, w.Body)
	}
	for name, h := range map[string]http.Handler{"A": rg.a, "B": rg.b} {
		if got := rg.reasons(h); got["alice"] != audit.ErasedValue || got["bob"] != "bob typed other words" {
			t.Errorf("wardynd %s after the erasure reads %v, want alice erased and bob open", name, got)
		}
	}
	if st, err := rg.pg.VerifyAuditChain(ctx); err != nil || !st.OK {
		t.Fatalf("chain after a shred = %+v, %v; the row hash covers ciphertext, so verify must still pass", st, err)
	}
	// The readable export shows it erased too, the raw rows keep the ciphertext.
	exp := rg.call(rg.b, http.MethodGet, "/api/v1/audit/export?action=approval.decide", "")
	if exp.Code != http.StatusOK || !strings.Contains(exp.Body.String(), audit.ErasedValue) || strings.Contains(exp.Body.String(), "alice typed") {
		t.Errorf("export = %d %s, want alice's reason erased", exp.Code, exp.Body)
	}
	// The record of the erasure survives the key it destroyed: named in the clear.
	var target string
	if err := rg.pool.QueryRow(ctx, `SELECT target FROM audit_events WHERE action='principal_key.destroyed'`).Scan(&target); err != nil || target != "alice" {
		t.Errorf("principal_key.destroyed target = %q, %v, want alice", target, err)
	}
	rg.noPlaintext("", "alice typed", "bob typed")
}

func TestPG_AuditSeal_AColdKeyWaitsInTheSpoolThenTheDrainReseals(t *testing.T) {
	rg := newSealRig(t)
	ctx := t.Context()

	rg.decide("dave", "dave typed first")        // dave's key exists from here
	rg.flaky.down.Store(true)                    // the key store goes away
	rg.decide("carol", "carol typed while down") // written, not dropped, not in the clear
	rg.decide("dave", "dave typed while down")
	if got := rg.stored(); strings.Contains(got, "carol") || strings.Contains(got, "typed while down") {
		t.Fatalf("a pending row reached the store: %s", got)
	}
	spooled := readFileOrEmpty(rg.spoolPath)
	if strings.Count(spooled, "\n") != 2 || !strings.Contains(spooled, `"`+audit.PendingMarker+`":true`) {
		t.Fatalf("the spool = %q, want the two pending rows", spooled)
	}
	rg.noPlaintext("the spool", "typed while down")
	rg.noPlaintext("the sink", "typed while down")

	// Still down: the drain keeps the lines and never quarantines them, however often it tries.
	for range 6 {
		if _, err := rg.spool.Drain(ctx, rg.drain, 10); err == nil {
			t.Fatal("the drain re-sealed a pending row while the key store was down")
		}
	}
	if _, err := os.Stat(rg.spoolPath + ".quarantine"); err == nil {
		t.Fatal("a pending row was quarantined because the key store was down")
	}
	if got := readFileOrEmpty(rg.spoolPath); strings.Count(got, "\n") != 2 {
		t.Fatalf("the drain lost a pending row: %q", got)
	}

	// Dave is erased while his second row still waits.
	rg.flaky.down.Store(false)
	if w := rg.call(rg.b, http.MethodPost, "/api/v1/people/dave/erasure", `{"scopes":["audit_personal_fields"]}`); w.Code != http.StatusOK {
		t.Fatalf("erasure = %d %s", w.Code, w.Body)
	}

	// The store is back: the drain re-seals and stores.
	n, err := rg.spool.Drain(ctx, rg.drain, 10)
	if err != nil || n != 2 {
		t.Fatalf("drain = %d, %v, want both rows replayed", n, err)
	}
	if got := readFileOrEmpty(rg.spoolPath); strings.TrimSpace(got) != "" {
		t.Errorf("the spool still holds %q after the drain", got)
	}
	if got := rg.stored(); strings.Contains(got, "pending_subject") || strings.Contains(got, "seal1p.") || strings.Contains(got, "typed") {
		t.Fatalf("the store holds a pending or plaintext field: %s", got)
	}
	got := rg.reasons(rg.a)
	if got["carol"] != "carol typed while down" {
		t.Errorf("carol's re-sealed row reads %q, want it opened", got["carol"])
	}
	// Dave's first row was sealed before the erasure and his second waited past it:
	// both read erased, and no fresh key was minted to seal the second.
	if got["dave"] != audit.ErasedValue {
		t.Errorf("dave's rows read %q, want erased", got["dave"])
	}
	var gens int
	if err := rg.pool.QueryRow(ctx, `SELECT count(*) FROM principal_keys WHERE owner='dave' AND purpose='audit-seal'`).Scan(&gens); err != nil || gens != 1 {
		t.Errorf("dave has %d audit-seal generations (%v), want the one that was destroyed", gens, err)
	}

	// And after erasing carol, her re-sealed row reads erased too.
	if w := rg.call(rg.a, http.MethodPost, "/api/v1/people/carol/erasure", `{"scopes":["audit_personal_fields"]}`); w.Code != http.StatusOK {
		t.Fatalf("erasure = %d %s", w.Code, w.Body)
	}
	if got := rg.reasons(rg.b); got["carol"] != audit.ErasedValue {
		t.Errorf("carol after her erasure reads %q, want erased", got["carol"])
	}
	if st, err := rg.pg.VerifyAuditChain(ctx); err != nil || !st.OK {
		t.Fatalf("chain = %+v, %v, want ok", st, err)
	}
}
