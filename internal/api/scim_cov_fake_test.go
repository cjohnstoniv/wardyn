// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/scim"
	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// scimCovStore is the store the SCIM routes reach, over memory: the identity rows, the deprovisioning
// ledger, the groups, and the handful of ordinary store calls a suspension or a purge makes. Every
// method it does not define panics through the nil embedded Store, so a handler that reaches past
// what a test set up fails loudly. It keeps the arguments of every write, a call log, and one
// injectable error per method.
type scimCovStore struct {
	store.Store
	mu sync.Mutex

	idents  map[uuid.UUID]*store.PrincipalIdentity
	order   []uuid.UUID
	aliases map[uuid.UUID][]scimCovAlias
	// emailSubs is what PrincipalsByEmail answers per lower-cased email.
	emailSubs map[string][]string

	jobs    []*store.DeprovisionJob
	groups  map[uuid.UUID]*store.ScimGroup
	members map[uuid.UUID][]uuid.UUID

	runs       []*types.AgentRun
	tokens     []*types.APIToken
	keys       []types.SSHPublicKey
	workspaces []types.Workspace
	grants     []types.UserDriveGrant
	drives     []types.UserDriveListItem

	pending       []store.PendingLeaver
	failures      []store.DeprovisionFailure
	grantsDeleted int64
	assignDeleted int64

	// fails is how many more calls of a method fail (negative: every call); failErr is what they answer.
	fails   map[string]int
	failErr map[string]error
	// skips is how many calls of a method pass before its armed failure applies.
	skips map[string]int
	// casLost is how many UpdateRunStateIf calls answer "someone else moved the run".
	casLost int

	calls       []string
	nextID      int
	plans       []store.SuspendPlan
	updates     []scimCovUpdate
	creates     []scimCovCreate
	searches    []scimCovSearch
	purgeMarks  []bool
	deleteRows  [][]string
	ownerWrites []scimCovOwnerWrite
	deletedKeys []string
	revokedToks []uuid.UUID
	pendingArgs []time.Duration
	now         time.Time
	// afterDue runs once PurgeDueIdentities has computed its answer, outside the lock: the window in
	// which a reactivation can commit before the purge re-checks.
	afterDue func()
}

// scimCovAlias is one alias row: the store keeps a user name and an email under different kinds.
type scimCovAlias struct{ kind, value string }

type scimCovUpdate struct {
	ID uuid.UUID
	U  store.IdentityUpdate
}

type scimCovCreate struct {
	Issuer, Tenant, Object string
	U                      store.IdentityUpdate
}

type scimCovSearch struct {
	Issuer, Attr, Value string
	Limit               int
}

type scimCovOwnerWrite struct {
	ID    uuid.UUID
	Owner string
}

var scimCovNow = time.Now().UTC().Truncate(time.Second)

func newSCIMCovStore() *scimCovStore {
	return &scimCovStore{
		idents: map[uuid.UUID]*store.PrincipalIdentity{}, aliases: map[uuid.UUID][]scimCovAlias{},
		emailSubs: map[string][]string{}, groups: map[uuid.UUID]*store.ScimGroup{},
		members: map[uuid.UUID][]uuid.UUID{}, fails: map[string]int{}, failErr: map[string]error{}, skips: map[string]int{}, now: scimCovNow,
	}
}

// failNext makes the next n calls of method answer err; a negative n fails every call. A ledger method
// may be named "Method:kind" to fail one kind only.
func (s *scimCovStore) failNext(method string, err error, n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fails[method], s.failErr[method] = n, err
}

// failAfter lets the next skip calls of method pass, then fails the one after with err.
func (s *scimCovStore) failAfter(method string, err error, skip int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fails[method], s.failErr[method], s.skips[method] = 1, err, skip
}

// enter logs the call and returns the injected error, if one is armed for it. The caller holds mu.
func (s *scimCovStore) enter(method string, kind ...string) error {
	s.calls = append(s.calls, method)
	names := []string{method}
	if len(kind) > 0 {
		names = []string{method + ":" + kind[0], method}
	}
	for _, name := range names {
		if s.skips[name] > 0 {
			s.skips[name]--
			continue
		}
		if n := s.fails[name]; n != 0 {
			if n > 0 {
				s.fails[name]--
			}
			return s.failErr[name]
		}
	}
	return nil
}

func (s *scimCovStore) callCount(method string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(slices.DeleteFunc(slices.Clone(s.calls), func(c string) bool { return c != method }))
}

func (s *scimCovStore) addIdentity(i store.PrincipalIdentity, aliases ...string) store.PrincipalIdentity {
	s.mu.Lock()
	defer s.mu.Unlock()
	if i.ID == uuid.Nil {
		i.ID = s.newID()
	}
	if i.CreatedAt.IsZero() {
		i.CreatedAt = scimCovNow.Add(-24 * time.Hour)
	}
	cp := i
	s.idents[i.ID] = &cp
	s.order = append(s.order, i.ID)
	for _, a := range aliases {
		kind := "user_name"
		if strings.Contains(a, "@") {
			kind = "email"
		}
		s.aliases[i.ID] = append(s.aliases[i.ID], scimCovAlias{kind, a})
	}
	return i
}

func (s *scimCovStore) newID() uuid.UUID {
	s.nextID++
	return uuid.MustParse(fmt.Sprintf("00000000-0000-4000-8000-%012d", s.nextID))
}

func (s *scimCovStore) identity(id uuid.UUID) store.PrincipalIdentity {
	s.mu.Lock()
	defer s.mu.Unlock()
	return *s.idents[id]
}

func (s *scimCovStore) addRun(createdBy string, state types.RunState, sandbox string) uuid.UUID {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := s.newID()
	s.runs = append(s.runs, &types.AgentRun{ID: id, CreatedBy: createdBy, State: state, SandboxRef: sandbox, Agent: "claude-code"})
	return id
}

func (s *scimCovStore) runState(id uuid.UUID) types.RunState {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.runs {
		if r.ID == id {
			return r.State
		}
	}
	return ""
}

func (s *scimCovStore) addToken(t types.APIToken) uuid.UUID {
	s.mu.Lock()
	defer s.mu.Unlock()
	if t.ID == uuid.Nil {
		t.ID = s.newID()
	}
	s.tokens = append(s.tokens, &t)
	return t.ID
}

func (s *scimCovStore) tokenRevoked(id uuid.UUID) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, t := range s.tokens {
		if t.ID == id {
			return t.RevokedAt != nil
		}
	}
	return false
}

// ledger is the rows of one identity and kind, as the store would list them.
func (s *scimCovStore) ledger(id uuid.UUID, kind string) []store.DeprovisionJob {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.listJobs(id, kind)
}

func (s *scimCovStore) listJobs(id uuid.UUID, kind string) []store.DeprovisionJob {
	var out []store.DeprovisionJob
	for _, j := range s.jobs {
		if j.IdentityID == id && j.Kind == kind {
			out = append(out, *j)
		}
	}
	return out
}

func (s *scimCovStore) findJob(id uuid.UUID, kind string, k store.JobKey) *store.DeprovisionJob {
	for _, j := range s.jobs {
		if j.IdentityID == id && j.Kind == kind && j.Step == k.Step && j.Target == k.Target {
			return j
		}
	}
	return nil
}

func (s *scimCovStore) pendingSteps(id uuid.UUID, kind string) []string {
	var out []string
	for _, j := range s.ledger(id, kind) {
		if !j.Done {
			out = append(out, j.Step+"/"+j.Target)
		}
	}
	return out
}

// ---- store.PrincipalIdentityStore ----

func (s *scimCovStore) UpsertLoginIdentity(context.Context, store.LoginIdentity, time.Time) (store.PrincipalIdentity, error) {
	panic("scimCovStore: a sign-in is not part of the SCIM routes")
}

func (s *scimCovStore) GetIdentityByObject(_ context.Context, issuer, tenantID, objectID string) (store.PrincipalIdentity, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.enter("GetIdentityByObject"); err != nil {
		return store.PrincipalIdentity{}, err
	}
	for _, id := range s.order {
		if i := s.idents[id]; objectID != "" && i.Issuer == issuer && i.TenantID == tenantID && i.ObjectID == objectID {
			return *i, nil
		}
	}
	return store.PrincipalIdentity{}, store.ErrNotFound
}

func (s *scimCovStore) IdentitiesByPrincipal(_ context.Context, principal string) ([]store.PrincipalIdentity, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.enter("IdentitiesByPrincipal"); err != nil {
		return nil, err
	}
	var out []store.PrincipalIdentity
	for _, id := range s.order {
		if i := s.idents[id]; i.Principal == principal {
			out = append(out, *i)
		}
	}
	return out, nil
}

// ---- store.LeaverStore: identities ----

func (s *scimCovStore) GetIdentity(_ context.Context, id uuid.UUID) (store.PrincipalIdentity, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.enter("GetIdentity"); err != nil {
		return store.PrincipalIdentity{}, err
	}
	if i, ok := s.idents[id]; ok {
		return *i, nil
	}
	return store.PrincipalIdentity{}, store.ErrNotFound
}

// hasAlias reports whether the identity holds an alias of v; kind "" matches any kind.
func (s *scimCovStore) hasAlias(id uuid.UUID, kind, v string) bool {
	return slices.Contains(s.aliases[id], scimCovAlias{kind, v}) ||
		(kind == "" && slices.ContainsFunc(s.aliases[id], func(a scimCovAlias) bool { return a.value == v }))
}

func (s *scimCovStore) SearchIdentities(_ context.Context, issuer, attr, value string, limit int) ([]store.PrincipalIdentity, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.searches = append(s.searches, scimCovSearch{issuer, attr, value, limit})
	if err := s.enter("SearchIdentities"); err != nil {
		return nil, err
	}
	v := strings.ToLower(strings.TrimSpace(value))
	var out []store.PrincipalIdentity
	for _, id := range s.order {
		i := s.idents[id]
		var hit bool
		switch attr {
		case store.SearchExternalID:
			hit = strings.ToLower(i.ScimExternalID) == v || i.ObjectID == v
		case store.SearchUserName:
			hit = strings.ToLower(i.ScimUserName) == v || s.hasAlias(id, "", v)
		case store.SearchEmail:
			hit = i.EmailLower == v || s.hasAlias(id, "email", v)
		default:
			return nil, fmt.Errorf("store: search identities: unknown attribute %q", attr)
		}
		if hit && i.Issuer == issuer {
			out = append(out, *i)
		}
	}
	return out, nil
}

func (s *scimCovStore) IdentitiesByAlias(_ context.Context, issuer, valueLower string) ([]store.PrincipalIdentity, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.enter("IdentitiesByAlias"); err != nil {
		return nil, err
	}
	var out []store.PrincipalIdentity
	for _, id := range s.order {
		if i := s.idents[id]; i.Issuer == issuer && i.ObjectID == "" && (i.EmailLower == valueLower || s.hasAlias(id, "", valueLower)) {
			out = append(out, *i)
		}
	}
	return out, nil
}

func (s *scimCovStore) IdentityAliasValues(_ context.Context, id uuid.UUID) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.enter("IdentityAliasValues"); err != nil {
		return nil, err
	}
	var out []string
	for _, a := range s.aliases[id] {
		if !slices.Contains(out, a.value) {
			out = append(out, a.value)
		}
	}
	slices.Sort(out)
	return out, nil
}

func (s *scimCovStore) applyUpdate(id uuid.UUID, u store.IdentityUpdate) (store.PrincipalIdentity, error) {
	i, ok := s.idents[id]
	if !ok {
		return store.PrincipalIdentity{}, store.ErrNotFound
	}
	if u.Reactivate && i.PurgedAt != nil {
		return store.PrincipalIdentity{}, store.ErrIdentityPurged
	}
	if u.ExternalID != "" {
		i.ScimExternalID = u.ExternalID
	}
	if name := strings.TrimSpace(u.UserName); name != "" {
		i.ScimUserName = name
		s.addAlias(id, "user_name", strings.ToLower(name))
	}
	for _, e := range u.Emails {
		if e = strings.ToLower(strings.TrimSpace(e)); e != "" {
			s.addAlias(id, "email", e)
			if i.EmailLower == "" {
				i.EmailLower = e
			}
		}
	}
	if u.Reactivate {
		for _, other := range s.idents {
			if other.PurgedAt == nil && (other.ID == id || (i.Principal != "" && other.Principal == i.Principal)) {
				other.DeactivatedAt, other.PurgeAfter = nil, nil
			}
		}
	}
	return *i, nil
}

func (s *scimCovStore) addAlias(id uuid.UUID, kind, v string) {
	if a := (scimCovAlias{kind, v}); !slices.Contains(s.aliases[id], a) {
		s.aliases[id] = append(s.aliases[id], a)
	}
}

func (s *scimCovStore) CreateScimIdentity(_ context.Context, issuer, tenantID, objectID string, u store.IdentityUpdate, _ time.Time) (store.PrincipalIdentity, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.creates = append(s.creates, scimCovCreate{issuer, tenantID, objectID, u})
	if err := s.enter("CreateScimIdentity"); err != nil {
		return store.PrincipalIdentity{}, false, err
	}
	created := true
	var id uuid.UUID
	for _, existing := range s.order {
		if i := s.idents[existing]; i.Issuer == issuer && i.TenantID == tenantID && i.ObjectID == objectID {
			id, created = existing, false
		}
	}
	if created {
		id = s.newID()
		s.idents[id] = &store.PrincipalIdentity{ID: id, Issuer: issuer, TenantID: tenantID, ObjectID: objectID, CreatedAt: scimCovNow}
		s.order = append(s.order, id)
	}
	out, err := s.applyUpdate(id, u)
	return out, created, err
}

func (s *scimCovStore) ApplyIdentityUpdate(_ context.Context, id uuid.UUID, u store.IdentityUpdate, _ time.Time) (store.PrincipalIdentity, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.updates = append(s.updates, scimCovUpdate{id, u})
	if err := s.enter("ApplyIdentityUpdate"); err != nil {
		return store.PrincipalIdentity{}, err
	}
	return s.applyUpdate(id, u)
}

func (s *scimCovStore) SuspendIdentity(_ context.Context, p store.SuspendPlan) (store.SuspendResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.plans = append(s.plans, p)
	if err := s.enter("SuspendIdentity"); err != nil {
		return store.SuspendResult{}, err
	}
	target, ok := s.idents[p.IdentityID]
	if !ok {
		return store.SuspendResult{}, store.ErrNotFound
	}
	res := store.SuspendResult{WasActive: target.DeactivatedAt == nil}
	for _, i := range s.idents {
		if i.ID != p.IdentityID && !slices.Contains(p.Principals, i.Principal) {
			continue
		}
		if i.DeactivatedAt == nil {
			t := s.now
			i.DeactivatedAt = &t
		}
		i.AuthorityEpoch++
		if i.PurgeAfter == nil && p.PurgeAfter > 0 {
			t := s.now.Add(p.PurgeAfter)
			i.PurgeAfter = &t
		}
	}
	if res.WasActive {
		s.jobs = slices.DeleteFunc(s.jobs, func(j *store.DeprovisionJob) bool {
			return j.IdentityID == p.IdentityID && j.Kind == store.JobKindSuspend
		})
	}
	cutoff := store.JobKey{Step: store.JobStepCutoff}
	detail := map[string]int{"sessions_cut": len(p.CutoffSubs)}
	if j := s.findJob(p.IdentityID, store.JobKindSuspend, cutoff); j != nil {
		j.Done, j.Attempts, j.Detail = true, j.Attempts+1, detail
	} else {
		s.jobs = append(s.jobs, &store.DeprovisionJob{IdentityID: p.IdentityID, Kind: store.JobKindSuspend, Step: cutoff.Step, Done: true, Attempts: 1, Detail: detail})
	}
	res.Epoch = target.AuthorityEpoch
	return res, nil
}

func (s *scimCovStore) PrincipalsByEmail(_ context.Context, emails []string) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.enter("PrincipalsByEmail"); err != nil {
		return nil, err
	}
	var out []string
	for _, e := range emails {
		for _, sub := range s.emailSubs[strings.ToLower(e)] {
			if !slices.Contains(out, sub) {
				out = append(out, sub)
			}
		}
	}
	return out, nil
}

func (s *scimCovStore) ListNonTerminalRunsBy(_ context.Context, createdBy string) ([]types.AgentRun, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.enter("ListNonTerminalRunsBy"); err != nil {
		return nil, err
	}
	var out []types.AgentRun
	for _, r := range s.runs {
		if r.CreatedBy == createdBy && !r.State.IsTerminal() {
			out = append(out, *r)
		}
	}
	return out, nil
}

// ---- store.LeaverStore: the ledger ----

func (s *scimCovStore) EnsureDeprovisionJobs(_ context.Context, id uuid.UUID, kind string, keys []store.JobKey) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.enter("EnsureDeprovisionJobs", kind); err != nil {
		return err
	}
	for _, k := range keys {
		if s.findJob(id, kind, k) == nil {
			s.jobs = append(s.jobs, &store.DeprovisionJob{IdentityID: id, Kind: kind, Step: k.Step, Target: k.Target})
		}
	}
	return nil
}

func (s *scimCovStore) ListDeprovisionJobs(_ context.Context, id uuid.UUID, kind string) ([]store.DeprovisionJob, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.enter("ListDeprovisionJobs", kind); err != nil {
		return nil, err
	}
	return s.listJobs(id, kind), nil
}

func (s *scimCovStore) FinishDeprovisionJob(_ context.Context, id uuid.UUID, kind string, k store.JobKey, detail map[string]int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.enter("FinishDeprovisionJob", kind); err != nil {
		return err
	}
	if j := s.findJob(id, kind, k); j != nil {
		if detail == nil {
			detail = map[string]int{}
		}
		j.Done, j.Attempts, j.LastError, j.Detail = true, j.Attempts+1, "", detail
	}
	return nil
}

func (s *scimCovStore) FailDeprovisionJob(_ context.Context, id uuid.UUID, kind string, k store.JobKey, cause error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.enter("FailDeprovisionJob", kind); err != nil {
		return err
	}
	if j := s.findJob(id, kind, k); j != nil {
		j.Attempts++
		j.LastError = cause.Error()
	}
	return nil
}

func (s *scimCovStore) ReopenDeprovisionJob(_ context.Context, id uuid.UUID, kind string, k store.JobKey) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.enter("ReopenDeprovisionJob", kind); err != nil {
		return err
	}
	if j := s.findJob(id, kind, k); j != nil {
		j.Done = false
	}
	return nil
}

// ---- store.LeaverStore: purge and status ----

func (s *scimCovStore) MarkIdentityPurged(_ context.Context, id uuid.UUID, requireDue bool) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.purgeMarks = append(s.purgeMarks, requireDue)
	if err := s.enter("MarkIdentityPurged"); err != nil {
		return false, err
	}
	i, ok := s.idents[id]
	if !ok {
		return false, store.ErrNotFound
	}
	due := i.DeactivatedAt != nil && i.PurgeAfter != nil && !i.PurgeAfter.After(s.now)
	if i.PurgedAt != nil || (requireDue && !due) {
		return false, nil
	}
	t := s.now
	i.PurgedAt = &t
	if k := (store.JobKey{Step: store.JobStepAuditDeprovision}); s.findJob(id, store.JobKindPurge, k) == nil {
		s.jobs = append(s.jobs, &store.DeprovisionJob{IdentityID: id, Kind: store.JobKindPurge, Step: k.Step})
	}
	return true, nil
}

func (s *scimCovStore) PurgeDueIdentities(_ context.Context, _ int) ([]uuid.UUID, error) {
	s.mu.Lock()
	if err := s.enter("PurgeDueIdentities"); err != nil {
		s.mu.Unlock()
		return nil, err
	}
	var out []uuid.UUID
	for _, id := range s.order {
		if i := s.idents[id]; i.PurgedAt == nil && i.DeactivatedAt != nil && i.PurgeAfter != nil && !i.PurgeAfter.After(s.now) {
			out = append(out, id)
		}
	}
	hook := s.afterDue
	s.mu.Unlock()
	if hook != nil {
		hook()
	}
	return out, nil
}

func (s *scimCovStore) PendingLeavers(_ context.Context, idleFor time.Duration, limit int) ([]store.PendingLeaver, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pendingArgs = append(s.pendingArgs, idleFor, time.Duration(limit))
	if err := s.enter("PendingLeavers"); err != nil {
		return nil, err
	}
	return slices.Clone(s.pending), nil
}

func (s *scimCovStore) DeleteUserSubjectRows(_ context.Context, _ uuid.UUID, principals, emails []string) (int64, int64, int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deleteRows = append(s.deleteRows, slices.Concat(principals, emails))
	if err := s.enter("DeleteUserSubjectRows"); err != nil {
		return 0, 0, 0, err
	}
	return s.grantsDeleted, s.assignDeleted, 0, nil
}

// HeldEmails: no other principal holds an address in this fake.
func (s *scimCovStore) HeldEmails(context.Context, uuid.UUID, []string, []string) ([]string, error) {
	return nil, nil
}

func (s *scimCovStore) ListDeactivatedIdentities(_ context.Context, _ int) ([]store.PrincipalIdentity, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.enter("ListDeactivatedIdentities"); err != nil {
		return nil, err
	}
	var out []store.PrincipalIdentity
	for _, id := range s.order {
		if i := s.idents[id]; i.DeactivatedAt != nil && i.PurgedAt == nil {
			out = append(out, *i)
		}
	}
	return out, nil
}

func (s *scimCovStore) ListDeprovisionFailures(_ context.Context, _ int) ([]store.DeprovisionFailure, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.enter("ListDeprovisionFailures"); err != nil {
		return nil, err
	}
	return slices.Clone(s.failures), nil
}

// ---- store.ScimGroupStore ----

func (s *scimCovStore) CreateScimGroup(_ context.Context, externalID, displayName string, now time.Time) (store.ScimGroup, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.enter("CreateScimGroup"); err != nil {
		return store.ScimGroup{}, err
	}
	externalID = strings.TrimSpace(externalID)
	for _, g := range s.groups {
		if strings.EqualFold(g.ExternalID, externalID) {
			return store.ScimGroup{}, store.ErrScimGroupExists
		}
	}
	g := &store.ScimGroup{ID: s.newID(), ExternalID: externalID, DisplayName: displayName, CreatedAt: now}
	s.groups[g.ID] = g
	return *g, nil
}

func (s *scimCovStore) addGroup(externalID, name string, members ...uuid.UUID) store.ScimGroup {
	s.mu.Lock()
	defer s.mu.Unlock()
	g := &store.ScimGroup{ID: s.newID(), ExternalID: externalID, DisplayName: name, CreatedAt: scimCovNow.Add(-time.Hour)}
	s.groups[g.ID] = g
	s.members[g.ID] = slices.Clone(members)
	return *g
}

func (s *scimCovStore) groupMembers(id uuid.UUID) []uuid.UUID {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.members[id])
}

func (s *scimCovStore) GetScimGroup(_ context.Context, id uuid.UUID) (store.ScimGroup, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.enter("GetScimGroup"); err != nil {
		return store.ScimGroup{}, err
	}
	if g, ok := s.groups[id]; ok {
		return *g, nil
	}
	return store.ScimGroup{}, store.ErrNotFound
}

func (s *scimCovStore) SearchScimGroups(_ context.Context, attr, value string, _ int) ([]store.ScimGroup, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.enter("SearchScimGroups"); err != nil {
		return nil, err
	}
	v := strings.ToLower(strings.TrimSpace(value))
	var out []store.ScimGroup
	for _, g := range s.groups {
		if (attr == store.SearchExternalID && strings.ToLower(strings.TrimSpace(g.ExternalID)) == v) ||
			(attr == store.SearchDisplayName && strings.ToLower(g.DisplayName) == v) {
			out = append(out, *g)
		}
	}
	return out, nil
}

func (s *scimCovStore) RenameScimGroup(_ context.Context, id uuid.UUID, displayName string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.enter("RenameScimGroup"); err != nil {
		return err
	}
	g, ok := s.groups[id]
	if !ok {
		return store.ErrNotFound
	}
	g.DisplayName = displayName
	return nil
}

func (s *scimCovStore) ScimGroupMembers(_ context.Context, id uuid.UUID) ([]uuid.UUID, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.enter("ScimGroupMembers"); err != nil {
		return nil, err
	}
	return slices.Clone(s.members[id]), nil
}

func (s *scimCovStore) AddScimGroupMembers(_ context.Context, id uuid.UUID, identities []uuid.UUID, _ time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.enter("AddScimGroupMembers"); err != nil {
		return err
	}
	for _, m := range identities {
		if _, known := s.idents[m]; known && !slices.Contains(s.members[id], m) {
			s.members[id] = append(s.members[id], m)
		}
	}
	return nil
}

func (s *scimCovStore) StartGroupRemoval(_ context.Context, groupID, identityID uuid.UUID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.enter("StartGroupRemoval"); err != nil {
		return err
	}
	target := groupID.String()
	wasMember := slices.Contains(s.members[groupID], identityID)
	s.members[groupID] = slices.DeleteFunc(s.members[groupID], func(m uuid.UUID) bool { return m == identityID })
	has := func(j *store.DeprovisionJob) bool {
		return j.IdentityID == identityID && j.Kind == store.JobKindGroupRemove && j.Target == target
	}
	existing := slices.IndexFunc(s.jobs, has) >= 0
	if wasMember && existing {
		s.jobs = slices.DeleteFunc(s.jobs, has)
		existing = false
	}
	if !existing {
		for _, step := range store.GroupRemovalSteps {
			s.jobs = append(s.jobs, &store.DeprovisionJob{IdentityID: identityID, Kind: store.JobKindGroupRemove, Step: step, Target: target})
		}
	}
	return nil
}

func (s *scimCovStore) GroupRemovalIdentities(_ context.Context, groupID uuid.UUID) ([]uuid.UUID, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.enter("GroupRemovalIdentities"); err != nil {
		return nil, err
	}
	out := slices.Clone(s.members[groupID])
	for _, j := range s.jobs {
		if j.Kind == store.JobKindGroupRemove && j.Target == groupID.String() && !j.Done && !slices.Contains(out, j.IdentityID) {
			out = append(out, j.IdentityID)
		}
	}
	return out, nil
}

func (s *scimCovStore) DeleteScimGroup(_ context.Context, id uuid.UUID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.enter("DeleteScimGroup"); err != nil {
		return err
	}
	if _, ok := s.groups[id]; !ok {
		return store.ErrNotFound
	}
	delete(s.groups, id)
	delete(s.members, id)
	return nil
}

func (s *scimCovStore) PendingGroupRemovals(context.Context, time.Duration, int) ([]store.PendingGroupRemoval, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return nil, s.enter("PendingGroupRemovals")
}

func (s *scimCovStore) GroupRemovalFailures(context.Context, int) ([]store.DeprovisionFailure, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return nil, s.enter("GroupRemovalFailures")
}

// ---- the ordinary store calls a suspension, a purge and the status read make ----

func (s *scimCovStore) GetRun(_ context.Context, id uuid.UUID) (types.AgentRun, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.enter("GetRun"); err != nil {
		return types.AgentRun{}, err
	}
	for _, r := range s.runs {
		if r.ID == id {
			return *r, nil
		}
	}
	return types.AgentRun{}, store.ErrNotFound
}

func (s *scimCovStore) UpdateRunStateIf(_ context.Context, id uuid.UUID, from, to types.RunState) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.enter("UpdateRunStateIf"); err != nil {
		return false, err
	}
	if s.casLost > 0 {
		s.casLost--
		return false, nil
	}
	for _, r := range s.runs {
		if r.ID == id && r.State == from {
			r.State = to
			return true, nil
		}
	}
	return false, nil
}

func (s *scimCovStore) ListAPITokens(context.Context) ([]types.APIToken, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.enter("ListAPITokens"); err != nil {
		return nil, err
	}
	out := make([]types.APIToken, 0, len(s.tokens))
	for _, t := range s.tokens {
		out = append(out, *t)
	}
	return out, nil
}

func (s *scimCovStore) ListAPITokensByPrincipal(_ context.Context, principal string) ([]types.APIToken, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.enter("ListAPITokensByPrincipal"); err != nil {
		return nil, err
	}
	var out []types.APIToken
	for _, t := range s.tokens {
		if t.Principal == principal {
			out = append(out, *t)
		}
	}
	return out, nil
}

func (s *scimCovStore) RevokeAPIToken(_ context.Context, id uuid.UUID, _ string, now time.Time) (types.APIToken, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.enter("RevokeAPIToken"); err != nil {
		return types.APIToken{}, err
	}
	for _, t := range s.tokens {
		if t.ID == id && t.RevokedAt == nil {
			t.RevokedAt = &now
			s.revokedToks = append(s.revokedToks, id)
			return *t, nil
		}
	}
	return types.APIToken{}, store.ErrNotFound
}

func (s *scimCovStore) ListSSHKeysByPrincipal(_ context.Context, principal string) ([]types.SSHPublicKey, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.enter("ListSSHKeysByPrincipal"); err != nil {
		return nil, err
	}
	var out []types.SSHPublicKey
	for _, k := range s.keys {
		if k.Principal == principal {
			out = append(out, k)
		}
	}
	return out, nil
}

func (s *scimCovStore) DeleteSSHKeys(_ context.Context, principal string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.enter("DeleteSSHKeys"); err != nil {
		return 0, err
	}
	s.deletedKeys = append(s.deletedKeys, principal)
	n := len(s.keys)
	s.keys = slices.DeleteFunc(s.keys, func(k types.SSHPublicKey) bool { return k.Principal == principal })
	return n - len(s.keys), nil
}

func (s *scimCovStore) ListWorkspaces(context.Context) ([]types.Workspace, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.enter("ListWorkspaces"); err != nil {
		return nil, err
	}
	return slices.Clone(s.workspaces), nil
}

func (s *scimCovStore) SetWorkspaceOwner(_ context.Context, id uuid.UUID, owner string) (types.Workspace, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ownerWrites = append(s.ownerWrites, scimCovOwnerWrite{id, owner})
	if err := s.enter("SetWorkspaceOwner"); err != nil {
		return types.Workspace{}, err
	}
	for i := range s.workspaces {
		if s.workspaces[i].ID == id {
			s.workspaces[i].OwnedBy = owner
			return s.workspaces[i], nil
		}
	}
	return types.Workspace{}, store.ErrNotFound
}

func (s *scimCovStore) ListUserDriveGrants(context.Context) ([]types.UserDriveGrant, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.enter("ListUserDriveGrants"); err != nil {
		return nil, err
	}
	return slices.Clone(s.grants), nil
}

func (s *scimCovStore) ListUserDrives(context.Context) ([]types.UserDriveListItem, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.enter("ListUserDrives"); err != nil {
		return nil, err
	}
	return slices.Clone(s.drives), nil
}

// scimCovPeople adds the one PersonStore read the SCIM match makes.
type scimCovPeople struct {
	*scimCovStore
	people map[string]types.Person
}

func (p scimCovPeople) CreatePerson(context.Context, types.Person) (types.Person, bool, error) {
	panic("scimCovPeople: not part of the SCIM routes")
}

func (p scimCovPeople) GetPerson(_ context.Context, principal string) (types.Person, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.enter("GetPerson"); err != nil {
		return types.Person{}, err
	}
	if person, ok := p.people[principal]; ok {
		return person, nil
	}
	return types.Person{}, store.ErrNotFound
}

func (p scimCovPeople) ListPeople(context.Context) ([]types.Person, error) {
	panic("scimCovPeople: not part of the SCIM routes")
}

func (p scimCovPeople) MarkPersonSignedIn(context.Context, string, time.Time) error {
	panic("scimCovPeople: not part of the SCIM routes")
}

// scimCovPaged adds the audit read the status card makes: it answers the newest-first rows of audit
// that the filter matches, and records every filter it was asked for.
type scimCovPaged struct {
	*scimCovStore
	audit   []types.AuditEvent
	filters []store.AuditFilter
	// err fails the audit read, of the one action errAction when that is set.
	err       error
	errAction string
}

func (p *scimCovPaged) QueryAuditEventsFilteredPage(_ context.Context, _ *uuid.UUID, f store.AuditFilter, _ store.Page) ([]types.AuditEvent, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, "QueryAuditEventsFilteredPage")
	p.filters = append(p.filters, f)
	if p.err != nil && (p.errAction == "" || p.errAction == f.Action) {
		return nil, p.err
	}
	var out []types.AuditEvent
	for _, ev := range p.audit {
		if f.Matches(ev) {
			out = append(out, ev)
		}
	}
	return out, nil
}

func (p *scimCovPaged) ListRunsPage(context.Context, store.Page) ([]types.AgentRun, error) {
	panic("scimCovPaged: not part of the status read")
}

func (p *scimCovPaged) ListPoliciesPage(context.Context, store.Page) ([]types.RunPolicy, error) {
	panic("scimCovPaged: not part of the status read")
}

func (p *scimCovPaged) ListWorkspacesPage(context.Context, store.Page) ([]types.Workspace, error) {
	panic("scimCovPaged: not part of the status read")
}

func (p *scimCovPaged) ListApprovalsPage(context.Context, types.ApprovalState, store.Page) ([]types.ApprovalRequest, error) {
	panic("scimCovPaged: not part of the status read")
}

func (p *scimCovPaged) QueryAuditEventsPage(context.Context, uuid.UUID, store.Page) ([]types.AuditEvent, error) {
	panic("scimCovPaged: not part of the status read")
}

func (p *scimCovPaged) QueryRecentAuditEventsPage(context.Context, store.Page) ([]types.AuditEvent, error) {
	panic("scimCovPaged: not part of the status read")
}

func (p *scimCovPaged) ListUserDriveGrantsPage(context.Context, store.Page) ([]types.UserDriveGrant, error) {
	panic("scimCovPaged: not part of the status read")
}

// scimCovRevocations is the session-revocation store: it records every cut and every revoke, and can
// refuse either.
type scimCovRevocations struct {
	mu       sync.Mutex
	revoked  []string
	cut      []string
	cutErr   error
	revokeFn error
}

func (r *scimCovRevocations) IsSessionRevoked(context.Context, string, string, time.Time) (bool, error) {
	return false, nil
}

func (r *scimCovRevocations) RevokeSub(_ context.Context, sub string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.revokeFn != nil {
		return r.revokeFn
	}
	r.revoked = append(r.revoked, sub)
	return nil
}

func (r *scimCovRevocations) CutSessions(_ context.Context, sub string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cutErr != nil {
		return r.cutErr
	}
	r.cut = append(r.cut, sub)
	return nil
}

func (r *scimCovRevocations) RevokeAll(context.Context) error { return nil }

func (r *scimCovRevocations) set(revokeErr, cutErr error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.revokeFn, r.cutErr = revokeErr, cutErr
}

func (r *scimCovRevocations) snapshot() (revoked, cut []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.revoked), slices.Clone(r.cut)
}

const (
	scimCovToken  = "scim-cov-token-0123456789abcdef0123456789abcdef"
	scimCovTenant = "11111111-2222-3333-4444-555555555555"
	scimCovIssuer = "https://login.microsoftonline.com/" + scimCovTenant + "/v2.0"
	scimCovOID    = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
)

// scimCovEnv is a server over a memory store with the SCIM routes mounted.
type scimCovEnv struct {
	t      *testing.T
	srv    *Server
	h      *harness
	st     *scimCovStore
	rev    *scimCovRevocations
	runner *scimRunner
	audit  *flakyAudit
}

// newSCIMCovEnv builds the server over st; shape edits the Config before it is built. The clock is fixed.
func newSCIMCovEnv(t *testing.T, st store.Store, shape ...func(*Config)) *scimCovEnv {
	t.Helper()
	h := newHarness(t)
	e := &scimCovEnv{t: t, h: h, rev: &scimCovRevocations{}, runner: &scimRunner{fakeRunner: &fakeRunner{}}}
	e.audit = &flakyAudit{recRecorder: h.audit}
	if cs, ok := st.(interface{ base() *scimCovStore }); ok {
		e.st = cs.base()
	}
	cfg := baseTestConfig(h, st)
	cfg.Audit = e.audit
	cfg.SessionRevocations = e.rev
	cfg.Runner = e.runner
	cfg.Now = func() time.Time { return scimCovNow }
	cfg.SCIM = &SCIMConfig{Token: scimCovToken, Issuer: scimCovIssuer, Tenant: scimCovTenant}
	for _, f := range shape {
		f(&cfg)
	}
	e.srv = New(cfg)
	return e
}

func (s *scimCovStore) base() *scimCovStore { return s }

// scim sends one SCIM request with the primary bearer.
func (e *scimCovEnv) scim(method, path, body string) *httptest.ResponseRecorder {
	e.t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+scimCovToken)
	r.Header.Set("Content-Type", scim.MediaType)
	r.RemoteAddr = "192.0.2.7:4000"
	w := httptest.NewRecorder()
	panicFails(e.t, e.srv.Handler()).ServeHTTP(w, r)
	return w
}

// auditRows are the audit rows of one action, in the order they were written.
func (e *scimCovEnv) auditRows(action string) []types.AuditEvent {
	var out []types.AuditEvent
	for _, ev := range e.h.audit.snapshot() {
		if ev.Action == action {
			out = append(out, ev)
		}
	}
	return out
}

func (e *scimCovEnv) auditData(ev types.AuditEvent) map[string]any {
	e.t.Helper()
	var d map[string]any
	if err := json.Unmarshal(ev.Data, &d); err != nil {
		e.t.Fatalf("row %s data %s: %v", ev.Action, ev.Data, err)
	}
	return d
}

// wantSCIMError asserts a SCIM error envelope: the status, the scimType and the content type.
func scimCovWantError(t *testing.T, w *httptest.ResponseRecorder, status int, scimType string) {
	t.Helper()
	if w.Code != status {
		t.Fatalf("status = %d, want %d: %s", w.Code, status, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != scim.MediaType {
		t.Errorf("content type = %q, want %q", ct, scim.MediaType)
	}
	var env struct {
		Schemas  []string `json:"schemas"`
		Status   string   `json:"status"`
		ScimType string   `json:"scimType"`
		Detail   string   `json:"detail"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("not a SCIM error body: %v: %s", err, w.Body.String())
	}
	if env.Status != fmt.Sprint(status) || env.ScimType != scimType || len(env.Schemas) != 1 || env.Schemas[0] != scim.SchemaError {
		t.Errorf("error envelope = %+v, want status %d scimType %q", env, status, scimType)
	}
}

// scimCovWantRetry asserts the retry answer of an incomplete step: a 500 envelope that names no driver error.
func scimCovWantRetry(t *testing.T, w *httptest.ResponseRecorder, leaked ...string) {
	t.Helper()
	scimCovWantError(t, w, http.StatusInternalServerError, "")
	for _, l := range leaked {
		if strings.Contains(w.Body.String(), l) {
			t.Errorf("the 500 leaked %q: %s", l, w.Body.String())
		}
	}
}

func scimCovDecode[T any](t *testing.T, w *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode %T: %v: %s", v, err, w.Body.String())
	}
	return v
}

// scimCovUser is the SCIM User as a test reads it.
type scimCovUser struct {
	ID         string       `json:"id"`
	ExternalID string       `json:"externalId"`
	UserName   string       `json:"userName"`
	Active     bool         `json:"active"`
	Emails     []scim.Email `json:"emails"`
}

var errSCIMCovBoom = errors.New("driver exploded: secret-dsn")

// scimCovPurgeAudit refuses the person.deprovision row of a purge, and only that one.
type scimCovPurgeAudit struct {
	*recRecorder
	mu   sync.Mutex
	fail bool
}

func (a *scimCovPurgeAudit) Record(ctx context.Context, ev types.AuditEvent) error {
	a.mu.Lock()
	fail := a.fail
	a.mu.Unlock()
	if fail && ev.Action == "person.deprovision" && strings.Contains(string(ev.Data), `"kind":"purge"`) {
		return errors.New("audit sink unavailable")
	}
	return a.recRecorder.Record(ctx, ev)
}

func (a *scimCovPurgeAudit) setFail(v bool) { a.mu.Lock(); a.fail = v; a.mu.Unlock() }

// scimCovSecrets is a secret store whose per-person view refuses to list once armed, which is how an
// erase of one person's credentials fails.
type scimCovSecrets struct {
	*memSecrets
	mu       sync.Mutex
	failFor  string
	failList error
}

func (s *scimCovSecrets) For(owner string) secretstore.Store {
	view := s.memSecrets.For(owner)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failList != nil && owner == s.failFor {
		return scimCovFailingView{Store: view, err: s.failList}
	}
	return view
}

func (s *scimCovSecrets) arm(owner string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failFor, s.failList = owner, err
}

type scimCovFailingView struct {
	secretstore.Store
	err error
}

func (v scimCovFailingView) List(context.Context) ([]string, error) { return nil, v.err }
