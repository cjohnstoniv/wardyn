// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
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

// sidePool is a second pool on the test database for the transactions a test holds open itself, so they never
// take a connection the server under test needs.
func (e *regPG) sidePool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	p, err := pgxpool.NewWithConfig(context.Background(), e.pool.Config().Copy())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	return p
}

// backendPID is the server process id of tx's connection.
func backendPID(t *testing.T, tx pgx.Tx) int32 {
	t.Helper()
	var pid int32
	if err := tx.QueryRow(context.Background(), `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	return pid
}

// waitBlockedBy waits until the database reports a backend waiting on holder's locks, other than skip, and
// returns that backend's process id: it is the database's own report that a request is blocked, not a guess
// from elapsed time.
func waitBlockedBy(t *testing.T, side *pgxpool.Pool, holder int32, skip ...int32) int32 {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		var pid int32
		err := side.QueryRow(context.Background(), `SELECT pid FROM pg_stat_activity
			WHERE $1::int = ANY(pg_blocking_pids(pid)) AND NOT (pid = ANY($2::int[])) ORDER BY pid LIMIT 1`, holder, append([]int32{0}, skip...)).Scan(&pid)
		switch {
		case err == nil:
			return pid
		case !errors.Is(err, pgx.ErrNoRows):
			t.Fatal(err)
		case time.Now().After(deadline):
			t.Fatalf("no backend waited on process %d", holder)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// waitWaitsOn waits until the database reports that waiter is blocked by holder.
func waitWaitsOn(t *testing.T, side *pgxpool.Pool, waiter, holder int32) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		var on bool
		if err := side.QueryRow(context.Background(), `SELECT $2::int = ANY(pg_blocking_pids($1::int))`, waiter, holder).Scan(&on); err != nil {
			t.Fatal(err)
		}
		if on {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("process %d never waited on process %d", waiter, holder)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// credentialUnknown checks that a held or refused registration's credential resolves to no portal: the lookup
// answers ErrNotFound, not a storage failure.
func (e *regPG) credentialUnknown(t *testing.T, cred string) {
	t.Helper()
	if _, err := e.st.GetIssuerDelegateByRaw(context.Background(), cred); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("credential lookup = %v, want store.ErrNotFound", err)
	}
}

// A person named in the configuration refuses a first registration on every path. The configured default
// policy refuses a direct write and a proposal (nothing is held). A change to the default policy after a
// proposal makes the approval stale, which no later configuration revives, so the registration is proposed
// again. The operator list, which the proposal does not freeze, then refuses the approval with the change left
// pending, and once the reference is gone the same change is approved.
func TestPortalRegistration_ConfiguredReferenceRefusedDirectHeldAndApproved_PG(t *testing.T) {
	e := newRegPG(t)
	srv := e.h.srv
	refer := func(issuer string) {
		person := types.PortalPrincipal(types.PortalIssuerKey(issuer), "00uSole")
		srv.cfg.DefaultPolicy.UIApps = []types.UIApp{{Name: "app", Port: 8080, Inbound: true, Callers: []string{"user:" + person}}}
	}
	clear := func() { srv.cfg.DefaultPolicy.UIApps = nil }
	t.Cleanup(clear)
	operators := srv.cfg.OperatorEmails
	t.Cleanup(func() { srv.cfg.OperatorEmails = operators })
	wantInUse := func(name string, w *httptest.ResponseRecorder, source string) {
		t.Helper()
		if w.Code != http.StatusConflict || reasonOf(t, w) != reasonDelegateNamespaceInUse || !strings.Contains(w.Body.String(), source+" (1)") {
			t.Errorf("%s = %d %s, want 409 %s naming %s", name, w.Code, w.Body.String(), reasonDelegateNamespaceInUse, source)
		}
	}

	const direct = "https://cfg-direct.example.com"
	refer(direct)
	wantInUse("direct registration", e.register(t, map[string]any{"issuer": direct}), censusConfiguredDefaultPolicy)
	if e.portals(t, direct) != 0 {
		t.Error("a refused direct registration stored a portal")
	}
	clear()
	e.registered(t, map[string]any{"issuer": direct})

	t.Setenv(envGovernanceSecondHuman, "true")
	const held = "https://cfg-held.example.com"
	refer(held)
	before := e.countPending(t)
	wantInUse("proposal", e.register(t, map[string]any{"issuer": held}), censusConfiguredDefaultPolicy)
	if e.countPending(t) != before {
		t.Error("a refused proposal left a pending change")
	}
	clear()
	if _, _, w := e.proposeHeld(t, map[string]any{"issuer": held}); w.Code != http.StatusAccepted {
		t.Fatalf("control proposal with no reference = %d %s, want 202", w.Code, w.Body.String())
	}

	// The default policy is frozen into the proposal: a change to it makes the approval stale for good.
	const late = "https://cfg-late.example.com"
	change, cred, w := e.proposeHeld(t, map[string]any{"issuer": late})
	if change == "" {
		t.Fatalf("proposal = %d %s, want 202", w.Code, w.Body.String())
	}
	refer(late)
	if w := e.decideHeld(t, change, "approve"); w.Code != http.StatusConflict || reasonOf(t, w) != reasonGovernanceChangeStale {
		t.Errorf("approval after a default-policy change = %d %s, want 409 %s", w.Code, w.Body.String(), reasonGovernanceChangeStale)
	}
	if s := e.changeState(t, change); s != "stale" {
		t.Errorf("state after the default-policy change = %s, want stale", s)
	}
	clear()
	if w := e.decideHeld(t, change, "approve"); w.Code != http.StatusConflict || reasonOf(t, w) != reasonGovernanceChangeNotPending {
		t.Errorf("approval of the stale change after clearing = %d %s, want 409 %s", w.Code, w.Body.String(), reasonGovernanceChangeNotPending)
	}
	if e.portals(t, late) != 0 {
		t.Error("a stale change stored a portal")
	}
	e.credentialUnknown(t, cred)

	// Proposed again; the operator list then names a person under the key while the change is held.
	change, cred, w = e.proposeHeld(t, map[string]any{"issuer": late})
	if change == "" {
		t.Fatalf("second proposal = %d %s, want 202", w.Code, w.Body.String())
	}
	srv.cfg.OperatorEmails = append(append([]string(nil), operators...), types.PortalPrincipal(types.PortalIssuerKey(late), "00uOp"))
	wantInUse("approval", e.decideHeld(t, change, "approve"), censusConfiguredOperatorEmails)
	if s := e.changeState(t, change); s != "pending" {
		t.Errorf("state after the refused approval = %s, want pending", s)
	}
	if e.portals(t, late) != 0 {
		t.Error("a refused approval stored a portal")
	}
	e.credentialUnknown(t, cred)
	srv.cfg.OperatorEmails = operators
	if w := e.decideHeld(t, change, "approve"); w.Code != http.StatusOK {
		t.Fatalf("approval with the reference removed = %d %s, want 200", w.Code, w.Body.String())
	}
	if d, err := e.st.GetIssuerDelegateByRaw(context.Background(), cred); err != nil || d.Issuer != late {
		t.Errorf("the approved portal = %+v, %v", d, err)
	}
}

// The approval reads the pin's unit and a custom user type on its own transaction and holds both rows until
// it commits. Both orders run the real decision, and each wait is the one the database reports
// (pg_blocking_pids): a removal that holds the row first makes the decision wait, and once it commits the
// decision is refused 409; a removal that starts while the decision holds the row waits for the decision to
// commit. That later removal then commits; keeping the row for good is the use-time checks' concern.
func TestPortalRegistration_ApprovalHoldsPinAndTypeThroughCommit_PG(t *testing.T) {
	e := newRegPG(t)
	ctx := context.Background()
	side := e.sidePool(t)
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
	propose := func(t *testing.T, unit uuid.UUID, ut string) (change, issuer string) {
		t.Helper()
		n++
		issuer = "https://race-" + strings.Repeat("r", n) + ".example.com"
		change, _, w := e.proposeHeld(t, map[string]any{"issuer": issuer, "run_in_under": unit.String(), "user_type": ut})
		if change == "" {
			t.Fatalf("proposal = %d %s, want 202", w.Code, w.Body.String())
		}
		return change, issuer
	}
	approve := func(change string) <-chan *httptest.ResponseRecorder {
		got := make(chan *httptest.ResponseRecorder, 1)
		go func() { got <- e.decideHeld(t, change, "approve") }()
		return got
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
			change, issuer := propose(t, unit, ut)
			tx, err := side.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback(ctx) }()
			if err := rm.remove(ctx, tx, unit, ut); err != nil {
				t.Fatal(err)
			}
			got := approve(change)
			waitBlockedBy(t, side, backendPID(t, tx)) // the decision waits on the uncommitted removal's row lock
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
			if e.portals(t, issuer) != 0 {
				t.Error("a refused approval stored a portal")
			}
		})
		t.Run(name+" removal starts while the decision holds the row", func(t *testing.T) {
			unit, ut := newRefs(t)
			change, issuer := propose(t, unit, ut)
			// An uncommitted binding of the same issuer parks the decision at its own binding insert, after it
			// has locked the pin and the type and before it commits.
			park, err := side.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = park.Rollback(ctx) }()
			if _, err := e.st.BindPortalIssuerQ(ctx, park, issuer, "", portalPersonIDClaim(oidc.PortalProfileAccessToken), "test"); err != nil {
				t.Fatal(err)
			}
			got := approve(change)
			decider := waitBlockedBy(t, side, backendPID(t, park))
			rtx, err := side.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = rtx.Rollback(ctx) }()
			removed := make(chan error, 1)
			remover := backendPID(t, rtx)
			go func() {
				err := rm.remove(ctx, rtx, unit, ut)
				if err == nil {
					err = rtx.Commit(ctx)
				}
				removed <- err
			}()
			waitWaitsOn(t, side, remover, decider) // the removal waits on the decision's row lock
			if err := park.Rollback(ctx); err != nil {
				t.Fatal(err)
			}
			if w := <-got; w.Code != http.StatusOK {
				t.Fatalf("approval = %d %s, want 200", w.Code, w.Body.String())
			}
			if err := <-removed; err != nil {
				t.Errorf("the removal after the decision committed = %v, want it to commit", err)
			}
			if e.portals(t, issuer) != 1 {
				t.Error("the approved registration stored no portal")
			}
		})
	}
}
