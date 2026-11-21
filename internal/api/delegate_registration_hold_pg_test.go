// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// proposeHeld proposes one registration (second-human mode on) and returns its change id and credential.
func (e *regPG) proposeHeld(t *testing.T, mut map[string]any) (change, cred string, w *httptest.ResponseRecorder) {
	t.Helper()
	w = e.register(t, mut)
	if w.Code != http.StatusAccepted {
		return "", "", w
	}
	var got struct {
		Pending struct {
			ID uuid.UUID `json:"id"`
		} `json:"pending_change"`
		Credential string `json:"credential"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	return got.Pending.ID.String(), got.Credential, w
}

func (e *regPG) decideHeld(t *testing.T, change, verb string) *httptest.ResponseRecorder {
	t.Helper()
	return doSSO(t, e.h.srv, http.MethodPost, "/api/v1/governance/changes/"+change+"/"+verb, e.admin2, `{"reason":"no"}`)
}

func (e *regPG) changeState(t *testing.T, change string) string {
	t.Helper()
	var s string
	if err := e.pool.QueryRow(context.Background(), `SELECT state FROM governance_changes WHERE id = $1`, change).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

func (e *regPG) countPending(t *testing.T) int {
	t.Helper()
	var n int
	if err := e.pool.QueryRow(context.Background(), `SELECT count(*) FROM governance_changes WHERE target_kind = 'delegate'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// A person named only in the configured default policy refuses a first registration on every path: the
// direct write, the proposal (nothing is held) and the approval (the reference was added after the proposal,
// and the change stays pending). With no reference the same registration succeeds.
func TestPortalRegistration_ConfiguredReferenceRefusedDirectHeldAndApproved_PG(t *testing.T) {
	e := newRegPG(t)
	ctx := context.Background()
	srv := e.h.srv
	refer := func(issuer string) {
		person := types.PortalPrincipal(types.PortalIssuerKey(issuer), "00uSole")
		srv.cfg.DefaultPolicy.UIApps = []types.UIApp{{Name: "app", Port: 8080, Inbound: true, Callers: []string{"user:" + person}}}
	}
	clear := func() { srv.cfg.DefaultPolicy.UIApps = nil }
	t.Cleanup(clear)
	wantInUse := func(name string, w *httptest.ResponseRecorder) {
		t.Helper()
		if w.Code != http.StatusConflict || reasonOf(t, w) != reasonDelegateNamespaceInUse || !strings.Contains(w.Body.String(), censusConfiguredDefaultPolicy+" (1)") {
			t.Errorf("%s = %d %s, want 409 %s naming %s", name, w.Code, w.Body.String(), reasonDelegateNamespaceInUse, censusConfiguredDefaultPolicy)
		}
	}

	const direct = "https://cfg-direct.example.com"
	refer(direct)
	wantInUse("direct registration", e.register(t, map[string]any{"issuer": direct}))
	if e.portals(t, direct) != 0 {
		t.Error("a refused direct registration stored a portal")
	}
	clear()
	e.registered(t, map[string]any{"issuer": direct})

	t.Setenv(envGovernanceSecondHuman, "true")
	const held = "https://cfg-held.example.com"
	refer(held)
	before := e.countPending(t)
	wantInUse("proposal", e.register(t, map[string]any{"issuer": held}))
	if e.countPending(t) != before {
		t.Error("a refused proposal left a pending change")
	}
	clear()
	if _, _, w := e.proposeHeld(t, map[string]any{"issuer": held}); w.Code != http.StatusAccepted {
		t.Fatalf("control proposal with no reference = %d %s, want 202", w.Code, w.Body.String())
	}

	const late = "https://cfg-late.example.com"
	change, cred, w := e.proposeHeld(t, map[string]any{"issuer": late})
	if change == "" {
		t.Fatalf("proposal = %d %s, want 202", w.Code, w.Body.String())
	}
	refer(late) // the configuration changes while the change is held
	wantInUse("approval", e.decideHeld(t, change, "approve"))
	if s := e.changeState(t, change); s != "pending" {
		t.Errorf("state after the refused approval = %s, want pending", s)
	}
	if e.portals(t, late) != 0 {
		t.Error("a refused approval stored a portal")
	}
	if _, err := e.st.GetIssuerDelegateByRaw(ctx, cred); err == nil {
		t.Error("the credential of a refused approval resolves to a portal")
	}
	clear()
	if w := e.decideHeld(t, change, "approve"); w.Code != http.StatusOK {
		t.Errorf("approval with the reference removed = %d %s, want 200", w.Code, w.Body.String())
	}
}

// The approval reads the pin's unit and a custom user type on its own transaction and holds both rows until
// it commits. Both commit orders: a removal that commits first refuses the approval 409, and a removal that
// starts while the decision holds the rows waits until it commits.
func TestPortalRegistration_ApprovalHoldsPinAndTypeThroughCommit_PG(t *testing.T) {
	e := newRegPG(t)
	ctx := context.Background()
	t.Setenv(envGovernanceSecondHuman, "true")
	n := 0
	newRefs := func(t *testing.T) (unit uuid.UUID, ut string) {
		t.Helper()
		unit, ut = uuid.New(), "race-"+strings.ToLower(uuid.NewString()[:8])
		if _, err := e.pool.Exec(ctx, `INSERT INTO governance_units (id, name, ceiling, limits, created_by) VALUES ($1,$2,'{}','{}','t')`, unit, "race-"+unit.String()); err != nil {
			t.Fatal(err)
		}
		if _, err := e.st.CreateUserType(ctx, types.UserType{ID: ut, Name: ut}); err != nil {
			t.Fatal(err)
		}
		return unit, ut
	}
	propose := func(t *testing.T, unit uuid.UUID, ut string) string {
		t.Helper()
		n++
		change, _, w := e.proposeHeld(t, map[string]any{"issuer": "https://race-" + strings.Repeat("r", n) + ".example.com", "run_in_under": unit.String(), "user_type": ut})
		if change == "" {
			t.Fatalf("proposal = %d %s, want 202", w.Code, w.Body.String())
		}
		return change
	}
	// parked reports whether done is still unfinished after a wait long enough for an unblocked statement.
	parked := func(done <-chan struct{}) bool {
		select {
		case <-done:
			return false
		case <-time.After(500 * time.Millisecond):
			return true
		}
	}
	removals := map[string]struct {
		reason string
		remove func(ctx context.Context, q store.Querier, unit uuid.UUID, ut string) error
	}{
		"unit": {reasonDelegateRunInUnderInvalid, func(ctx context.Context, q store.Querier, unit uuid.UUID, _ string) error {
			_, err := q.Exec(ctx, `DELETE FROM governance_units WHERE id = $1`, unit)
			return err
		}},
		"user type": {reasonDelegateUserTypeInvalid, func(ctx context.Context, q store.Querier, _ uuid.UUID, ut string) error {
			_, err := q.Exec(ctx, `DELETE FROM user_types WHERE id = $1`, ut)
			return err
		}},
	}
	for name, rm := range removals {
		t.Run(name+" removal commits first", func(t *testing.T) {
			unit, ut := newRefs(t)
			change := propose(t, unit, ut)
			tx, err := e.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback(ctx) }()
			if err := rm.remove(ctx, tx, unit, ut); err != nil {
				t.Fatal(err)
			}
			got := make(chan *httptest.ResponseRecorder, 1)
			done := make(chan struct{})
			go func() { got <- e.decideHeld(t, change, "approve"); close(done) }()
			if !parked(done) {
				t.Fatal("the approval did not wait for the uncommitted removal's row lock")
			}
			if err := tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			w := <-got
			if w.Code != http.StatusConflict || reasonOf(t, w) != rm.reason {
				t.Errorf("approval after the removal committed = %d %s, want 409 %s", w.Code, w.Body.String(), rm.reason)
			}
			if s := e.changeState(t, change); s != "pending" {
				t.Errorf("state = %s, want pending", s)
			}
		})
		t.Run(name+" removal starts while the decision holds the row", func(t *testing.T) {
			unit, ut := newRefs(t)
			d := types.Delegate{RunInUnder: &unit, UserType: ut}
			decision, err := e.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = decision.Rollback(ctx) }()
			if ref, err := lockPortalReferences(ctx, decision, d); err != nil || ref != nil {
				t.Fatalf("lockPortalReferences = %v, %v, want both held", ref, err)
			}
			done := make(chan struct{})
			go func() {
				defer close(done)
				rtx, err := e.pool.Begin(ctx)
				if err != nil {
					return
				}
				defer func() { _ = rtx.Rollback(ctx) }()
				if rm.remove(ctx, rtx, unit, ut) == nil {
					_ = rtx.Commit(ctx)
				}
			}()
			if !parked(done) {
				t.Fatal("a removal committed while the decision held the row")
			}
			if err := decision.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Fatal("the removal never ran after the decision committed")
			}
			// Read again after the removal: the rows are gone and the lock read says so.
			tx, err := e.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback(ctx) }()
			ref, err := lockPortalReferences(ctx, tx, d)
			if err != nil || ref == nil || ref.reason != rm.reason {
				t.Errorf("lock read after the removal = %+v, %v, want refusal %s", ref, err, rm.reason)
			}
		})
	}
}
