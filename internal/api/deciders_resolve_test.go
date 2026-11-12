// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The decide-path pins of unit deciders: who may decide a hold when a unit's rule governs its kind. They run
// on a fake store: dc-root with dc-team below it, the run launched in dc-team by dcOwner. dcAdmin administers
// dc-root and dcAdmin2 dc-team by user grants; dcLead and dcLead2 are in the group dc-leads; dcOther is a
// member with no grant and no group; dcSec and dcSec2 are security admins.
const (
	dcOwner  = "sub-dc-owner"
	dcAdmin  = "sub-dc-admin"
	dcAdmin2 = "sub-dc-admin-2"
	dcLead   = "sub-dc-lead"
	dcLead2  = "sub-dc-lead-2"
	dcOther  = "sub-dc-other"
	dcSec    = "sub-dc-sec"
	dcSec2   = "sub-dc-sec-2"
	dcGroup  = "dc-leads"
	dcHost   = "registry.npmjs.org"
)

// dcRuleKinds is every kind a rule names that a run hold carries (elevation is not a run hold).
var dcRuleKinds = []string{
	types.DeciderKindEgressDomain, types.DeciderKindADOEscalation, types.DeciderKindMCPCall,
	types.DeciderKindCredential, types.DeciderKindPushContent,
}

// decidersStore is authzStore plus a unit tree, unit-admin grants, egress_host grants and each unit's deciders.
type decidersStore struct {
	*authzStore
	ua       *fakeUnitAdminStore
	mu       sync.Mutex
	units    []types.GovernanceUnit
	unitsErr error
	rules    map[uuid.UUID][]types.DeciderRule
	rulesErr error
	grants   []types.CapabilityGrant
	enf      map[string]bool
}

var (
	_ store.UnitDeciderStore = (*decidersStore)(nil)
	_ store.UnitAdminStore   = (*decidersStore)(nil)
)

func (s *decidersStore) ListUnitDeciders(context.Context) ([]types.UnitDeciders, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.rulesErr != nil {
		return nil, s.rulesErr
	}
	var out []types.UnitDeciders
	for unit, rules := range s.rules {
		out = append(out, types.UnitDeciders{UnitID: unit, Rules: rules})
	}
	return out, nil
}

func (s *decidersStore) ListGovernanceUnits(context.Context) ([]types.GovernanceUnit, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.units), s.unitsErr
}

func (s *decidersStore) ListUnitAdmins(ctx context.Context) ([]types.GovernanceUnitAdmin, error) {
	return s.ua.ListUnitAdmins(ctx)
}

func (s *decidersStore) ListUnitAdminOf(ctx context.Context, users, groups []string) ([]types.GovernanceUnitAdmin, error) {
	return s.ua.ListUnitAdminOf(ctx, users, groups)
}

func (s *decidersStore) UnitScope(ctx context.Context, unit uuid.UUID, users, groups []string, strict bool) (bool, error) {
	return s.ua.UnitScope(ctx, unit, users, groups, strict)
}

func (s *decidersStore) GetUnitAdmin(ctx context.Context, unit, id uuid.UUID) (types.GovernanceUnitAdmin, error) {
	return s.ua.GetUnitAdmin(ctx, unit, id)
}

func (s *decidersStore) ListCapabilityGrantsFor(ctx context.Context, users, groups []string, userType string) ([]types.CapabilityGrant, error) {
	return (&mayDecideStore{grants: s.grants}).ListCapabilityGrantsFor(ctx, users, groups, userType)
}

func (s *decidersStore) GetCapabilityEnforcement(context.Context) (map[string]bool, error) {
	return s.enf, nil
}

// setRules replaces unit's rules; none clears them.
func (s *decidersStore) setRules(unit uuid.UUID, rules ...types.DeciderRule) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(rules) == 0 {
		delete(s.rules, unit)
		return
	}
	s.rules[unit] = rules
}

// revokeGrants removes every unit-admin grant naming subject.
func (s *decidersStore) revokeGrants(subject string) {
	s.ua.mu.Lock()
	defer s.ua.mu.Unlock()
	s.ua.grants = slices.DeleteFunc(s.ua.grants, func(g types.GovernanceUnitAdmin) bool { return g.Subject == subject })
}

// decidersSvcStore is decidersStore that also answers one service identity, for a service identity's run.
type decidersSvcStore struct {
	*decidersStore
	store.ServiceIdentityStore // only GetServiceIdentity is asked
	svc                        types.ServiceIdentity
}

func (s *decidersSvcStore) GetServiceIdentity(_ context.Context, name string) (types.ServiceIdentity, error) {
	if name != s.svc.Name {
		return types.ServiceIdentity{}, store.ErrNotFound
	}
	return s.svc, nil
}

// dcFix is one server over a decidersStore, with the run it holds approvals for.
type dcFix struct {
	t          *testing.T
	h          *harness
	srv        *Server
	st         *decidersStore
	aps        *ruleApprovals
	root, team types.GovernanceUnit
	run        types.AgentRun
	// svc, when set before the server is built, is the service identity the store answers.
	svc *types.ServiceIdentity
}

func newDCFix(t *testing.T, configure ...func(*dcFix)) *dcFix {
	t.Helper()
	h := newHarness(t)
	ast := newAuthzStore()
	root := types.GovernanceUnit{ID: uuid.New(), Name: "dc-root"}
	team := types.GovernanceUnit{ID: uuid.New(), Name: "dc-team", ParentID: &root.ID}
	f := &dcFix{t: t, h: h, root: root, team: team,
		st: &decidersStore{authzStore: ast, units: []types.GovernanceUnit{root, team}, rules: map[uuid.UUID][]types.DeciderRule{},
			ua: &fakeUnitAdminStore{parents: map[uuid.UUID]uuid.UUID{team.ID: root.ID}, grants: []types.GovernanceUnitAdmin{
				{ID: uuid.New(), UnitID: root.ID, SubjectType: types.CapabilitySubjectUser, Subject: dcAdmin, GrantedBy: "seed"},
				{ID: uuid.New(), UnitID: team.ID, SubjectType: types.CapabilitySubjectUser, Subject: dcAdmin2, GrantedBy: "seed"},
			}}},
		aps: &ruleApprovals{authzApprovals: newAuthzApprovals(ast), rec: h.audit},
	}
	f.run = types.AgentRun{ID: uuid.New(), CreatedBy: dcOwner, State: types.RunRunning, UnitID: &team.ID}
	for _, c := range configure {
		c(f)
	}
	ast.mu.Lock()
	ast.runs[f.run.ID] = f.run
	ast.mu.Unlock()
	var st store.Store = f.st
	if f.svc != nil {
		st = &decidersSvcStore{decidersStore: f.st, svc: *f.svc}
	}
	cfg := baseTestConfig(h, st)
	cfg.OIDC = &oidc.Authenticator{}
	cfg.Approvals = f.aps
	f.srv = New(cfg)
	return f
}

// createdBy sets the run's creator before the server is built.
func createdBy(who string) func(*dcFix) { return func(f *dcFix) { f.run.CreatedBy = who } }

// dcServiceRun makes the run a service identity's (service:ci), sponsored by sponsor, with author as its
// recorded human author.
func dcServiceRun(sponsor, author string) func(*dcFix) {
	return func(f *dcFix) {
		f.svc = &types.ServiceIdentity{Name: "ci", Sponsor: sponsor, UserType: types.UserTypeStandard}
		f.run.CreatedBy, f.run.HumanAuthor = types.ServicePrincipalPrefix+"ci", author
	}
}

// dcRule is one decider rule for kind; credential and push_content rules state second_human, as they must.
func dcRule(kind string, who ...string) types.DeciderRule {
	if who == nil {
		who = []string{}
	}
	r := types.DeciderRule{Kinds: []string{kind}, Who: who}
	r.SecondHuman = kind == types.DeciderKindCredential || kind == types.DeciderKindPushContent
	return r
}

// hold seeds a PENDING request of the decider kind on the run; any other kind is a plain tool_call no rule names.
func (f *dcFix) hold(kind string) types.ApprovalRequest {
	f.t.Helper()
	ap := types.ApprovalRequest{ID: uuid.New(), RunID: f.run.ID, State: types.ApprovalPending, RequestedAt: time.Now().UTC()}
	switch kind {
	case types.DeciderKindEgressDomain:
		ap.Kind, ap.RequestedScope = types.ApprovalEgressDomain, json.RawMessage(`{"host":"`+dcHost+`"}`)
	case types.DeciderKindADOEscalation:
		g := uuid.New()
		ap.Kind, ap.GrantID = types.ApprovalToolCall, &g
		ap.RequestedScope = json.RawMessage(`{"lane":"azure_devops","grant_id":"` + g.String() + `","capability":"pr_create"}`)
	case types.DeciderKindMCPCall:
		ap.Kind, ap.RequestedScope = types.ApprovalToolCall, mcpCallScopeJSON(f.t, "jira", "delete_issue", `{}`)
	case types.DeciderKindCredential:
		ap.Kind, ap.RequestedScope = types.ApprovalCredential, json.RawMessage(`{}`)
	case types.DeciderKindPushContent:
		ap.Kind, ap.RequestedScope = types.ApprovalPushContent, json.RawMessage(`{}`)
	default:
		ap.Kind, ap.RequestedScope = types.ApprovalToolCall, json.RawMessage(`{"tool":"Bash","cmd":"ls"}`)
	}
	f.aps.mu.Lock()
	f.aps.byID[ap.ID] = ap
	f.aps.mu.Unlock()
	return ap
}

// row is the approval as stored now.
func (f *dcFix) row(ap types.ApprovalRequest) types.ApprovalRequest {
	f.t.Helper()
	got, err := f.aps.Get(context.Background(), ap.ID)
	if err != nil {
		f.t.Fatal(err)
	}
	return got
}

// decide posts verb on ap as c (nil: the admin token), with body or a bare reason.
func (f *dcFix) decide(c *http.Cookie, ap types.ApprovalRequest, verb, body string) *httptest.ResponseRecorder {
	f.t.Helper()
	if body == "" {
		body = `{"reason":"t"}`
	}
	path := "/api/v1/approvals/" + ap.ID.String() + "/" + verb
	if c == nil {
		return do(f.t, f.srv, http.MethodPost, path, adminToken, body)
	}
	return doSSO(f.t, f.srv, http.MethodPost, path, c, body)
}

// dcMember is a member's session with a fresh group snapshot.
func dcMember(t *testing.T, sub string, groups ...string) *http.Cookie {
	t.Helper()
	if groups == nil {
		groups = []string{}
	}
	return uaSession(t, sub, groups, false)
}

func dcSecurity(t *testing.T, sub string) *http.Cookie {
	t.Helper()
	return ssoSession(t, sub, sub+"@corp.example", oidc.RoleSecurityAdmin)
}

// dcCtx is the request context a session cookie gives mayDecide.
func dcCtx(sub, role string, groups []string, truncated bool) context.Context {
	return withHumanIdentity(context.Background(), sub, sub+"@corp.example", role, types.UserTypeStandard, groups, truncated)
}

// mayDecideAs is mayDecide's verdict on ap for ctx.
func (f *dcFix) mayDecideAs(ctx context.Context, ap types.ApprovalRequest) bool {
	req := httptest.NewRequest(http.MethodPost, "/api/v1/approvals/"+ap.ID.String()+"/deny", nil).WithContext(ctx)
	return f.srv.mayDecide(req, ap, f.run)
}

// wantStatus fails unless w answered code (and reason, when one is named) and ap stayed as want says.
func (f *dcFix) wantStatus(name string, w *httptest.ResponseRecorder, code int, reason string, ap types.ApprovalRequest) {
	f.t.Helper()
	if w.Code != code || (reason != "" && wireReason(f.t, w) != reason) {
		f.t.Errorf("%s: %d %s, want %d %s", name, w.Code, strings.TrimSpace(w.Body.String()), code, reason)
	}
	decided := f.row(ap).State != types.ApprovalPending
	if decided != (code == http.StatusOK) {
		f.t.Errorf("%s: state %s after %d", name, f.row(ap).State, w.Code)
	}
}

// TestDecidersNonOwnerTypesNeverMatchAuthor: unit_admins and group deciders never match the run's own people,
// whatever second_human says: its creator, the sponsor of the service identity that created it, or its
// recorded human author. Those people decide only through owner. Another person of the same decider type
// decides. One case per kind x {unit_admins, group}.
func TestDecidersNonOwnerTypesNeverMatchAuthor(t *testing.T) {
	for _, kind := range dcRuleKinds {
		for _, tc := range []struct {
			typ           string
			author, other string
			groups        []string
		}{
			{types.DeciderUnitAdmins, dcAdmin, dcAdmin2, nil},
			{types.DeciderGroupPrefix + dcGroup, dcLead, dcLead2, []string{dcGroup}},
		} {
			t.Run(kind+"/"+tc.typ, func(t *testing.T) {
				for _, people := range []struct {
					name string
					run  func(*dcFix)
				}{
					{"creator", createdBy(tc.author)},
					{"sponsor", dcServiceRun(tc.author, "sub-dc-someone")},
					{"human author", dcServiceRun("sub-dc-someone", tc.author)},
				} {
					f := newDCFix(t, people.run)
					f.st.setRules(f.root.ID, dcRule(kind, tc.typ))
					ap := f.hold(kind)
					if f.mayDecideAs(dcCtx(tc.author, oidc.RoleUser, tc.groups, false), ap) {
						t.Errorf("%s: mayDecide = true for the run's own person", people.name)
					}
					if w := f.decide(dcMember(t, tc.author, tc.groups...), ap, "deny", ""); w.Code == http.StatusOK {
						t.Errorf("%s: the run's own person decided it: %d %s", people.name, w.Code, w.Body)
					}
					if f.row(ap).State != types.ApprovalPending {
						t.Fatalf("%s: state %s, want PENDING", people.name, f.row(ap).State)
					}
					w := f.decide(dcMember(t, tc.other, tc.groups...), ap, "deny", "")
					f.wantStatus(people.name+": another "+tc.typ, w, http.StatusOK, "", ap)
				}
			})
		}
	}
}

// TestDecidersChainUnreadableRefuses: a rule or unit-tree read that fails is never read as "no rule". The run's
// owner is told (503 deciders_unreadable); anyone else gets the plain 404; a security admin still decides.
func TestDecidersChainUnreadableRefuses(t *testing.T) {
	for name, broken := range map[string]func(*decidersStore){
		"rules":     func(s *decidersStore) { s.rulesErr = errors.New("deciders read failed") },
		"unit tree": func(s *decidersStore) { s.unitsErr = errors.New("units read failed") },
	} {
		t.Run(name, func(t *testing.T) {
			f := newDCFix(t)
			f.st.setRules(f.team.ID, dcRule(types.DeciderKindEgressDomain, types.DeciderOwner, types.DeciderUnitAdmins))
			broken(f.st)
			ap := f.hold(types.DeciderKindEgressDomain)
			f.wantStatus("owner", f.decide(dcMember(t, dcOwner), ap, "deny", ""), http.StatusServiceUnavailable, "deciders_unreadable", ap)
			if f.mayDecideAs(dcCtx(dcOwner, oidc.RoleUser, []string{}, false), ap) {
				t.Error("mayDecide = true for the owner with the rules unreadable")
			}
			f.wantStatus("unit admin", f.decide(dcMember(t, dcAdmin), ap, "deny", ""), http.StatusNotFound, reasonApprovalNotFound, ap)
			f.wantStatus("security", f.decide(dcSecurity(t, dcSec), ap, "deny", ""), http.StatusOK, "", ap)
		})
	}
}

// TestDecidersStaleGroupSnapshotMatchesNone: a group decider decides only from a complete snapshot. A
// truncated one, and one the IdP left out (overage, stamped as truncated), match no group.
func TestDecidersStaleGroupSnapshotMatchesNone(t *testing.T) {
	f := newDCFix(t)
	f.st.setRules(f.root.ID, dcRule(types.DeciderKindEgressDomain, types.DeciderGroupPrefix+dcGroup))
	for name, groups := range map[string][]string{"truncated": {dcGroup}, "overage": {}} {
		ap := f.hold(types.DeciderKindEgressDomain)
		if f.mayDecideAs(dcCtx(dcLead, oidc.RoleUser, groups, true), ap) {
			t.Errorf("%s: mayDecide = true", name)
		}
		f.wantStatus(name, f.decide(uaSession(t, dcLead, groups, true), ap, "deny", ""), http.StatusNotFound, reasonApprovalNotFound, ap)
	}
	ap := f.hold(types.DeciderKindEgressDomain)
	f.wantStatus("complete snapshot", f.decide(dcMember(t, dcLead, dcGroup), ap, "deny", ""), http.StatusOK, "", ap)
}

// TestDecidersUnknownStoredWhoMatchesNone: a stored rule from a newer build may name a decider type or a kind
// this build does not know. The type matches nobody, the kind governs nothing, and the rule still binds: the
// owner it leaves out is refused, and the group it names decides.
func TestDecidersUnknownStoredWhoMatchesNone(t *testing.T) {
	f := newDCFix(t)
	f.st.setRules(f.root.ID,
		types.DeciderRule{Kinds: []string{types.DeciderKindEgressDomain}, Who: []string{"manager", "delegate:" + dcOther, types.DeciderGroupPrefix + dcGroup}},
		types.DeciderRule{Kinds: []string{"component_attach"}, Who: []string{types.DeciderUnitAdmins}})
	ap := f.hold(types.DeciderKindEgressDomain)
	f.wantStatus("named by an unknown type", f.decide(dcMember(t, dcOther), ap, "deny", ""), http.StatusNotFound, reasonApprovalNotFound, ap)
	f.wantStatus("owner left out", f.decide(dcMember(t, dcOwner), ap, "deny", ""), http.StatusForbidden, "not_a_decider", ap)
	f.wantStatus("unit admin under an unknown kind", f.decide(dcMember(t, dcAdmin), ap, "deny", ""), http.StatusNotFound, reasonApprovalNotFound, ap)
	f.wantStatus("group member", f.decide(dcMember(t, dcLead, dcGroup), ap, "deny", ""), http.StatusOK, "", ap)
}

// TestDecidersServicePortalDeviceNeverMatch: a service identity, a person acting through a portal's delegated
// token and a device match no decider type, owner included, even on a run that is theirs.
func TestDecidersServicePortalDeviceNeverMatch(t *testing.T) {
	who := []string{types.DeciderOwner, types.DeciderUnitAdmins, types.DeciderGroupPrefix + dcGroup}
	svc := newDCFix(t, dcServiceRun("sub-dc-someone", "sub-dc-someone"))
	svc.st.setRules(svc.root.ID, dcRule(types.DeciderKindEgressDomain, who...))
	if svc.mayDecideAs(withServiceIdentity(context.Background(), "ci", "sub-dc-someone"), svc.hold(types.DeciderKindEgressDomain)) {
		t.Error("a service identity decides egress on its own run under a rule")
	}

	f := newDCFix(t)
	f.st.setRules(f.root.ID, dcRule(types.DeciderKindEgressDomain, who...))
	ap := f.hold(types.DeciderKindEgressDomain)
	portal := types.DelegationVia{Delegate: uuid.New(), Grant: uuid.New()}
	for name, ctx := range map[string]context.Context{
		"portal (owner)":      audit.WithDelegation(dcCtx(dcOwner, oidc.RoleUser, []string{}, false), portal),
		"portal (group)":      audit.WithDelegation(dcCtx(dcLead, oidc.RoleUser, []string{dcGroup}, false), portal),
		"portal (unit admin)": audit.WithDelegation(dcCtx(dcAdmin, oidc.RoleUser, []string{}, false), portal),
		"device (owner)":      context.WithValue(dcCtx(dcOwner, oidc.RoleUser, []string{}, false), deviceCtxKey{}, types.Device{ID: uuid.New(), Name: "laptop"}),
		"device (group)":      context.WithValue(dcCtx(dcLead, oidc.RoleUser, []string{dcGroup}, false), deviceCtxKey{}, types.Device{ID: uuid.New(), Name: "laptop"}),
	} {
		if f.mayDecideAs(ctx, ap) {
			t.Errorf("%s: mayDecide = true", name)
		}
	}
}

// TestDecidersSecurityDecidesEveryKindUnderEveryRule: whatever a rule says, a security admin who is not the
// run's own person decides every kind, and so does the admin token.
func TestDecidersSecurityDecidesEveryKindUnderEveryRule(t *testing.T) {
	for _, kind := range dcRuleKinds {
		for name, who := range map[string][]string{
			"security only": {}, "unit admins": {types.DeciderUnitAdmins}, "group": {types.DeciderGroupPrefix + dcGroup},
		} {
			f := newDCFix(t)
			f.st.setRules(f.team.ID, dcRule(kind, who...))
			ap := f.hold(kind)
			if !f.mayDecideAs(dcCtx(dcSec, oidc.RoleSecurityAdmin, nil, false), ap) {
				t.Errorf("%s/%s: mayDecide = false for a security admin", kind, name)
			}
			f.wantStatus(kind+"/"+name+": security", f.decide(dcSecurity(t, dcSec), ap, "deny", ""), http.StatusOK, "", ap)
			ap = f.hold(kind)
			f.wantStatus(kind+"/"+name+": admin token", f.decide(nil, ap, "deny", ""), http.StatusOK, "", ap)
		}
	}
}

// TestDecidersEmptyWhoDecidedBySecurityOnly: a rule with no who leaves the kind to the security tier. The owner
// who decides it without a rule is told who decides; a unit admin and a group member get the plain 404.
func TestDecidersEmptyWhoDecidedBySecurityOnly(t *testing.T) {
	f := newDCFix(t)
	f.st.setRules(f.team.ID, dcRule(types.DeciderKindEgressDomain))
	ap := f.hold(types.DeciderKindEgressDomain)
	w := f.decide(dcMember(t, dcOwner), ap, "deny", "")
	f.wantStatus("owner", w, http.StatusForbidden, "not_a_decider", ap)
	if !strings.Contains(w.Body.String(), "security") {
		t.Errorf("not_a_decider body %s does not name who decides", w.Body)
	}
	f.wantStatus("unit admin", f.decide(dcMember(t, dcAdmin), ap, "deny", ""), http.StatusNotFound, reasonApprovalNotFound, ap)
	f.wantStatus("group member", f.decide(dcMember(t, dcLead, dcGroup), ap, "deny", ""), http.StatusNotFound, reasonApprovalNotFound, ap)
	f.wantStatus("security", f.decide(dcSecurity(t, dcSec), ap, "deny", ""), http.StatusOK, "", ap)
}

// TestDecidersGrantRevokedWhilePendingRefused: a unit admin is resolved at decide time, so a grant revoked
// while a request waits no longer decides it.
func TestDecidersGrantRevokedWhilePendingRefused(t *testing.T) {
	f := newDCFix(t)
	f.st.setRules(f.root.ID, dcRule(types.DeciderKindEgressDomain, types.DeciderUnitAdmins))
	first, second := f.hold(types.DeciderKindEgressDomain), f.hold(types.DeciderKindEgressDomain)
	f.wantStatus("with the grant", f.decide(dcMember(t, dcAdmin), first, "deny", ""), http.StatusOK, "", first)
	f.st.revokeGrants(dcAdmin)
	f.wantStatus("grant revoked", f.decide(dcMember(t, dcAdmin), second, "deny", ""), http.StatusNotFound, reasonApprovalNotFound, second)
}

// TestDecidersGroupLeftWhilePendingRefused: a group decider is read from the session deciding, so a new session
// without the group no longer decides.
func TestDecidersGroupLeftWhilePendingRefused(t *testing.T) {
	f := newDCFix(t)
	f.st.setRules(f.root.ID, dcRule(types.DeciderKindEgressDomain, types.DeciderGroupPrefix+dcGroup))
	first, second := f.hold(types.DeciderKindEgressDomain), f.hold(types.DeciderKindEgressDomain)
	f.wantStatus("in the group", f.decide(dcMember(t, dcLead, dcGroup), first, "deny", ""), http.StatusOK, "", first)
	f.wantStatus("left the group", f.decide(dcMember(t, dcLead), second, "deny", ""), http.StatusNotFound, reasonApprovalNotFound, second)
}

// TestDecidersRuleEditedWhilePendingUsesCurrent: the rules are read when the decision is made, not when the
// request was raised, and every rule for the kind from the run's unit up binds it.
func TestDecidersRuleEditedWhilePendingUsesCurrent(t *testing.T) {
	f := newDCFix(t)
	f.st.setRules(f.root.ID, dcRule(types.DeciderKindEgressDomain, types.DeciderGroupPrefix+dcGroup))
	ap := f.hold(types.DeciderKindEgressDomain)
	f.wantStatus("owner under the group rule", f.decide(dcMember(t, dcOwner), ap, "deny", ""), http.StatusForbidden, "not_a_decider", ap)
	f.st.setRules(f.team.ID, dcRule(types.DeciderKindEgressDomain, types.DeciderUnitAdmins))
	f.wantStatus("group member under a nearer rule naming nobody in common", f.decide(dcMember(t, dcLead, dcGroup), ap, "deny", ""), http.StatusNotFound, reasonApprovalNotFound, ap)
	f.st.setRules(f.team.ID, dcRule(types.DeciderKindEgressDomain, types.DeciderOwner))
	f.wantStatus("owner while the rule above leaves owner out", f.decide(dcMember(t, dcOwner), ap, "deny", ""), http.StatusForbidden, "not_a_decider", ap)
	f.st.setRules(f.root.ID, dcRule(types.DeciderKindEgressDomain, types.DeciderOwner, types.DeciderGroupPrefix+dcGroup))
	f.wantStatus("owner once both rules list owner", f.decide(dcMember(t, dcOwner), ap, "deny", ""), http.StatusOK, "", ap)
}
