// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// The shared masking registry (ha-l2.1) through the API's own doors, against a
// real Postgres: two Servers over one database are two wardynds, each with its
// own registry cache. Guarded by WARDYN_TEST_PG (throwawayPGPool); skipped
// cleanly when unset.

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/identity"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/subjectkey"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A value registered on server A is masked in a recording uploaded to server B,
// and after a dispatch each rendering is in exactly one table: the run's
// dispatch-time values in its manifest, the post-dispatch injection values in
// the shared registry.
func TestMaskRegistry_ARegistrationOnAMasksAnUploadToB(t *testing.T) {
	l := newMaskLab(t)
	a, b := l.replica(), l.replica()
	run := l.run()
	a.dispatch(t, run, "dispatch-time-value-1")

	perRun := func() int {
		return l.count(`SELECT count(*) FROM mask_values WHERE run_id=$1 AND NOT tombstone`, run.ID)
	}
	manifest := func() int { return l.count(`SELECT count(*) FROM run_mask_values WHERE run_id=$1`, run.ID) }
	if perRun() != 0 || manifest() != 1 {
		t.Fatalf("after dispatch: %d per_run rows and %d manifest values, want 0 and 1", perRun(), manifest())
	}

	// What the injection routes do, after dispatch, on server A.
	if err := a.srv.maskInjected(run.ID, []byte("injected-after-dispatch-2")); err != nil {
		t.Fatal(err)
	}
	if err := a.reg.AddGlobalUntil(maskOwner, "aws-sso", time.Now(), time.Now().Add(time.Hour), []byte("global-sso-token-3"), []byte("global-sso-refresh-4")); err != nil {
		t.Fatal(err)
	}
	if perRun() != 1 || manifest() != 1 {
		t.Fatalf("after the injection: %d per_run rows and %d manifest values, want 1 and 1", perRun(), manifest())
	}

	body := "out: dispatch-time-value-1 injected-after-dispatch-2 global-sso-token-3 global-sso-refresh-4\n"
	if w := l.upload(b, run, body); w.Code != http.StatusNoContent {
		t.Fatalf("upload to B = %d: %s", w.Code, w.Body.String())
	}
	saved := string(b.rec.saved)
	for _, v := range []string{"dispatch-time-value-1", "injected-after-dispatch-2", "global-sso-token-3", "global-sso-refresh-4"} {
		if strings.Contains(saved, v) {
			t.Errorf("B persisted %q unmasked: %q", v, saved)
		}
	}
	if !strings.Contains(saved, "<secret-hidden>") {
		t.Errorf("B masked nothing: %q", saved)
	}
}

// A failed registration fails the injection: nothing is handed out, and the
// value is not half-registered.
func TestMaskRegistry_AFailedRegistrationFailsTheInjection(t *testing.T) {
	l := newMaskLab(t)
	down := newLabPool(t, l)
	a := l.replicaMasking(down)
	run := l.run()
	a.dispatch(t, run, "dispatch-time-value-1")
	down.Close()

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/api/v1/internal/injections/x", nil)
	claims := &identity.Claims{RunID: run.ID, SPIFFEID: run.SPIFFEID, Sub: maskOwner}
	if !a.srv.refuseUnmasked(w, r, claims, "injection.resolve", []byte("a-credential-about-to-be-handed-out")) {
		t.Fatal("a credential that could not be recorded was handed out")
	}
	wantMaskRefusal(t, w, "an injection whose value could not be recorded")
	if strings.Contains(string(a.reg.Masker(run.ID).Mask([]byte("a-credential-about-to-be-handed-out"))), "<secret-hidden>") {
		t.Error("a value whose registration failed is in the registry")
	}
}

// With Postgres down for masking, an upload answers 503, a new attach is
// refused, and a live chunk is the placeholder rather than bytes the server
// cannot vouch for.
func TestMaskRegistry_PostgresDownFailsClosed(t *testing.T) {
	l := newMaskLab(t)
	a := l.replica()
	down := newLabPool(t, l)
	b := l.replicaMasking(down)
	run := l.run()
	a.dispatch(t, run, "value-before-the-outage")
	if !b.srv.maskCovered(t.Context(), run.ID) {
		t.Fatal("B does not cover a dispatched run while Postgres is up")
	}
	var live bytes.Buffer
	mw := &liveMaskWriter{reg: b.reg, runID: run.ID, dst: &live}
	if _, err := mw.Write([]byte("healthy chunk value-before-the-outage\n")); err != nil {
		t.Fatal(err)
	}
	if got := live.String(); strings.Contains(got, "value-before-the-outage") || !strings.Contains(got, "<secret-hidden>") {
		t.Fatalf("a healthy chunk = %q, want the value masked", got)
	}

	down.Close()

	if w := l.upload(b, run, "anything value-before-the-outage\n"); w.Code != http.StatusServiceUnavailable {
		t.Errorf("upload with Postgres down = %d, want 503: %s", w.Code, w.Body.String())
	} else {
		wantMaskRefusal(t, w, "upload with Postgres down")
	}
	if len(b.rec.saved) != 0 {
		t.Errorf("an upload with Postgres down reached the store: %q", b.rec.saved)
	}

	ts := httptest.NewServer(panicFails(t, b.srv.Handler()))
	defer ts.Close()
	tok, err := mintAttachTicket(t.Context(), b.srv.cfg.Store, run.ID, types.ActorHuman, maskOwner, oidc.RoleAdmin, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	_, resp, derr := websocket.Dial(t.Context(), "ws"+strings.TrimPrefix(ts.URL, "http")+"/api/v1/runs/"+run.ID.String()+"/attach?ticket="+tok, nil)
	if derr == nil || resp == nil || resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("a new attach with Postgres down: err=%v resp=%v, want 503", derr, resp)
	}

	live.Reset()
	if _, err := mw.Write([]byte("live chunk value-before-the-outage\n")); err != nil {
		t.Fatal(err)
	}
	if got := live.String(); got != "<secret-hidden>" {
		t.Errorf("a live chunk with Postgres down = %q, want only the placeholder", got)
	}
	if !mw.capture.dropped || !mw.capture.uncovered {
		t.Errorf("the writer did not record that a chunk was replaced: %+v", mw.capture)
	}
}

// A person's AWS SSO and Azure DevOps refresh tokens, registered as globals on
// A, cannot be decrypted after the person's erasure; purge on B with values
// registered on A, restart both, and no live table holds a value of theirs, and
// nothing that runs afterwards writes one back.
func TestMaskRegistry_ErasureLeavesNothingToDecryptAndNothingRecreatesIt(t *testing.T) {
	l := newMaskLab(t)
	fail := false
	a, b := erasureReplica(l, &fail), erasureReplica(l, &fail)
	ctx := t.Context()
	const alice, bob = "alice-sub", "bob-sub"
	cred := subjectkey.PurposeCred
	awsName, adoName := harnessCredSecretName(awsSSOProvider), adoEntraSecretName("row-1")

	run := l.personRun(alice, "alice plan")
	a.dispatch(t, run, "alice-dispatch-value-0")
	for _, g := range []struct{ owner, name, access, refresh string }{
		{alice, awsName, "alice-aws-access-token", "alice-aws-refresh-token"},
		{alice, adoName, "alice-ado-access-token", "alice-ado-refresh-token"},
		{bob, awsName, "bobs-aws-access-token-0", "bobs-aws-refresh-token-0"},
	} {
		if err := a.reg.AddGlobalUntil(g.owner, g.name, time.Now(), time.Now().Add(time.Hour), []byte(g.access), []byte(g.refresh)); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.reg.Add(run.ID, []byte("alice-injected-value-1")); err != nil {
		t.Fatal(err)
	}
	if err := b.st.Fresh(ctx, time.Now()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b.reg.Masker(run.ID).Mask([]byte("alice-ado-refresh-token"))), "<secret-hidden>") {
		t.Fatal("B does not mask A's registration before the erasure")
	}

	// Ciphertext as it was before the erasure, to try to open afterwards.
	type sealedRow struct {
		id      uuid.UUID
		version int
	}
	var held []sealedRow
	rows, err := l.pool.Query(ctx, `SELECT id, key_version FROM mask_values WHERE owner=$1 AND NOT tombstone`, alice)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var r sealedRow
		if err := rows.Scan(&r.id, &r.version); err != nil {
			t.Fatal(err)
		}
		held = append(held, r)
	}
	rows.Close()
	if len(held) != 5 {
		t.Fatalf("alice has %d live masking rows, want 5 (two credentials with two values each, one per-run value)", len(held))
	}

	sec := ssoSession(t, "sec-sub", "sec@corp.example", oidc.RoleSecurityAdmin)
	if w := doSSO(t, b.srv, http.MethodPost, "/api/v1/people/"+alice+"/erasure", sec,
		erasureBody("mask_copies", "run_outputs", "run_tasks", "audit_personal_fields", "credentials")); w.Code != http.StatusOK {
		t.Fatalf("erasure = %d %s", w.Code, w.Body)
	}

	if n := l.count(`SELECT count(*) FROM mask_values WHERE owner=$1 AND NOT tombstone`, alice); n != 0 {
		t.Errorf("%d live mask_values rows for alice after the erasure", n)
	}
	if n := l.count(`SELECT count(*) FROM mask_values WHERE tombstone AND (sealed IS NOT NULL OR owner <> '' OR name <> '')`); n != 0 {
		t.Errorf("%d tombstones still hold ciphertext or an owner", n)
	}
	if n := l.count(`SELECT count(*) FROM run_mask_values WHERE owner=$1`, alice); n != 0 {
		t.Errorf("%d manifest values for alice after the erasure", n)
	}
	for _, r := range held {
		if _, err := a.sec.SubjectKeys().Key(ctx, alice, cred, r.version); !errors.Is(err, subjectkey.ErrDataLoss) {
			t.Errorf("the key that sealed a row of alice's is still readable (%v)", err)
		}
	}
	if n := l.count(`SELECT count(*) FROM mask_values WHERE owner=$1 AND NOT tombstone`, bob); n != 2 {
		t.Errorf("bob has %d masking rows, want his two untouched", n)
	}

	// Restart both, and run everything that runs afterwards: a reconcile on boot,
	// the secret sweeps, a read of the registry. Nothing writes alice's values back.
	a2, b2 := erasureReplica(l, &fail), erasureReplica(l, &fail)
	for name, rp := range map[string]replica{"A": a, "B": b, "A restarted": a2, "B restarted": b2} {
		if err := rp.srv.ReconcileOnBoot(ctx); err != nil {
			t.Fatalf("%s: ReconcileOnBoot: %v", name, err)
		}
		if _, err := rp.srv.SweepRunSecrets(ctx); err != nil {
			t.Fatalf("%s: SweepRunSecrets: %v", name, err)
		}
		if err := rp.st.Fresh(ctx, time.Now()); err != nil {
			t.Fatalf("%s: read: %v", name, err)
		}
		for _, v := range []string{"alice-aws-refresh-token", "alice-ado-refresh-token", "alice-injected-value-1", "alice-dispatch-value-0"} {
			if strings.Contains(string(rp.reg.Masker(run.ID).Mask([]byte(v))), "<secret-hidden>") {
				t.Errorf("%s still masks %q after the erasure", name, v)
			}
		}
		if !strings.Contains(string(rp.reg.Masker(uuid.New()).Mask([]byte("bobs-aws-refresh-token-0"))), "<secret-hidden>") {
			t.Errorf("%s lost bob's masked value", name)
		}
	}
	if n := l.count(`SELECT count(*) FROM mask_values WHERE owner=$1 AND NOT tombstone`, alice) +
		l.count(`SELECT count(*) FROM run_mask_values WHERE owner=$1`, alice); n != 0 {
		t.Errorf("%d rows of alice's exist after the restarts and sweeps", n)
	}

	// A person who signs in again is registered under a NEW key generation: a
	// destroyed one is never revived to hold a value.
	if err := a2.reg.AddGlobal(alice, awsName, time.Now(), []byte("alice-aws-token-after-signing-in-again")); err != nil {
		t.Fatal(err)
	}
	var v int
	if err := l.pool.QueryRow(ctx, `SELECT key_version FROM mask_values WHERE owner=$1 AND NOT tombstone`, alice).Scan(&v); err != nil || v <= held[0].version {
		t.Errorf("a re-registration sealed under key version %d (err %v), want a version after %d", v, err, held[0].version)
	}
}

// A follower's live writer never forwards an erased person's value, not even in
// the beat after the erasure: the read that applies the erasure's tombstones drops
// the value from the corpus, and the guard it consults for that same chunk must
// already see the fence rather than answer from before it.
func TestMaskRegistry_AFollowersLiveWriterDropsTheChunkRightAfterAnErasure(t *testing.T) {
	l := newMaskLab(t)
	fail := false
	a, b := erasureReplica(l, &fail), erasureReplica(l, &fail)
	b.srv.maskBeat = time.Hour // the guard answers from its cache unless the registry moved
	ctx := t.Context()
	const alice = "alice-sub"
	const injected = "alice-injected-value-1"

	run := l.personRun(alice, "alice plan")
	a.dispatch(t, run, "alice-dispatch-value-0")
	if err := a.reg.Add(run.ID, []byte(injected)); err != nil {
		t.Fatal(err)
	}
	if err := b.st.Fresh(ctx, time.Now()); err != nil {
		t.Fatal(err)
	}
	var live bytes.Buffer
	mw := &liveMaskWriter{reg: b.reg, runID: run.ID, dst: &live, guard: b.srv.maskGuard(run.ID)}
	if _, err := mw.Write([]byte("before " + injected + "\n")); err != nil {
		t.Fatal(err)
	}
	if got := live.String(); strings.Contains(got, injected) || !strings.Contains(got, "<secret-hidden>") {
		t.Fatalf("a chunk before the erasure = %q, want the value masked", got)
	}

	sec := ssoSession(t, "sec-sub", "sec@corp.example", oidc.RoleSecurityAdmin)
	if w := doSSO(t, a.srv, http.MethodPost, "/api/v1/people/"+alice+"/erasure", sec,
		erasureBody("mask_copies", "run_outputs", "run_tasks", "audit_personal_fields", "credentials")); w.Code != http.StatusOK {
		t.Fatalf("erasure = %d %s", w.Code, w.Body)
	}

	live.Reset()
	if _, err := mw.Write([]byte("after " + injected + "\n")); err != nil {
		t.Fatal(err)
	}
	if got := live.String(); strings.Contains(got, injected) {
		t.Errorf("a chunk right after the erasure reached the viewer with the value in clear: %q", got)
	}
	if !mw.capture.dropped {
		t.Errorf("the writer did not record that the chunk was dropped: %+v", mw.capture)
	}
}

// staticLease is a SweeperLease with a fixed answer.
type staticLease struct{ leader bool }

func (s staticLease) Join() (context.Context, int64, func(), bool) {
	return context.Background(), 1, func() {}, s.leader
}
func (staticLease) Current(context.Context, int64) (bool, error) { return true, nil }

// Past RunSecretGrace the leader deletes a terminal run's per-run rows and its
// manifest, and every replica drops its cache when it reads the tombstone; a
// follower deletes nothing.
func TestMaskRegistry_TheLeaderDeletesATerminalRunsRowsPastTheGrace(t *testing.T) {
	l := newMaskLab(t)
	leader, follower := l.replica(), l.replica()
	leader.srv.cfg.SweeperLease = staticLease{leader: true}
	follower.srv.cfg.SweeperLease = staticLease{leader: false}
	ctx := t.Context()
	cold, warm := l.run(), l.run()
	for _, run := range []types.AgentRun{cold, warm} {
		leader.dispatch(t, run, "dispatch-value-"+run.ID.String())
		if err := leader.srv.maskInjected(run.ID, []byte("injected-value-"+run.ID.String())); err != nil {
			t.Fatal(err)
		}
	}
	if err := follower.st.Fresh(ctx, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := l.pool.Exec(ctx, `UPDATE agent_runs SET state='STOPPED', updated_at = now() - make_interval(secs => $2) WHERE id=$1`, cold.ID, (2 * RunSecretGrace).Seconds()); err != nil {
		t.Fatal(err)
	}
	rows := func(run types.AgentRun) (int, int) {
		return l.count(`SELECT count(*) FROM mask_values WHERE run_id=$1 AND NOT tombstone`, run.ID),
			l.count(`SELECT count(*) FROM run_mask_manifest WHERE run_id=$1`, run.ID)
	}

	if _, err := follower.srv.SweepRunSecrets(ctx); err != nil {
		t.Fatal(err)
	}
	if v, m := rows(cold); v != 1 || m != 1 {
		t.Fatalf("a follower's sweep left %d per_run rows and %d manifests for the cold run, want its rows untouched", v, m)
	}

	if _, err := leader.srv.SweepRunSecrets(ctx); err != nil {
		t.Fatal(err)
	}
	if v, m := rows(cold); v != 0 || m != 0 {
		t.Errorf("after the leader's sweep the cold run has %d per_run rows and %d manifests, want none", v, m)
	}
	if v, m := rows(warm); v != 1 || m != 1 {
		t.Errorf("the leader's sweep touched a run that is not cold: %d per_run rows, %d manifests", v, m)
	}
	if n := l.count(`SELECT count(*) FROM mask_values WHERE tombstone AND sealed IS NOT NULL`); n != 0 {
		t.Errorf("%d tombstones hold ciphertext", n)
	}

	// The follower reads the tombstone and drops the cold run's cache.
	if err := follower.st.Fresh(ctx, time.Now()); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(follower.reg.Masker(cold.ID).Mask([]byte("injected-value-"+cold.ID.String()))), "<secret-hidden>") {
		t.Error("the follower still masks the swept run's value after reading the tombstone")
	}
	if !strings.Contains(string(follower.reg.Masker(warm.ID).Mask([]byte("injected-value-"+warm.ID.String()))), "<secret-hidden>") {
		t.Error("the follower dropped a live run's value")
	}
}

// newLabPool is a second pool on the lab's database that a test can close to take
// Postgres away from whatever is built over it.
func newLabPool(t *testing.T, l *maskLab) *pgxpool.Pool {
	t.Helper()
	p, err := pgxpool.NewWithConfig(t.Context(), l.pool.Config())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	return p
}
