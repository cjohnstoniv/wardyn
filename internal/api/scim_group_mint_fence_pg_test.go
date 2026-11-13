// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"runtime/debug"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/test/entrafake"
)

// A person mints their own token while the identity provider removes them from a group. Token use replays the
// token's login-time group snapshot and never reads a sessions-only cut, so a token written after the removal's
// cut and sweep would act with the group the person left. Guarded by WARDYN_TEST_PG (newSCIMEnv).

// removeDuringMint signs the mover in with the group in their claim and arms the late-check hook to remove them
// from the group on the other instance, once, after the mint's late session check has answered "not cut".
func removeDuringMint(t *testing.T, w *moverWorld) *http.Cookie {
	t.Helper()
	e := w.e
	cookie := sessionCookieOf(e.signInAs(e.a, entrafake.Identity{Username: moverEmail, Subject: "sub-mover", Groups: []string{groupClaim}}))
	if cookie == nil {
		t.Fatal("the mover cannot sign in")
	}
	patch := groupRemovePatch(t, "entra-doc-patch-group-remove-members-value-array.json", w.moverID)
	e.rev.setLateCheck(func() {
		if r := e.patchGroup(e.b, w.group.ID, patch); r.Code != http.StatusOK {
			t.Errorf("remove member = %d %s", r.Code, r.Body.String())
		}
	})
	return cookie
}

// refusedAcrossRemoval asserts the mint got the owner guard's refusal and wrote no token, and that the removal
// it raced ran in full.
func refusedAcrossRemoval(t *testing.T, w *moverWorld, r *httptest.ResponseRecorder) {
	t.Helper()
	e := w.e
	if r.Code == http.StatusCreated {
		var created types.APIToken
		if err := json.Unmarshal(r.Body.Bytes(), &created); err == nil && (e.tokenWorks(e.a, created.Token) || e.tokenWorks(e.b, created.Token)) {
			t.Error("a token minted across the group removal authenticates, with the removed group in its snapshot")
		}
	}
	if r.Code != http.StatusForbidden || errorReason(r) != reasonIdentityDeactivated {
		t.Errorf("the mint across the removal = %d %s, want 403 %s", r.Code, r.Body.String(), reasonIdentityDeactivated)
	}
	if toks, err := e.st.ListAPITokensByPrincipal(context.Background(), "sub-mover"); err != nil || len(toks) != 3 {
		t.Errorf("the mover holds %d tokens (%v), want the world's 3 and none from the mint", len(toks), err)
	}
	w.checkRemoved(t, e.a, e.b)
}

// The self-service mint: its insert lands after the removal's cut and token sweep, and is refused.
func TestSCIMGroupRemovalRefusesAMintInFlight(t *testing.T) {
	w := newMoverWorld(t)
	cookie := removeDuringMint(t, w)
	refusedAcrossRemoval(t, w, doSSO(t, w.e.a.srv, http.MethodPost, "/api/v1/me/tokens", cookie, `{"name":"racer"}`))
}

// An invitation's claim, the other door a person mints their own token through: refused the same way, and the
// invitation stays pending.
func TestSCIMGroupRemovalRefusesAnInvitationClaimInFlight(t *testing.T) {
	w := newMoverWorld(t)
	e := w.e
	ctx := context.Background()
	now := time.Now().UTC()
	inv, err := e.st.CreateInvitation(ctx, types.Invitation{ID: uuid.New(), Target: "sub-mover", CreatedBy: "sec", CreatedAt: now,
		ExpiresAt: now.Add(time.Hour), TokenName: "ci"}, 10, now)
	if err != nil {
		t.Fatal(err)
	}
	cookie := removeDuringMint(t, w)
	refusedAcrossRemoval(t, w, doSSO(t, e.a.srv, http.MethodPost, "/api/v1/me/invitations/"+inv.ID.String()+"/claim", cookie, `{}`))
	if got, err := e.st.GetInvitation(ctx, inv.ID, ""); err != nil || got.StateAt(time.Now()) != types.InvitationPending {
		t.Errorf("invitation after the refused claim = %+v (%v), want pending", got, err)
	}
}

// holdTokenFinish runs hook once, before the first group-removal token step it is asked to complete.
type holdTokenFinish struct {
	store.PG
	mu   sync.Mutex
	hook func()
}

func (s *holdTokenFinish) FinishDeprovisionJob(ctx context.Context, id uuid.UUID, kind string, k store.JobKey, detail map[string]int) error {
	if kind == store.JobKindGroupRemove && k.Step == store.GroupStepTokens {
		s.mu.Lock()
		hook := s.hook
		s.hook = nil
		s.mu.Unlock()
		if hook != nil {
			hook()
		}
	}
	return s.PG.FinishDeprovisionJob(ctx, id, kind, k, detail)
}

// pauseJobRead holds the first group-removal ledger read after its removal opened, until release is closed.
type pauseJobRead struct {
	store.PG
	mu      sync.Mutex
	started bool
	reached chan struct{}
	release chan struct{}
}

func (s *pauseJobRead) StartGroupRemoval(ctx context.Context, groupID, identityID uuid.UUID) error {
	err := s.PG.StartGroupRemoval(ctx, groupID, identityID)
	s.mu.Lock()
	s.started = err == nil
	s.mu.Unlock()
	return err
}

func (s *pauseJobRead) ListDeprovisionJobs(ctx context.Context, id uuid.UUID, kind string) ([]store.DeprovisionJob, error) {
	s.mu.Lock()
	hold := s.started && kind == store.JobKindGroupRemove
	s.started = false
	s.mu.Unlock()
	if hold {
		close(s.reached)
		<-s.release
	}
	return s.PG.ListDeprovisionJobs(ctx, id, kind)
}

// An older removal of the mover completes its token step only after the person was added back, signed in,
// minted a token and was removed again, and the newer removal reads its ledger after that completion. The older
// completion must not mark the newer removal's token step done: the newer removal sweeps, the token minted
// between the two stops authenticating, and the newer removal writes the one row, counting that token.
func TestSCIMGroupRemovalOlderCompletionLeavesTheNewerRemoval(t *testing.T) {
	w := newMoverWorld(t)
	e := w.e
	patch := groupRemovePatch(t, "entra-doc-patch-group-remove-members-value-array.json", w.moverID)
	second := &pauseJobRead{PG: e.st, reached: make(chan struct{}), release: make(chan struct{})}
	newer := e.node(func(c *Config) { c.Store = second })
	done := make(chan *httptest.ResponseRecorder, 1)
	var between groupToken
	first := &holdTokenFinish{PG: e.st, hook: func() {
		if r := e.patchGroup(e.a, w.group.ID, addMembersPatch(w.moverID)); r.Code != http.StatusOK {
			t.Errorf("re-add = %d %s", r.Code, r.Body.String())
		}
		_, between.id, between.raw = e.mintTokenAs(e.a, entrafake.Identity{Username: moverEmail, Subject: "sub-mover", Groups: []string{groupClaim}})
		go func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("panic: %v\n%s", r, debug.Stack())
					done <- httptest.NewRecorder()
				}
			}()
			done <- e.patchGroup(newer, w.group.ID, patch)
		}()
		select {
		case <-second.reached:
		case <-time.After(10 * time.Second):
			t.Error("the newer removal never reached its ledger read")
		}
	}}
	older := e.node(func(c *Config) { c.Store = first })

	if r := e.patchGroup(older, w.group.ID, patch); r.Code != http.StatusOK {
		t.Errorf("the older removal = %d %s", r.Code, r.Body.String())
	}
	if between.raw == "" {
		t.Fatal("the older removal never reached its token step's completion")
	}
	close(second.release)
	if r := <-done; r.Code != http.StatusOK {
		t.Fatalf("the newer removal = %d %s", r.Code, r.Body.String())
	}

	for name, n := range map[string]*scimNode{"a": e.a, "b": e.b} {
		if e.tokenWorks(n, between.raw) {
			t.Errorf("instance %s: the token minted between the two removals still authenticates", name)
		}
	}
	if !e.tokenRevoked(between.id) {
		t.Error("the token minted between the two removals is not marked revoked")
	}
	if !e.groupJobsDone(w.moverID, w.group.ID) {
		t.Error("the newer removal left ledger steps pending")
	}
	if rows := e.rows(older, "scim.group.member_remove"); len(rows) != 0 {
		t.Errorf("the older removal wrote %d scim.group.member_remove rows from the newer removal's ledger, want none", len(rows))
	}
	if rows := e.rows(newer, "scim.group.member_remove"); len(rows) != 1 || dataOf(t, rows[0])["tokens_revoked"] != float64(1) {
		t.Errorf("the newer removal's scim.group.member_remove rows = %+v, want one counting the token minted between", rows)
	}
}
