// Copyright 2025 The Wardyn Authors
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

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const (
	utPM  = "portfolio-manager"
	utDev = "developer"
)

var utKnown = []types.UserType{{ID: utPM, Name: "Portfolio manager"}, {ID: utDev, Name: "Developer"}}

// utCtx is what humanOrAdminAuth publishes for a signed-in person of one type.
func utCtx(role, userType string, groups []string) context.Context {
	return withOIDCUserType(withOIDCGroups(operatorCtx(capSub, capEmail, role), groups), userType)
}

// utResolvers asks all three capability resolvers the same question and
// reports each answer: capAllowed and capGranted (both over capScan) and
// capBatch.allowed (its own scan over capBatch.load's snapshot).
func utResolvers(t *testing.T, srv *Server, ctx context.Context, kind, value string) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	var err error
	if out["capAllowed"], err = srv.capAllowed(ctx, kind, value); err != nil {
		t.Fatalf("capAllowed: %v", err)
	}
	if out["capGranted"], err = srv.capGranted(ctx, kind, value); err != nil {
		t.Fatalf("capGranted: %v", err)
	}
	if out["capBatch.allowed"], err = srv.newCapBatch(ctx).allowed(ctx, kind, value); err != nil {
		t.Fatalf("capBatch.allowed: %v", err)
	}
	return out
}

// TestUserTypeDenyIsAWallInEveryResolver is the nonescape pin: a DENY written
// against a user type binds everyone of that type in every resolver, a user
// allow on the same value does not lift it, and a security admin of that type
// is walled too. Only isOperator is exempt. Another type's deny never binds.
func TestUserTypeDenyIsAWallInEveryResolver(t *testing.T) {
	const agent = "claude-code"
	st := &capStore{
		userTypes: utKnown,
		enf:       map[string]bool{capAgent: true},
		grants: []types.CapabilityGrant{
			grant(types.CapabilitySubjectUser, capSub, capAgent, agent, types.CapabilityAllow),
			grant(types.CapabilitySubjectUserType, utPM, capAgent, agent, types.CapabilityDeny),
		},
	}
	srv := capServer(st)
	for _, tc := range []struct {
		name     string
		role     string
		userType string
		want     bool
	}{
		{"a user of the denied type", oidc.RoleUser, utPM, false},
		{"a security admin of the denied type", oidc.RoleSecurityAdmin, utPM, false},
		{"a user of another type", oidc.RoleUser, utDev, true},
		{"a super admin of the denied type (the one exemption)", oidc.RoleAdmin, utPM, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for resolver, got := range utResolvers(t, srv, utCtx(tc.role, tc.userType, []string{}), capAgent, agent) {
				if got != tc.want {
					t.Errorf("%s = %v, want %v", resolver, got, tc.want)
				}
			}
		})
	}
}

// TestUserTypeAllowGrantsOnlyThatType: on an enforced kind a type allow is what
// lets its people through — narrowing (capAllowed, capBatch) and widening
// (capGranted, the image kind) alike — and only that type's people.
func TestUserTypeAllowGrantsOnlyThatType(t *testing.T) {
	const img = "ghcr.io/corp/pm-tools:1"
	st := &capStore{
		userTypes: utKnown,
		enf:       map[string]bool{capImage: true},
		grants:    []types.CapabilityGrant{grant(types.CapabilitySubjectUserType, utPM, capImage, img, types.CapabilityAllow)},
	}
	srv := capServer(st)
	for userType, want := range map[string]bool{utPM: true, utDev: false, types.UserTypeStandard: false} {
		for resolver, got := range utResolvers(t, srv, utCtx(oidc.RoleUser, userType, []string{}), capImage, img) {
			if got != want {
				t.Errorf("%s for %s = %v, want %v", resolver, userType, got, want)
			}
		}
	}
}

// TestUntypedHumanResolvesAsTheBuiltInType: an API token carries no type yet,
// so it answers as Standard user — a deny on the built-in type binds it —
// while a caller with no human at all (admin token, local mode) has no type.
func TestUntypedHumanResolvesAsTheBuiltInType(t *testing.T) {
	st := &capStore{grants: []types.CapabilityGrant{
		grant(types.CapabilitySubjectUserType, types.UserTypeStandard, capAgent, "claude-code", types.CapabilityDeny),
	}}
	srv := capServer(st)
	token := withHumanIdentity(context.Background(), capSub, capEmail, oidc.RoleUser, "", []string{}, false)
	subj, err := srv.callerSubjects(token)
	if err != nil || subj.userType != types.UserTypeStandard {
		t.Fatalf("callerSubjects(untyped human) = %q, %v; want %q", subj.userType, err, types.UserTypeStandard)
	}
	for resolver, got := range utResolvers(t, srv, token, capAgent, "claude-code") {
		if got {
			t.Errorf("%s let an untyped token past a Standard user deny", resolver)
		}
	}
	if subj, err := srv.callerSubjects(context.Background()); err != nil || subj.userType != "" {
		t.Fatalf("callerSubjects(no human) = %q, %v; want no type", subj.userType, err)
	}
}

// TestUnknownUserTypeRefusesEveryResolver: a stamped type whose row is gone
// refuses every control that names a type — the three capability resolvers,
// the ceiling and the drive — with errUserTypeUnknown, never resolving
// without the type. The refusal is audited once per resolve, and not on a
// display read.
func TestUnknownUserTypeRefusesEveryResolver(t *testing.T) {
	st := &capStore{userTypes: utKnown}
	rec := &recRecorder{}
	srv := &Server{cfg: Config{Store: st, Audit: rec, Now: time.Now, DefaultPolicy: govDeployment()}}
	ctx := utCtx(oidc.RoleUser, "contractor", []string{})

	calls := map[string]func() error{
		"capAllowed": func() error { _, err := srv.capAllowed(ctx, capAgent, "claude-code"); return err },
		"capGranted": func() error { _, err := srv.capGranted(ctx, capAgent, "claude-code"); return err },
		"capBatch.allowed": func() error {
			_, err := srv.newCapBatch(ctx).allowed(ctx, capAgent, "claude-code")
			return err
		},
		"effectiveCeiling": func() error { _, err := srv.effectiveCeiling(ctx); return err },
		"resolveUserDrive": func() error { _, err := srv.resolveUserDrive(ctx, 0); return err },
	}
	st.enf = map[string]bool{capAgent: true} // capGranted reaches the scan only on an enforced kind
	for name, call := range calls {
		if err := call(); !errors.Is(err, errUserTypeUnknown) {
			t.Errorf("%s = %v, want errUserTypeUnknown", name, err)
		}
	}
	if denials := userTypeUnknownDenials(rec); denials != len(calls) {
		t.Errorf("authz.denied user_type_unknown rows = %d, want %d (one per resolve outside a request)", denials, len(calls))
	}
	if _, err := srv.effectiveCeiling(withDisplayRead(ctx)); !errors.Is(err, errUserTypeUnknown) {
		t.Fatalf("display read = %v, want errUserTypeUnknown", err)
	}
	if n := len(rec.snapshot()); n != len(calls) {
		t.Errorf("a display read wrote an audit row (%d rows, want %d)", n, len(calls))
	}
}

func userTypeUnknownDenials(rec *recRecorder) int {
	n := 0
	for _, ev := range rec.snapshot() {
		if ev.Action == "authz.denied" && strings.Contains(string(ev.Data), `"reason":"user_type_unknown"`) {
			n++
		}
	}
	return n
}

// TestUnknownUserTypeIsOneDenialPerRequest: one request that asks every
// resolver (as a POST /runs does) is refused by each, and writes one
// authz.denied row, like its groups_snapshot_stale twin.
func TestUnknownUserTypeIsOneDenialPerRequest(t *testing.T) {
	st := &capStore{userTypes: utKnown, enf: map[string]bool{capAgent: true}}
	rec := &recRecorder{}
	srv := &Server{cfg: Config{Store: st, Audit: rec, Now: time.Now, DefaultPolicy: govDeployment()}}

	var errs []error
	h := ceilingMemoMiddleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		_, batchErr := srv.newCapBatch(ctx).allowed(ctx, capAgent, "claude-code")
		_, grantErr := srv.capGranted(ctx, capAgent, "claude-code")
		_, ceilingErr := srv.effectiveCeiling(ctx)
		_, driveErr := srv.resolveUserDrive(ctx, 0)
		errs = []error{batchErr, grantErr, ceilingErr, driveErr}
	}))
	req := httptest.NewRequest(http.MethodPost, "/api/v1/runs", nil).
		WithContext(utCtx(oidc.RoleUser, "contractor", []string{}))
	h.ServeHTTP(httptest.NewRecorder(), req)

	for i, err := range errs {
		if !errors.Is(err, errUserTypeUnknown) {
			t.Errorf("resolve %d = %v, want errUserTypeUnknown", i, err)
		}
	}
	if n := userTypeUnknownDenials(rec); n != 1 {
		t.Errorf("authz.denied user_type_unknown rows = %d, want 1 per request", n)
	}
}

// utResolveStore records the user type the ceiling and drive resolvers are
// asked with.
type utResolveStore struct {
	*capStore
	govType, driveType string
}

func (s *utResolveStore) ResolveGovernanceProfile(ctx context.Context, users, groups []string, userType string) (*types.GovernanceProfile, types.CapabilitySubjectType, error) {
	s.govType = userType
	return s.capStore.ResolveGovernanceProfile(ctx, users, groups, userType)
}

func (s *utResolveStore) ResolveUserDrive(ctx context.Context, users, groups []string, userType string) (*types.UserDrive, *types.UserDriveGrant, types.CapabilitySubjectType, error) {
	s.driveType = userType
	return s.capStore.ResolveUserDrive(ctx, users, groups, userType)
}

// TestCeilingAndDriveResolveOnTheCallerType: both tiered resolvers are asked
// with the caller's stamped type, on the answerable path and on the
// stale-snapshot one.
func TestCeilingAndDriveResolveOnTheCallerType(t *testing.T) {
	for name, groups := range map[string][]string{"answerable snapshot": {"eng"}, "stale snapshot": nil} {
		t.Run(name, func(t *testing.T) {
			st := &utResolveStore{capStore: &capStore{userTypes: utKnown}}
			srv := &Server{cfg: Config{Store: st, DefaultPolicy: govDeployment()}}
			ctx := utCtx(oidc.RoleUser, utPM, groups)
			if _, err := srv.effectiveCeiling(ctx); err != nil {
				t.Fatalf("effectiveCeiling: %v", err)
			}
			if _, err := srv.resolveUserDrive(ctx, 0); err != nil {
				t.Fatalf("resolveUserDrive: %v", err)
			}
			if st.govType != utPM || st.driveType != utPM {
				t.Fatalf("resolved on type %q (ceiling) / %q (drive), want %q", st.govType, st.driveType, utPM)
			}
		})
	}
}

// TestStaleSnapshotDoesNotTrustATypeTierCeiling: the type tier sits BELOW
// group, so with the group snapshot unanswerable a type-tier winner could have
// been outranked by a group row, exactly like an all-tier one. It is refused
// while group-tier assignments exist, and served when none do.
func TestStaleSnapshotDoesNotTrustATypeTierCeiling(t *testing.T) {
	for _, hasGroupTier := range []bool{true, false} {
		st := &capStore{
			userTypes:       utKnown,
			govProfile:      govProfile("pm-profile"),
			govTier:         types.CapabilitySubjectUserType,
			govHasGroupTier: hasGroupTier,
		}
		ceiling, err := govServer(st).effectiveCeiling(utCtx(oidc.RoleUser, utPM, nil))
		switch {
		case hasGroupTier && !errors.Is(err, errGroupsSnapshotStale):
			t.Errorf("group tier exists: err = %v, want errGroupsSnapshotStale", err)
		case !hasGroupTier && (err != nil || ceiling.Profile == nil || ceiling.Profile.Name != "pm-profile"):
			t.Errorf("no group tier: ceiling = %+v, %v; want the type's profile", ceiling.Profile, err)
		}
	}
}

// TestStaleSnapshotDoesNotTrustATypeTierDrive is
// TestStaleSnapshotDoesNotTrustATypeTierCeiling's drive twin: driveWithUnusable
// Groups' trusted shapes are a USER-tier winner or no group-tier grant at all,
// so a TYPE-tier winner must be refused (errGroupsSnapshotStale) while a
// group-tier drive grant exists, and served (the type's drive) when none do —
// pinning the tier == CapabilitySubjectUser check at user_drives_resolve.go
// against being loosened to also trust a type-tier winner.
func TestStaleSnapshotDoesNotTrustATypeTierDrive(t *testing.T) {
	d := driveFixture(nil)
	g := grantFixture(d.ID, func(g *types.UserDriveGrant) {
		g.SubjectType = types.CapabilitySubjectUserType
		g.Subject = utPM
	})
	for _, hasGroupTier := range []bool{true, false} {
		st := &capStore{
			userTypes:         utKnown,
			drive:             d,
			driveGrant:        g,
			driveTier:         types.CapabilitySubjectUserType,
			driveHasGroupTier: hasGroupTier,
		}
		resolved, err := govServer(st).resolveUserDrive(utCtx(oidc.RoleUser, utPM, nil), 0)
		switch {
		case hasGroupTier && !errors.Is(err, errGroupsSnapshotStale):
			t.Errorf("group tier exists: err = %v, want errGroupsSnapshotStale", err)
		case !hasGroupTier && (err != nil || resolved == nil || resolved.Drive.ID != d.ID):
			t.Errorf("no group tier: resolved = %+v, %v; want the type's drive", resolved, err)
		}
	}
}

// TestUnknownUserTypeSessionIsRefusedThroughTheRouter: the SSO lane publishes
// the session's type, and a deleted one is a 403 with the capitalised
// sentence, not a 500.
func TestUnknownUserTypeSessionIsRefusedThroughTheRouter(t *testing.T) {
	srv, st := permServer(t)
	st.userTypes = utKnown
	st.grants = []types.CapabilityGrant{grant(types.CapabilitySubjectUserType, utPM, capAgent, "claude-code", types.CapabilityDeny)}

	w := doSSO(t, srv, http.MethodGet, "/api/v1/me/capabilities",
		ssoSessionOfType(t, "sub-ut-carol", "carol@corp.example", oidc.RoleUser, "contractor"), "")
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), userTypeUnknownMsg) {
		t.Fatalf("deleted type = %d %s, want 403 with %q", w.Code, w.Body.String(), userTypeUnknownMsg)
	}

	// GET /me still answers, and names the state rather than "no drive".
	w = doSSO(t, srv, http.MethodGet, "/api/v1/me",
		ssoSessionOfType(t, "sub-ut-carol", "carol@corp.example", oidc.RoleUser, "contractor"), "")
	var me struct {
		UserDriveUnavailable string `json:"user_drive_unavailable"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &me); err != nil || w.Code != http.StatusOK || me.UserDriveUnavailable != driveUnavailableUserType {
		t.Fatalf("/me = %d %s, want 200 with user_drive_unavailable %q", w.Code, w.Body.String(), driveUnavailableUserType)
	}

	w = doSSO(t, srv, http.MethodGet, "/api/v1/me/capabilities",
		ssoSessionOfType(t, "sub-ut-carol", "carol@corp.example", oidc.RoleUser, utPM), "")
	if w.Code != http.StatusOK {
		t.Fatalf("known type = %d %s, want 200", w.Code, w.Body.String())
	}
	var body meCapabilitiesResponse
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Grants) != 1 || body.Grants[0].Subject != utPM {
		t.Fatalf("grants = %+v, want the type's own row", body.Grants)
	}
}

// ─── the write boundary ─────────────────────────────────────────────────────

// TestValidatorsKeepAUserTypeSubjectVerbatim: all three validators accept a
// well-formed type id unfolded, and the two that can see the id rule refuse a
// malformed one.
func TestValidatorsKeepAUserTypeSubjectVerbatim(t *testing.T) {
	g := types.CapabilityGrant{SubjectType: types.CapabilitySubjectUserType, Subject: "  " + utPM + " ",
		Capability: capAgent, Value: "claude-code", Effect: types.CapabilityDeny}
	if err := validateCapabilityGrant(&g); err != nil || g.Subject != utPM {
		t.Fatalf("grant = %q, %v", g.Subject, err)
	}
	a := types.GovernanceAssignment{SubjectType: types.CapabilitySubjectUserType, Subject: utPM, ProfileID: uuid.New()}
	if err := validateGovernanceAssignment(&a); err != nil || a.Subject != utPM {
		t.Fatalf("assignment = %q, %v", a.Subject, err)
	}
	d := types.UserDriveGrant{SubjectType: types.CapabilitySubjectUserType, Subject: utPM, DriveID: uuid.New()}
	if err := types.ValidateUserDriveGrant(&d); err != nil || d.Subject != utPM {
		t.Fatalf("drive grant = %q, %v", d.Subject, err)
	}
	for _, bad := range []string{"Portfolio Manager", "PM", "pm_team"} {
		g := types.CapabilityGrant{SubjectType: types.CapabilitySubjectUserType, Subject: bad,
			Capability: capAgent, Value: "claude-code", Effect: types.CapabilityDeny}
		if err := validateCapabilityGrant(&g); err == nil {
			t.Errorf("grant accepted malformed type id %q", bad)
		}
		a := types.GovernanceAssignment{SubjectType: types.CapabilitySubjectUserType, Subject: bad, ProfileID: uuid.New()}
		if err := validateGovernanceAssignment(&a); err == nil {
			t.Errorf("assignment accepted malformed type id %q", bad)
		}
	}
}

// TestGrantWriteRefusesAUserTypeThatDoesNotExist: a row naming a missing type
// is a 400 with the People page's sentence, never stored.
func TestGrantWriteRefusesAUserTypeThatDoesNotExist(t *testing.T) {
	srv, st := permServer(t)
	st.userTypes = utKnown
	admin := permAdmin(t)
	post := func(subject string) int {
		return doSSO(t, srv, http.MethodPost, "/api/v1/permissions/grants", admin,
			`{"subject_type":"user_type","subject":"`+subject+`","capability":"agent","value":"claude-code","effect":"deny"}`).Code
	}
	if code := post("contractor"); code != http.StatusBadRequest {
		t.Fatalf("missing type = %d, want 400", code)
	}
	if len(st.grants) != 0 {
		t.Fatalf("a row naming a missing type was stored: %+v", st.grants)
	}
	for _, id := range []string{utPM, types.UserTypeStandard} {
		if code := post(id); code != http.StatusCreated {
			t.Fatalf("existing type %s = %d, want 201", id, code)
		}
	}
}

// utWriteStore is the assignment write the governance handler makes, over a
// capStore that knows utKnown.
type utWriteStore struct {
	*capStore
	saved []types.GovernanceAssignment
}

func (s *utWriteStore) UpsertGovernanceAssignment(_ context.Context, a types.GovernanceAssignment) (types.GovernanceAssignment, error) {
	s.saved = append(s.saved, a)
	return a, nil
}

func TestAssignmentWriteRefusesAUserTypeThatDoesNotExist(t *testing.T) {
	st := &utWriteStore{capStore: &capStore{userTypes: utKnown}}
	srv := New(Config{Store: st, Audit: &recRecorder{}, LocalMode: true})
	post := func(subject string) int {
		return driveCall(t, srv.handleUpsertGovernanceAssignment, http.MethodPost, "/api/v1/governance/assignments",
			`{"subject_type":"user_type","subject":"`+subject+`","profile_id":"`+uuid.NewString()+`"}`, nil).Code
	}
	if code := post("contractor"); code != http.StatusBadRequest || len(st.saved) != 0 {
		t.Fatalf("missing type = %d (saved %d), want 400 and nothing saved", code, len(st.saved))
	}
	if code := post(utPM); code != http.StatusCreated {
		t.Fatalf("existing type = %d, want 201", code)
	}
}

// utDriveCRUDStore is driveCRUDStore that also knows utKnown.
type utDriveCRUDStore struct{ *driveCRUDStore }

func (utDriveCRUDStore) GetUserType(_ context.Context, id string) (types.UserType, error) {
	for _, t := range utKnown {
		if t.ID == id {
			return t, nil
		}
	}
	return seededUserType(id)
}

func TestDriveGrantWriteRefusesAUserTypeThatDoesNotExist(t *testing.T) {
	st := newDriveCRUDStore()
	d := *driveFixture(nil)
	st.drives[d.ID] = d
	srv := New(Config{Store: utDriveCRUDStore{st}, Audit: &recRecorder{}, RunnerTarget: "docker", LocalMode: true})
	post := func(subject string) int {
		return driveCall(t, srv.handleUpsertUserDriveGrant, http.MethodPost, "/api/v1/drives/grants",
			`{"subject_type":"user_type","subject":"`+subject+`","drive_id":"`+d.ID.String()+`"}`, nil).Code
	}
	if code := post("contractor"); code != http.StatusBadRequest || len(st.grants) != 0 {
		t.Fatalf("missing type = %d (stored %d), want 400 and nothing stored", code, len(st.grants))
	}
	if code := post(utPM); code != http.StatusCreated {
		t.Fatalf("existing type = %d, want 201", code)
	}
}

var _ store.Store = utDriveCRUDStore{}

// TestUnknownUserTypeRefusalRecordsTheTypeID is C's proof: now that
// callerSubjects no longer stamps the row itself ("user_type" is a reserved
// Datum key; Principal.UserType alone fills it, from refusal.go's
// oidc.UserTypeFromContext), a real SSO request whose stamped type has been
// deleted still names it on the authz.denied row. Unlike utCtx's synthetic
// context, this drives a signed session cookie through the real oidc
// middleware, so oidc.UserTypeFromContext is the one Middleware itself sets.
func TestUnknownUserTypeRefusalRecordsTheTypeID(t *testing.T) {
	h := newHarness(t)
	st := &capStore{userTypes: utKnown, enf: map[string]bool{capAgent: true}}
	cfg := baseTestConfig(h, st)
	cfg.OIDC = &oidc.Authenticator{}
	cfg.DefaultPolicy = types.RunPolicySpec{MinConfinementClass: types.CC2, AllowedDomains: []string{"api.anthropic.com"}}
	srv := New(cfg)

	const gone = "contractor" // utKnown holds utPM/utDev only
	member := ssoSessionOfType(t, "sub-utid-gone", "utid-gone@corp.example", oidc.RoleUser, gone)
	doSSO(t, srv, http.MethodPost, "/api/v1/runs/preflight", member, `{"agent":"claude-code","task":"t"}`)

	for _, ev := range h.audit.snapshot() {
		if ev.Action != "authz.denied" {
			continue
		}
		var data map[string]any
		if err := json.Unmarshal(ev.Data, &data); err != nil {
			t.Fatalf("unmarshal audit data: %v", err)
		}
		if data["reason"] == "user_type_unknown" {
			if data["user_type"] != gone {
				t.Errorf("user_type_unknown row user_type = %v, want %q", data["user_type"], gone)
			}
			return
		}
	}
	t.Fatal("no authz.denied user_type_unknown row was recorded")
}
