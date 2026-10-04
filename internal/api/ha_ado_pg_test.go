// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// The Azure DevOps run token across two Servers over one database (ha-l2.4): one server's mint is
// the other's cache hit, two servers never mint one run's token twice, a pause's revoke and a
// resume on the other server leave nothing live on a paused run, and a person's erasure takes the
// token rows with it. Guarded by WARDYN_TEST_PG; skipped cleanly when unset.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/adorunpat"
	"github.com/cjohnstoniv/wardyn/internal/broker"
	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/identity"
	secretspg "github.com/cjohnstoniv/wardyn/internal/secretstore/pg"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// haADO is the Azure DevOps fixture with its run, its token state and its locks in one Postgres,
// and a second server over the same fixture.
type haADO struct {
	*adoPATFixture
	t    *testing.T
	pool *pgxpool.Pool
	a, b *Server
}

func newHAADO(t *testing.T) *haADO {
	t.Helper()
	fx := newADOPATLane(t)
	pool := throwawayPGPool(t)
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	sec, err := secretspg.New(pool, id)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if _, err := store.NewPG(pool).CreateRun(context.Background(), types.AgentRun{
		ID: fx.run.ID, CreatedAt: now, UpdatedAt: now, CreatedBy: fx.subject, OperatorOwned: true,
		Agent: "claude-code", Task: "ado token", ConfinementClass: types.CC2, State: types.RunRunning,
		SPIFFEID: "spiffe://wardyn.test/agent-run/" + fx.run.ID.String(), RunnerTarget: "docker",
	}); err != nil {
		t.Fatal(err)
	}
	locker := db.NewPGLocker(pool, 8)
	fx.srv.cfg.ADORunPATs = adorunpat.New(pool, sec.SubjectKeys())
	fx.srv.locks.override = locker
	b := New(fx.srv.cfg)
	b.adoPATs = fx.pats
	b.locks.override = locker
	return &haADO{adoPATFixture: fx, t: t, pool: pool, a: fx.srv, b: b}
}

// resolveOn is one proxy resolve for host, served by srv.
func (h *haADO) resolveOn(srv *Server, host string, q url.Values) (*httptest.ResponseRecorder, types.ResolvedInjection) {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/api/v1/internal/injection/"+h.grants[host].String()+"?"+q.Encode(), nil)
	srv.resolveADOInjection(w, r,
		&identity.Claims{RunID: h.run.ID, Sub: h.subject, SPIFFEID: "spiffe://wardyn.local/run"},
		broker.Minted{JTI: "jti-" + host, Injection: &egress.InjectionRule{Host: host, SecretName: types.ADOEntraAccessTokenSecret}},
		h.grants[host])
	var resp types.ResolvedInjection
	if w.Code == http.StatusOK {
		_ = json.Unmarshal(w.Body.Bytes(), &resp)
	}
	return w, resp
}

func (h *haADO) stateRows() int {
	var n int
	if err := h.pool.QueryRow(context.Background(), `SELECT count(*) FROM ado_run_pat_state WHERE run_id = $1`, h.run.ID).Scan(&n); err != nil {
		h.t.Fatal(err)
	}
	return n
}

// A run token created by A is served to a proxy that asks B, and the table holds it sealed.
func TestHALivePG_ARunTokenCachedByAIsServedToB(t *testing.T) {
	h := newHAADO(t)
	if !h.dispatch(t) {
		t.Fatalf("dispatch refused: %q", h.st.hint)
	}
	token := h.pats.tokens[0]
	if h.stateRows() != 1 {
		t.Fatal("dispatch recorded no token state in Postgres")
	}
	w, resp := h.resolveOn(h.b, "dev.azure.com", nil)
	if w.Code != http.StatusOK || resp.Value != "Basic "+base64.StdEncoding.EncodeToString([]byte(":"+token)) {
		t.Fatalf("B's resolve = %d %s, want A's token as Basic", w.Code, w.Body.String())
	}
	if n := h.pats.createCount(); n != 1 {
		t.Fatalf("creates = %d, want 1: B must serve A's token, not mint another", n)
	}
	var sealed []byte
	if err := h.pool.QueryRow(t.Context(), `SELECT sealed FROM ado_run_pat_state WHERE run_id = $1`, h.run.ID).Scan(&sealed); err != nil {
		t.Fatal(err)
	}
	if len(sealed) == 0 || strings.Contains(string(sealed), token) {
		t.Fatal("the token is not sealed in the table")
	}
}

// Two servers resolving one run's token at once, with no token yet, mint exactly one PAT.
func TestHALivePG_TwoServersResolvingConcurrentlyMintExactlyOnePAT(t *testing.T) {
	h := newHAADO(t)
	if !h.dispatch(t) {
		t.Fatalf("dispatch refused: %q", h.st.hint)
	}
	// Both servers restarted: the dispatch's token is not the state any more.
	if err := h.a.cfg.ADORunPATs.Delete(t.Context(), h.run.ID); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	var values []string
	hosts := []string{"dev.azure.com", "vssps.dev.azure.com", "contoso.visualstudio.com"}
	for i := range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			srv := h.a
			if i%2 == 1 {
				srv = h.b
			}
			w, resp := h.resolveOn(srv, hosts[i%len(hosts)], nil)
			if w.Code != http.StatusOK {
				t.Errorf("resolve %d = %d %s", i, w.Code, w.Body.String())
				return
			}
			mu.Lock()
			values = append(values, resp.Value)
			mu.Unlock()
		}()
	}
	wg.Wait()
	if n := h.pats.createCount(); n != 2 {
		t.Fatalf("creates = %d, want 2 (the dispatch's, then exactly one after the restart)", n)
	}
	if len(values) != 12 || len(slices.Compact(slices.Sorted(slices.Values(values)))) != 1 {
		t.Fatalf("the 12 resolves handed out %d distinct values, want one token", len(slices.Compact(slices.Sorted(slices.Values(values)))))
	}
	if n := len(h.st.unrevoked(t)); n != 2 {
		t.Fatalf("%d live token rows, want the dispatch's and the one minted after the restart", n)
	}
}

// A pause's revoke on A racing a resolve on B leaves no unrevoked token on the paused run, and the
// resume on B then creates one.
func TestHALivePG_APauseRevokeOnAAndAResumeOnBLeaveNoUnrevokedPATOnAPausedRun(t *testing.T) {
	h := newHAADO(t)
	if !h.dispatch(t) {
		t.Fatalf("dispatch refused: %q", h.st.hint)
	}
	ctx := context.Background()
	for i := range 12 {
		// Resumed: B serves a token (minting one when the pause emptied the state).
		h.st.edit(h.run.ID, func(r *types.AgentRun) { r.PausedAt, r.PausedReason = nil, "" })
		if w, _ := h.resolveOn(h.b, "dev.azure.com", nil); w.Code != http.StatusOK {
			t.Fatalf("round %d: the resume's resolve on B = %d %s", i, w.Code, w.Body.String())
		}
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { // B's resolve, racing
			defer wg.Done()
			h.resolveOn(h.b, "vssps.dev.azure.com", nil)
		}()
		go func() { // A's pause: the mark, then the revoke
			defer wg.Done()
			now := time.Now().UTC()
			h.st.edit(h.run.ID, func(r *types.AgentRun) { r.PausedAt, r.PausedReason = &now, "idle" })
			h.a.revokeRunPATs(ctx, h.run.ID, adoPATRevokePause)
		}()
		wg.Wait()
		if live := h.st.unrevoked(t); len(live) != 0 {
			t.Fatalf("round %d: %d unrevoked tokens on a paused run: %+v", i, len(live), live)
		}
	}
	// A resolve on a paused run creates nothing.
	creates := h.pats.createCount()
	if w, _ := h.resolveOn(h.b, "dev.azure.com", nil); w.Code != http.StatusConflict {
		t.Fatalf("a resolve on a paused run = %d, want 409", w.Code)
	}
	if h.pats.createCount() != creates {
		t.Fatal("a token was created for a paused run")
	}
}

// A person's erasure deletes their run tokens' rows and the output chunks, and no late write from
// any server recreates either.
func TestHALivePG_ErasureDeletesRunTokenAndChunkRowsAndNoLateWriteRecreatesThem(t *testing.T) {
	l := newMaskLab(t)
	a, b := l.replica(), l.replica()
	run := l.run()
	a.dispatch(t, run, "pg-secret-value-123")
	ctx := t.Context()
	rec := adorunpat.Record{Owner: maskOwner, AuthorizationID: "auth-1", Token: "the-run-token-value-0123456789abcdefghij", Scope: "vso.code", ValidTo: time.Now().Add(time.Hour)}
	if err := a.srv.cfg.ADORunPATs.Save(ctx, run.ID, maskOwner, rec); err != nil {
		t.Fatal(err)
	}
	w := a.srv.openExecOutput(run, false)
	writeExecOutput(t, w, "printed before the erasure\n")
	haWait(t, 5*time.Second, "the chunks", func() bool { return l.storedChunks(run.ID) != "" })

	if _, err := b.srv.eraseMaskCopies(ctx, maskOwner); err != nil {
		t.Fatal(err)
	}
	if _, err := b.srv.eraseRunOutputsOf(ctx, maskOwner); err != nil {
		t.Fatal(err)
	}
	tokenRows := func() int { return l.count(`SELECT count(*) FROM ado_run_pat_state WHERE owner = $1`, maskOwner) }
	if tokenRows() != 0 || l.storedChunks(run.ID) != "" {
		t.Fatalf("after the erasure: %d token rows, chunks %q, want none", tokenRows(), l.storedChunks(run.ID))
	}

	// Late writes: a replica caching a token, and the tail still printing.
	if err := a.srv.cfg.ADORunPATs.Save(ctx, run.ID, maskOwner, rec); err == nil {
		t.Fatal("a token was written back after the erasure")
	}
	// The fenced manifest already stops the masker passing a byte on; the tombstone stops the
	// table itself for any writer that got that far.
	writeExecOutput(t, w, "printed after the erasure\n")
	if _, err := store.NewPG(l.pool).AppendRunOutputChunk(ctx, run.ID, "late", []byte("x"), 1<<20); !errors.Is(err, store.ErrRunOutputErased) {
		t.Fatalf("a chunk written after the erasure: %v, want ErrRunOutputErased", err)
	}
	time.Sleep(4 * runOutputChunkEvery)
	if tokenRows() != 0 || l.storedChunks(run.ID) != "" {
		t.Fatalf("a late write recreated rows: %d token rows, chunks %q", tokenRows(), l.storedChunks(run.ID))
	}
}
