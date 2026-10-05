// Copyright 2025 The Wardyn Authors
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
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// govCovQuerier is a store.Querier that carries no data. It answers every statement from a script:
// an Exec returns a chosen command tag or error, a QueryRow's Scan returns a chosen error (no row by
// default), and a Query fails. It records the statements it saw, so a test can assert what a
// governance transaction did and, as importantly, what it did not do after a failed step.
type govCovQuerier struct {
	// execFn decides an Exec; nil answers a one-row command tag.
	execFn func(sql string) (pgconn.CommandTag, error)
	// rowErr decides a QueryRow's Scan error; nil answers pgx.ErrNoRows.
	rowErr func(sql string) error
	// queryErr is what every Query answers.
	queryErr error
	// seen is every statement, prefixed with the call that carried it.
	seen []string
}

func (q *govCovQuerier) Exec(_ context.Context, sql string, _ ...any) (pgconn.CommandTag, error) {
	q.seen = append(q.seen, "exec "+sql)
	if q.execFn != nil {
		return q.execFn(sql)
	}
	return pgconn.NewCommandTag("SELECT 1"), nil
}

func (q *govCovQuerier) Query(_ context.Context, sql string, _ ...any) (pgx.Rows, error) {
	q.seen = append(q.seen, "query "+sql)
	return nil, q.queryErr
}

func (q *govCovQuerier) QueryRow(_ context.Context, sql string, _ ...any) pgx.Row {
	q.seen = append(q.seen, "row "+sql)
	if q.rowErr != nil {
		return govCovRow{err: q.rowErr(sql)}
	}
	return govCovRow{err: pgx.ErrNoRows}
}

// writes lists the statements that changed data: one whose first word is INSERT, UPDATE or DELETE
// (a SELECT ... FOR UPDATE is a read).
func (q *govCovQuerier) writes() []string {
	var out []string
	for _, s := range q.seen {
		_, stmt, _ := strings.Cut(s, " ")
		switch strings.ToUpper(strings.Fields(stmt + " x")[0]) {
		case "INSERT", "UPDATE", "DELETE":
			out = append(out, s)
		}
	}
	return out
}

type govCovRow struct{ err error }

func (r govCovRow) Scan(...any) error { return r.err }

// govCovStore is the governance-change half of store.Store over scripted answers. Every method it
// does not define panics through the nil embedded Store, so a handler that reaches past what a test
// set up fails loudly.
type govCovStore struct {
	store.Store

	// list / get
	changes   []types.GovernanceChange
	listErr   error
	listState string
	got       types.GovernanceChange
	getErr    error

	// propose
	proposed       []types.GovernanceChange
	proposedTTL    time.Duration
	proposeSaved   types.GovernanceChange
	proposeExpired []store.ExpiredGovernanceChange
	proposeErr     error

	// decide: decideFn stands in for the decision transaction. decisions records what was asked.
	decideFn  func(fn store.GovernanceDecideFunc) (types.GovernanceChange, error)
	decisions []store.GovernanceDecision
	decideIDs []uuid.UUID

	// dryRunFn stands in for a rolled-back transaction; nil answers success without running the read.
	dryRunFn func(fn func(store.Querier) error) error
	dryRuns  int

	// the rest of the reads a hold handler makes
	userTypes    []types.UserType
	userTypeErr  error
	roleMappings []types.RoleMapping
	listRolesErr error
	userType     types.UserType
	getTypeErr   error
	profiles     []types.GovernanceProfile
	deleteErr    error
	writeErr     error
	apiTokens    []types.APIToken
	apiTokensErr error
	apiToken     types.APIToken
	deleted      []uuid.UUID
}

func (s *govCovStore) ListGovernanceChanges(_ context.Context, state string) ([]types.GovernanceChange, error) {
	s.listState = state
	return s.changes, s.listErr
}

func (s *govCovStore) GetGovernanceChange(context.Context, uuid.UUID) (types.GovernanceChange, error) {
	return s.got, s.getErr
}

func (s *govCovStore) ProposeGovernanceChange(_ context.Context, ch types.GovernanceChange, ttl time.Duration) (types.GovernanceChange, []store.ExpiredGovernanceChange, error) {
	s.proposed = append(s.proposed, ch)
	s.proposedTTL = ttl
	return s.proposeSaved, s.proposeExpired, s.proposeErr
}

func (s *govCovStore) DecideGovernanceChange(_ context.Context, id uuid.UUID, d store.GovernanceDecision, fn store.GovernanceDecideFunc) (types.GovernanceChange, error) {
	s.decisions = append(s.decisions, d)
	s.decideIDs = append(s.decideIDs, id)
	return s.decideFn(fn)
}

func (s *govCovStore) DryRunGovernance(_ context.Context, fn func(store.Querier) error) error {
	s.dryRuns++
	if s.dryRunFn == nil {
		return nil
	}
	return s.dryRunFn(fn)
}

func (s *govCovStore) ListUserTypes(context.Context) ([]types.UserType, error) {
	return s.userTypes, s.userTypeErr
}

func (s *govCovStore) ListRoleMappings(context.Context) ([]types.RoleMapping, error) {
	return s.roleMappings, s.listRolesErr
}

func (s *govCovStore) ListAPITokens(context.Context) ([]types.APIToken, error) {
	return s.apiTokens, s.apiTokensErr
}

func (s *govCovStore) GetAPITokenByRaw(context.Context, string) (types.APIToken, error) {
	return s.apiToken, nil
}

func (s *govCovStore) TouchAPIToken(context.Context, uuid.UUID, time.Time) error { return nil }

func (s *govCovStore) GetUserType(context.Context, string) (types.UserType, error) {
	return s.userType, s.getTypeErr
}

func (s *govCovStore) ListGovernanceProfiles(context.Context) ([]types.GovernanceProfile, error) {
	return s.profiles, nil
}

// WriteGovernanceProfile runs build over the stored profiles and, like the real store, returns what
// build returned without storing anything when build refuses.
func (s *govCovStore) WriteGovernanceProfile(_ context.Context, id uuid.UUID, build store.GovernanceProfileBuild) (types.GovernanceProfile, error) {
	p, err := build(s.profiles)
	if err != nil {
		return types.GovernanceProfile{}, err
	}
	if s.writeErr != nil {
		return types.GovernanceProfile{}, s.writeErr
	}
	p.ID = id
	return p, nil
}

func (s *govCovStore) DeleteGovernanceProfile(_ context.Context, id uuid.UUID) error {
	s.deleted = append(s.deleted, id)
	return s.deleteErr
}

// govCovServer is a server over st with SSO configured, so a signed session cookie is a human.
func govCovServer(t *testing.T, st store.Store, mutate ...func(*Config)) (*Server, *harness) {
	t.Helper()
	h := newHarness(t)
	cfg := baseTestConfig(h, st)
	cfg.OIDC = &oidc.Authenticator{}
	cfg.DefaultPolicy = types.RunPolicySpec{
		AllowedDomains:      []string{"api.anthropic.com", "pypi.org", "github.com"},
		MinConfinementClass: types.CC2,
	}
	for _, m := range mutate {
		m(&cfg)
	}
	return New(cfg), h
}

// govCovHumanReq is a request that carries a human on its context, for the helpers that take a
// request rather than going through the router.
func govCovHumanReq(method, path, sub, email, role string) *http.Request {
	r := httptest.NewRequest(method, path, nil)
	return r.WithContext(withHumanIdentity(r.Context(), sub, email, role, types.UserTypeStandard, nil, false))
}

func govCovAudits(h *harness, action string) []types.AuditEvent {
	var out []types.AuditEvent
	for _, ev := range h.audit.snapshot() {
		if ev.Action == action {
			out = append(out, ev)
		}
	}
	return out
}

func govCovAuditActions(h *harness) []string {
	var out []string
	for _, ev := range h.audit.snapshot() {
		out = append(out, ev.Action)
	}
	return out
}

// govCovDecode unmarshals a recorder's body into a generic map.
func govCovDecode(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatalf("body %q is not a JSON object: %v", w.Body.String(), err)
	}
	return m
}

// govCovAuditData decodes an audit row's data.
func govCovAuditData(t *testing.T, ev types.AuditEvent) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(ev.Data, &m); err != nil {
		t.Fatalf("audit %s data %q: %v", ev.Action, ev.Data, err)
	}
	return m
}

// govCovSession is a signed session for a human of the given role.
func govCovSession(t *testing.T, sub, email, role string) *http.Cookie {
	t.Helper()
	return ssoSession(t, sub, email, role)
}
