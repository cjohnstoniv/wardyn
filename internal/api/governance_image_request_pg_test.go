// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// Image requests against Postgres (POST /governance/requests, kind image): who may ask (a person whose
// default context has an image rule up its chain), who decides (the met rule's who, never the requester),
// the one exact allow grant an approval writes, and what still binds at launch. Guarded by WARDYN_TEST_PG.
//
// They run on the unit-admin tree of governance_unit_admin_pg_test.go (ua-a with ua-a1 and ua-a2 below it,
// ua-b beside it; adminA on ua-a, adminA1 on ua-a1). The requester is a member of ua-a1, and ua-a carries
// an image rule: unit admins and the img-deciders group decide.

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

	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/imageadmit"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const (
	imgDeciders = "img-deciders"
	imgReqSub   = "sub-img-req"
	imgDecider  = "sub-img-decider"
	imgHex      = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	imgRef      = "registry.example.com/team/tool:1@sha256:" + imgHex
	// imgOtherName is the same bytes under another name.
	imgOtherName = "mirror.example.com/tools/tool@sha256:" + imgHex
)

type imgEnv struct {
	*unitAdminEnv
	req, deciderGroup *http.Cookie
}

func newImgEnv(t *testing.T) *imgEnv {
	t.Helper()
	e := &imgEnv{unitAdminEnv: newUnitAdminEnv(t, false)}
	e.req = uaSession(t, imgReqSub, []string{}, false)
	e.deciderGroup = uaSession(t, imgDecider, []string{imgDeciders}, false)
	treeMember(t, e.govEnv, e.a1, types.CapabilitySubjectUser, imgReqSub, 1, true)
	e.setRules(e.a, imageRule(nil))
	e.enforceImages()
	return e
}

// imageRule is ua-a's image rule, changed by f.
func imageRule(f func(*types.DeciderRule)) types.DeciderRule {
	r := types.DeciderRule{Kinds: []string{types.DeciderKindImage}, Who: []string{types.DeciderUnitAdmins, "group:" + imgDeciders}}
	if f != nil {
		f(&r)
	}
	return r
}

// setRules stores u's rules as a write that passed validation would.
func (e *imgEnv) setRules(u types.GovernanceUnit, rules ...types.DeciderRule) {
	e.t.Helper()
	valid, err := types.ValidateDeciderRules(rules)
	if err != nil {
		e.t.Fatalf("rules for %s: %v", u.Name, err)
	}
	if _, err := store.SetUnitDecidersQ(context.Background(), e.pool, u.ID, valid, "seed"); err != nil {
		e.t.Fatalf("store rules for %s: %v", u.Name, err)
	}
}

// enforceImages switches the image kind on, so an allow grant is what lets a person launch an image.
// newImgEnv does it: off, an image request is not offered.
func (e *imgEnv) enforceImages() {
	e.t.Helper()
	if _, err := e.pg.PutCapabilityEnforcement(context.Background(), map[string]bool{capImage: true}); err != nil {
		e.t.Fatal(err)
	}
}

func imgBody(ref, reason, ticket string) string {
	m := map[string]any{"kind": "image", "image": ref, "reason": reason}
	if ticket != "" {
		m["ticket"] = ticket
	}
	b, _ := json.Marshal(m)
	return string(b)
}

// ask posts an image request as c and requires the 202.
func (e *imgEnv) ask(c *http.Cookie, ref, ticket string) elevCreated {
	e.t.Helper()
	w := e.call(c, http.MethodPost, requestsPath, imgBody(ref, "the build needs this tool", ticket))
	if w.Code != http.StatusAccepted {
		e.t.Fatalf("request image %s = %d %s, want 202", ref, w.Code, w.Body)
	}
	if loc := w.Header().Get("Location"); loc != "" {
		e.t.Errorf("a request's 202 sets Location %q; a requester cannot read /governance/changes", loc)
	}
	var out elevCreated
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || out.Pending.ID == uuid.Nil || out.Request.ID != out.Pending.ID {
		e.t.Fatalf("202 body %s: %v", w.Body, err)
	}
	return out
}

type imgList struct {
	Requests     []GovernanceRequestView `json:"requests"`
	Offered      []ElevationOffer        `json:"offered"`
	ImageOffered bool                    `json:"image_offered"`
}

func (e *imgEnv) list(c *http.Cookie) imgList {
	e.t.Helper()
	w := e.call(c, http.MethodGet, requestsPath, "")
	var out imgList
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &out) != nil {
		e.t.Fatalf("GET %s = %d %s", requestsPath, w.Code, w.Body)
	}
	return out
}

// createAs calls the create handler with ctx as the caller, for identities a cookie cannot carry.
func (e *imgEnv) createAs(ctx context.Context, body string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	e.srv.handleCreateGovernanceRequest(w, httptest.NewRequest(http.MethodPost, requestsPath, strings.NewReader(body)).WithContext(ctx))
	return w
}

// listAs calls the list handler with ctx as the caller and requires the 200.
func (e *imgEnv) listAs(ctx context.Context) imgList {
	e.t.Helper()
	w := httptest.NewRecorder()
	e.srv.handleListGovernanceRequests(w, httptest.NewRequest(http.MethodGet, requestsPath, nil).WithContext(ctx))
	var out imgList
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &out) != nil {
		e.t.Fatalf("list as ctx = %d %s", w.Code, w.Body)
	}
	return out
}

func (e *imgEnv) wantReason(name string, w *httptest.ResponseRecorder, code int, reason string) {
	e.t.Helper()
	if w.Code != code || wireReason(e.t, w) != reason {
		e.t.Errorf("%s = %d %s, want %d %s", name, w.Code, w.Body, code, reason)
	}
}

// grantOf is sub's image grant of exactly ref, nil when none.
func (e *imgEnv) grantOf(sub, ref string) *types.CapabilityGrant {
	e.t.Helper()
	g, err := store.GetCapabilityGrantByKeyQ(context.Background(), e.pool, string(types.CapabilitySubjectUser), types.CanonicalUserSubject(sub), capImage, ref, false)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		e.t.Fatal(err)
	}
	return &g
}

// seedGrant writes sub's grant of ref with effect, as a direct permissions write does.
func (e *imgEnv) seedGrant(st types.CapabilitySubjectType, sub, ref string, effect types.CapabilityEffect) {
	e.t.Helper()
	if st == types.CapabilitySubjectUser {
		sub = types.CanonicalUserSubject(sub)
	}
	if _, err := e.pg.UpsertCapabilityGrant(context.Background(), types.CapabilityGrant{SubjectType: st, Subject: sub,
		Capability: capImage, Value: ref, Effect: effect, CreatedBy: "seed"}); err != nil {
		e.t.Fatal(err)
	}
}

func (e *imgEnv) rowsFor(action, target string) []map[string]any {
	e.t.Helper()
	var out []map[string]any
	for _, ev := range e.audits(action) {
		if ev.Target == target {
			out = append(out, auditData(e.t, ev))
		}
	}
	return out
}

// approved asks for ref as the requester, approves it as adminA and returns the grant it wrote.
func (e *imgEnv) approved(ref string) types.CapabilityGrant {
	e.t.Helper()
	got := e.ask(e.req, ref, "")
	if w := e.call(e.adminA, http.MethodPost, requestPath(got.Pending.ID, "approve"), ""); w.Code != http.StatusOK {
		e.t.Fatalf("approve = %d %s", w.Code, w.Body)
	}
	g := e.grantOf(imgReqSub, ref)
	if g == nil {
		e.t.Fatal("the approval wrote no grant")
	}
	return *g
}

// imgPersonCtx is the requester's context as a run create reads it.
func imgPersonCtx(sub string) context.Context { return elevPersonCtx(sub, []string{}, false) }

// imgRunRefusal is the image check of a run create naming ref, as ctx.
func (e *imgEnv) imgRunRefusal(ctx context.Context, ref string) *runRefusal {
	e.t.Helper()
	_, ref2 := e.srv.runRequestGovernance(governReq(ctx), createRunRequest{Image: ref})
	return ref2
}

// The two refusal sentences, pinned byte for byte.
const (
	imgNotGrantedText = "image " + imgRef + " is not granted to you — ask an admin for an image grant that covers it, " +
		"or launch with the agent's convention image or an onboarded workspace's base image"
	imgRequestItText = "image " + imgRef + " is not granted to you — request it in Approvals, ask an admin for an image grant that covers it, " +
		"or launch with the agent's convention image or an onboarded workspace's base image"
)

// imgDecidersDown is a store whose deciders read fails.
type imgDecidersDown struct{ store.PG }

func (imgDecidersDown) ListUnitDeciders(context.Context) ([]types.UnitDeciders, error) {
	return nil, errors.New("deciders unreadable")
}

// imgMembershipsDown is a store whose membership read fails.
type imgMembershipsDown struct{ store.PG }

func (imgMembershipsDown) ListMembershipsFor(context.Context, []string, []string, string) ([]store.MembershipMatch, error) {
	return nil, errors.New("memberships unreadable")
}

// imgAdmitSite is a store whose site config turns image admission on.
type imgAdmitSite struct{ store.PG }

func (imgAdmitSite) GetSiteConfig(context.Context) (types.SiteConfig, error) {
	return admissionSite(types.ImageAdmissionEnforce), nil
}

func TestImageRequestApproveCreatesExactGrant(t *testing.T) {
	e := newImgEnv(t)
	got := e.ask(e.req, imgRef, "")
	if got.Pending.TargetKind != types.GovernanceTargetImageRequest || got.Request.Kind != "image" || got.Request.Image != imgRef ||
		got.Request.UnitID == nil || *got.Request.UnitID != e.a1.ID || !got.Request.Mine || got.Request.Decidable {
		t.Fatalf("created = %+v, want a pending image request on ua-a1, mine and not decidable", got)
	}
	if want := types.CanonicalUserSubject(imgReqSub) + "|sha256:" + imgHex; got.Pending.TargetKey != want {
		t.Errorf("target key = %q, want %q", got.Pending.TargetKey, want)
	}
	if e.grantOf(imgReqSub, imgRef) != nil {
		t.Fatal("a pending request wrote a grant")
	}
	// A root unit admin decides under the image rule on the root.
	if w := e.call(e.adminA, http.MethodPost, requestPath(got.Pending.ID, "approve"), ""); w.Code != http.StatusOK {
		t.Fatalf("approve = %d %s", w.Code, w.Body)
	}
	g := e.grantOf(imgReqSub, imgRef)
	if g == nil || g.SubjectType != types.CapabilitySubjectUser || g.Subject != types.CanonicalUserSubject(imgReqSub) ||
		g.Capability != capImage || g.Value != imgRef || g.Effect != types.CapabilityAllow || g.CreatedBy != uaSubAdminA {
		t.Fatalf("grant = %+v, want an exact allow of the ref for the requester, created by the decider", g)
	}
	all, err := e.pg.ListCapabilityGrants(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, x := range all {
		if x.Subject == g.Subject {
			n++
			if x.Value != imgRef || strings.Contains(x.Value, "*") {
				t.Errorf("grant %+v: want only the exact ref, never a prefix", x)
			}
		}
	}
	if n != 1 {
		t.Errorf("the requester holds %d grants, want exactly one", n)
	}
	rows := e.rowsFor("governance.image_request.approve", g.ID.String())
	if len(rows) != 1 || rows[0]["grant_id"] != g.ID.String() || rows[0]["image"] != imgRef || rows[0]["unit_scope"] != e.a.ID.String() {
		t.Errorf("approve rows = %v, want one with grant_id, image and unit_scope ua-a", rows)
	}
	for _, ev := range e.audits("capability.grant.create") {
		if ev.Target == g.ID.String() {
			t.Error("an image request's approval also wrote capability.grant.create; want one row")
		}
	}
	l := e.list(e.req)
	if len(l.Requests) != 1 || l.Requests[0].State != types.GovernanceChangeApplied || l.Requests[0].Image != imgRef {
		t.Errorf("requester's list = %+v, want the applied request", l.Requests)
	}
}

func TestImageRequestRequesterCannotApprove(t *testing.T) {
	e := newImgEnv(t)
	got := e.ask(e.req, imgRef, "")
	if w := e.call(e.req, http.MethodPost, requestPath(got.Pending.ID, "approve"), ""); !scopeRefused(t, w) {
		t.Errorf("the requester approving = %d %s, want 403 unit_scope_denied", w.Code, w.Body)
	}
	// A requester who is also a decider (in img-deciders) is still refused: the second-human rule.
	const both = "sub-img-both"
	treeMember(t, e.govEnv, e.a1, types.CapabilitySubjectUser, both, 1, true)
	c := uaSession(t, both, []string{imgDeciders}, false)
	mine := e.ask(c, imgRef, "")
	if w := e.call(c, http.MethodPost, requestPath(mine.Pending.ID, "approve"), ""); w.Code != http.StatusForbidden || wireReason(t, w) != "second_human_required" {
		t.Errorf("a requester in the deciders group approving = %d %s, want 403 second_human_required", w.Code, w.Body)
	}
	if e.grantOf(both, imgRef) != nil || e.grantOf(imgReqSub, imgRef) != nil {
		t.Error("a refused approval wrote a grant")
	}
}

func TestImageRequestNotOfferedWithoutRule(t *testing.T) {
	e := newImgEnv(t)
	e.setRules(e.a)
	e.wantReason("no image rule", e.call(e.req, http.MethodPost, requestsPath, imgBody(imgRef, "need it", "")), http.StatusBadRequest, reasonImageRequestNotOffered)
	if l := e.list(e.req); l.ImageOffered {
		t.Error("image_offered with no image rule on the chain")
	}
}

func TestImageRequestUsesDefaultContextChain(t *testing.T) {
	e := newImgEnv(t)
	// Home in ua-b (no rule); a member of ua-a1 too, whose chain has one. The default context is ua-b.
	const sub = "sub-img-two"
	treeMember(t, e.govEnv, e.b, types.CapabilitySubjectUser, sub, 1, true)
	treeMember(t, e.govEnv, e.a1, types.CapabilitySubjectUser, sub, 5, false)
	c := uaSession(t, sub, []string{}, false)
	e.wantReason("a laxer non-default unit", e.call(c, http.MethodPost, requestsPath, imgBody(imgRef, "need it", "")), http.StatusBadRequest, reasonImageRequestNotOffered)
	if l := e.list(c); l.ImageOffered {
		t.Error("image_offered from a unit that is not the default context")
	}
	e.setRules(e.b, imageRule(nil))
	got := e.ask(c, imgRef, "")
	if got.Request.UnitID == nil || *got.Request.UnitID != e.b.ID {
		t.Errorf("request unit = %v, want the default context ua-b", got.Request.UnitID)
	}
}

func TestImageRequestUnassignedNotOffered(t *testing.T) {
	e := newImgEnv(t)
	// Even a rule on Default offers nothing: Default is never an offer.
	e.setRules(types.GovernanceUnit{ID: store.DefaultUnitID, Name: "Default"}, imageRule(nil))
	c := uaSession(t, "sub-img-nowhere", []string{}, false)
	e.wantReason("an unassigned person", e.call(c, http.MethodPost, requestsPath, imgBody(imgRef, "need it", "")), http.StatusBadRequest, reasonImageRequestNotOffered)
	if l := e.list(c); l.ImageOffered {
		t.Error("image_offered to a person in no unit")
	}
}

func TestImageRequestDeciderRecheckedInTx(t *testing.T) {
	e := newImgEnv(t)
	got := e.ask(e.req, imgRef, "")
	if l := e.list(e.deciderGroup); len(l.Requests) != 1 || !l.Requests[0].Decidable {
		t.Fatalf("group decider's list = %+v, want the request, decidable", l.Requests)
	}
	e.setRules(e.a, imageRule(func(r *types.DeciderRule) { r.Who = []string{types.DeciderUnitAdmins} }))
	r := httptest.NewRequest(http.MethodPost, "/", nil).WithContext(elevPersonCtx(imgDecider, []string{imgDeciders}, false))
	kind := governanceChangeKinds[types.GovernanceTargetImageRequest].forChange(got.Pending)
	err := e.pg.DryRunGovernance(r.Context(), func(q store.Querier) error { return e.srv.recheckApprover(r.Context(), r, q, kind) })
	var refused *govRefusal
	if !errors.As(err, &refused) {
		t.Errorf("the in-transaction recheck of the removed group = %v, want a refusal", err)
	}
	if w := e.call(e.deciderGroup, http.MethodPost, requestPath(got.Pending.ID, "approve"), ""); !scopeRefused(t, w) && w.Code != http.StatusNotFound {
		t.Errorf("the removed group approving = %d %s, want 404 or 403 unit_scope_denied", w.Code, w.Body)
	}
	if e.grantOf(imgReqSub, imgRef) != nil {
		t.Error("a refused approval wrote a grant")
	}
}

func TestImageRequestServicePortalDeviceRefused(t *testing.T) {
	e := newImgEnv(t)
	body := imgBody(imgRef, "need it", "")
	e.wantReason("the admin token", e.admin(http.MethodPost, requestsPath, body), http.StatusForbidden, reasonGovernanceRequestNeedsPerson)
	service := withServiceIdentity(imgPersonCtx("sub-img-svc"), "deploy-bot", imgReqSub)
	e.wantReason("a service token", e.createAs(service, body), http.StatusForbidden, reasonGovernanceRequestNeedsPerson)
	device := context.WithValue(imgPersonCtx("sub-img-dev"), deviceCtxKey{}, types.Device{ID: uuid.New(), Name: "laptop"})
	e.wantReason("a device", e.createAs(device, body), http.StatusForbidden, reasonGovernanceRequestNeedsPerson)
	if l := e.listAs(service); l.ImageOffered {
		t.Error("image_offered to a service")
	}
	for key := range delegationAllowed {
		if strings.Contains(key, "/governance/requests") {
			t.Errorf("delegationAllowed holds %q; a portal must get delegation_scope", key)
		}
	}
}

func TestImageRequestAlreadyGranted409(t *testing.T) {
	e := newImgEnv(t)
	e.seedGrant(types.CapabilitySubjectUser, imgReqSub, imgRef, types.CapabilityAllow)
	e.wantReason("an image already granted", e.call(e.req, http.MethodPost, requestsPath, imgBody(imgRef, "need it", "")), http.StatusConflict, reasonImageAlreadyGranted)
	// The grant is exact: the same digest under another name is not granted, so it may be asked for.
	e.ask(e.req, imgOtherName, "")
}

func TestImageRequestRestrictionStillRefusesAtLaunch(t *testing.T) {
	e := newImgEnv(t)
	e.approved(imgRef)
	ctx := imgPersonCtx(imgReqSub)
	if ok, err := e.srv.capGranted(withCapBatch(ctx), capImage, imgRef); err != nil || !ok {
		t.Fatalf("after the approval capGranted = %v, %v; want granted", ok, err)
	}
	// A deny still wins over the approved grant at launch.
	e.seedGrant(types.CapabilitySubjectAll, "", imgRef, types.CapabilityDeny)
	ref := e.imgRunRefusal(ctx, imgRef)
	if ref == nil || ref.decision == nil || ref.decision.Reason != authz.ReasonBYOIUser || ref.decision.Target != "runs.image" {
		t.Errorf("a launch of the denied image = %+v, want byoi_user on runs.image", ref)
	}
}

func TestImageRequestAdmissionStillApplies(t *testing.T) {
	e := newImgEnv(t)
	e.approved(imgRef)
	e.srv.cfg.Store = imgAdmitSite{e.pg}
	e.srv.cfg.ImageAdmissionConfig.ImageAdmitter = &fakeAdmitter{admit: func(imageadmit.Requirements) (imageadmit.Verdict, error) {
		return imageadmit.Verdict{Reason: imageadmit.SignatureMissing, Digest: "sha256:" + imgHex, Signers: []string{admitSigner}}, nil
	}}
	ctx := imgPersonCtx(imgReqSub)
	if ok, err := e.srv.capGranted(withCapBatch(ctx), capImage, imgRef); err != nil || !ok {
		t.Fatalf("after the approval capGranted = %v, %v; want granted", ok, err)
	}
	got := runAdmission(e.srv, ctx, foldCreate, imgRef, nil, nil)
	if got.ok || got.body(t).Reason != string(imageadmit.SignatureMissing) {
		t.Errorf("admission of the approved image = ok %v %s, want refused signature_missing", got.ok, got.resp.Body)
	}
}

func TestImageRequestReasonAndTicketAudited(t *testing.T) {
	e := newImgEnv(t)
	got := e.ask(e.req, imgRef, "CHG-77")
	req := e.rowsFor("governance.image_request.request", got.Pending.ID.String())
	if len(req) != 1 || req[0]["reason"] != "the build needs this tool" || req[0]["ticket"] != "CHG-77" || req[0]["change_id"] != got.Pending.ID.String() ||
		req[0]["image"] != imgRef || req[0]["unit_id"] != e.a1.ID.String() || req[0]["expires_at"] == nil {
		t.Fatalf("request rows = %v", req)
	}
	if units, _ := req[0]["units"].([]any); len(units) != 1 || units[0] != e.a1.ID.String() {
		t.Errorf("request row units = %v, want [ua-a1]", req[0]["units"])
	}
	fields := audit.SealedFields("governance.image_request.request")
	if len(fields) != 1 || fields[0].Path != "reason" || fields[0].Subject != audit.SubjectActor {
		t.Errorf("sealed fields of the request row = %+v, want reason under the actor", fields)
	}
	if w := e.call(e.adminA1, http.MethodPost, requestPath(got.Pending.ID, "approve"), ""); w.Code != http.StatusOK {
		t.Fatalf("approve = %d %s", w.Code, w.Body)
	}
	g := e.grantOf(imgReqSub, imgRef)
	app := e.rowsFor("governance.image_request.approve", g.ID.String())
	if len(app) != 1 || app[0]["ticket"] != "CHG-77" || app[0]["change_id"] != got.Pending.ID.String() || app[0]["subject"] != g.Subject ||
		app[0]["subject_type"] != "user" || app[0]["unit_id"] != e.a1.ID.String() || app[0]["unit_scope"] != e.a1.ID.String() || app[0]["reason"] != nil {
		t.Errorf("approve rows = %v, want ticket, change_id, subject, unit_id, unit_scope ua-a1 and no reason", app)
	}
	// A reject row carries the ticket and no reason.
	const other = "sub-img-other"
	treeMember(t, e.govEnv, e.a1, types.CapabilitySubjectUser, other, 1, true)
	rej := e.ask(uaSession(t, other, []string{}, false), imgRef, "CHG-9")
	if w := e.call(e.adminA1, http.MethodPost, requestPath(rej.Pending.ID, "reject"), `{"reason":"not now"}`); w.Code != http.StatusOK {
		t.Fatalf("reject = %d %s", w.Code, w.Body)
	}
	rows := e.rowsFor("governance.image_request.reject", rej.Pending.ID.String())
	if len(rows) != 1 || rows[0]["ticket"] != "CHG-9" || rows[0]["image"] != imgRef || rows[0]["reason"] != nil || rows[0]["withdrawn"] != nil {
		t.Errorf("reject rows = %v, want the image and ticket, no reason, not withdrawn", rows)
	}
}

func TestImageRequestStaleWhenGrantAppeared(t *testing.T) {
	e := newImgEnv(t)
	got := e.ask(e.req, imgRef, "")
	e.seedGrant(types.CapabilitySubjectUser, imgReqSub, imgRef, types.CapabilityAllow)
	_ = e.call(e.alice, http.MethodPost, requestPath(got.Pending.ID, "approve"), "")
	if s := e.changeState(got.Pending.ID); s != types.GovernanceChangeStale {
		t.Errorf("a request whose equal grant appeared = %s, want stale", s)
	}
	if g := e.grantOf(imgReqSub, imgRef); g == nil || g.CreatedBy != "seed" {
		t.Errorf("grant = %+v, want the seeded one untouched", g)
	}
}

func TestImageRequestStaleWhenRuleRemoved(t *testing.T) {
	e := newImgEnv(t)
	got := e.ask(e.req, imgRef, "")
	e.setRules(e.a)
	_ = e.call(e.alice, http.MethodPost, requestPath(got.Pending.ID, "approve"), "")
	if s := e.changeState(got.Pending.ID); s != types.GovernanceChangeStale || e.grantOf(imgReqSub, imgRef) != nil {
		t.Errorf("a request whose rule was removed = %s, want stale and no grant", s)
	}
}

func TestImageRequestPendingTwiceIs409(t *testing.T) {
	e := newImgEnv(t)
	e.ask(e.req, imgRef, "")
	e.wantReason("the same ref again", e.call(e.req, http.MethodPost, requestsPath, imgBody(imgRef, "again", "")), http.StatusConflict, reasonGovernanceChangePending)
	e.wantReason("the same digest under another name", e.call(e.req, http.MethodPost, requestsPath, imgBody(imgOtherName, "again", "")), http.StatusConflict, reasonGovernanceChangePending)
}

func TestImageRefusalTextOffersRequestOnlyWhenOffered(t *testing.T) {
	e := newImgEnv(t)
	ref := e.imgRunRefusal(imgPersonCtx(imgReqSub), imgRef)
	if ref == nil || ref.decision == nil || ref.decision.Sentence != imgRequestItText ||
		ref.decision.Reason != authz.ReasonBYOIUser || ref.decision.Target != "runs.image" {
		t.Errorf("offered person's refusal = %+v, want %q on byoi_user, runs.image", ref, imgRequestItText)
	}
	const elsewhere = "sub-img-elsewhere"
	treeMember(t, e.govEnv, e.b, types.CapabilitySubjectUser, elsewhere, 1, true)
	ref = e.imgRunRefusal(imgPersonCtx(elsewhere), imgRef)
	if ref == nil || ref.decision == nil || ref.decision.Sentence != imgNotGrantedText ||
		ref.decision.Reason != authz.ReasonBYOIUser || ref.decision.Target != "runs.image" {
		t.Errorf("not-offered person's refusal = %+v, want the unchanged %q", ref, imgNotGrantedText)
	}
}

func TestImageRequestNotOfferedWhileKindOff(t *testing.T) {
	e := newImgEnv(t)
	if _, err := e.pg.PutCapabilityEnforcement(context.Background(), map[string]bool{capImage: false}); err != nil {
		t.Fatal(err)
	}
	w := e.call(e.req, http.MethodPost, requestsPath, imgBody(imgRef, "need it", ""))
	e.wantReason("a request while the image kind is off", w, http.StatusBadRequest, reasonImageRequestNotOffered)
	if !strings.Contains(w.Body.String(), "image grants are off in this deployment") {
		t.Errorf("body = %s, want the switch-off sentence", w.Body)
	}
	if ref := e.imgRunRefusal(imgPersonCtx(imgReqSub), imgRef); ref == nil || ref.decision == nil || ref.decision.Sentence != imgNotGrantedText {
		t.Errorf("launch refusal while the image kind is off = %+v, want the unchanged %q", ref, imgNotGrantedText)
	}
	// The list's image_offered is rule-based: there is no ref to ask about.
	if l := e.list(e.req); !l.ImageOffered {
		t.Error("image_offered false with the rule present and the switch off; want rule-based")
	}
}

func TestImageRefusalTextOnOfferReadErrorIsUnchanged(t *testing.T) {
	e := newImgEnv(t)
	e.srv.cfg.Store = imgDecidersDown{e.pg}
	ref := e.imgRunRefusal(imgPersonCtx(imgReqSub), imgRef)
	if ref == nil || ref.decision == nil || ref.decision.Sentence != imgNotGrantedText {
		t.Errorf("refusal with the deciders unreadable = %+v, want the unchanged %q", ref, imgNotGrantedText)
	}
}

func TestGovernanceRequestsImageOffered(t *testing.T) {
	e := newImgEnv(t)
	if l := e.list(e.req); !l.ImageOffered {
		t.Error("image_offered false for a person whose default context has an image rule up its chain")
	}
	if l := e.listAs(withServiceIdentity(imgPersonCtx("sub-img-svc"), "deploy-bot", imgReqSub)); l.ImageOffered {
		t.Error("image_offered to a non-person")
	}
	if l := e.list(uaSession(t, "sub-img-default", []string{}, false)); l.ImageOffered {
		t.Error("image_offered to a person in Default")
	}
	treeMember(t, e.govEnv, e.a1, types.CapabilitySubjectGroup, "img-team", 1, false)
	if l := e.listAs(elevPersonCtx("sub-img-stale", []string{"img-team"}, true)); l.ImageOffered {
		t.Error("image_offered on a truncated group snapshot")
	}
	e.srv.cfg.Store = imgMembershipsDown{e.pg}
	if l := e.listAs(imgPersonCtx(imgReqSub)); l.ImageOffered {
		t.Error("image_offered when the offer could not be read")
	}
}

func TestImageRequestStaleGroupsNotOffered(t *testing.T) {
	e := newImgEnv(t)
	treeMember(t, e.govEnv, e.a1, types.CapabilitySubjectGroup, "img-team", 1, false)
	ctx := elevPersonCtx("sub-img-stale", []string{"img-team"}, true)
	e.wantReason("a truncated snapshot", e.createAs(ctx, imgBody(imgRef, "need it", "")), http.StatusBadRequest, reasonImageRequestNotOffered)
	if l := e.listAs(ctx); l.ImageOffered {
		t.Error("image_offered on a truncated group snapshot")
	}
	denied := func() int {
		n := 0
		for _, ev := range e.audits(authz.AuditAction) {
			if ev.Target == "governance.ceiling" {
				n++
			}
		}
		return n
	}
	before := denied()
	unit, rule, err := e.srv.imageRequestOffered(ctx)
	if err != nil || rule != nil {
		t.Errorf("imageRequestOffered on a truncated snapshot = %v, %+v, %v; want no rule, no error", unit.ID, rule, err)
	}
	if after := denied(); after != before {
		t.Errorf("the offer read recorded %d groups-stale refusal rows, want none", after-before)
	}
	// A person a user row places in the rule's chain is not offered on a truncated snapshot either.
	member := elevPersonCtx(imgReqSub, []string{}, true)
	e.wantReason("a member on a truncated snapshot", e.createAs(member, imgBody(imgRef, "need it", "")), http.StatusBadRequest, reasonImageRequestNotOffered)
	if l := e.listAs(member); l.ImageOffered {
		t.Error("image_offered to a member on a truncated group snapshot")
	}
}

func TestImageRequestOfferedReadErrorIs503(t *testing.T) {
	e := newImgEnv(t)
	e.srv.cfg.Store = imgDecidersDown{e.pg}
	e.wantReason("deciders unreadable", e.call(e.req, http.MethodPost, requestsPath, imgBody(imgRef, "need it", "")), http.StatusServiceUnavailable, reasonDecidersUnreadable)
}

func TestImageRequestGroupDeciderAuditUnitScope(t *testing.T) {
	e := newImgEnv(t)
	got := e.ask(e.req, imgRef, "")
	if w := e.call(e.deciderGroup, http.MethodPost, requestPath(got.Pending.ID, "approve"), ""); w.Code != http.StatusOK {
		t.Fatalf("group approve = %d %s", w.Code, w.Body)
	}
	g := e.grantOf(imgReqSub, imgRef)
	if rows := e.rowsFor("governance.image_request.approve", g.ID.String()); len(rows) != 1 || rows[0]["unit_scope"] != "group:"+imgDeciders {
		t.Errorf("approve rows = %v, want unit_scope group:%s", rows, imgDeciders)
	}
	const other = "sub-img-rej"
	treeMember(t, e.govEnv, e.a1, types.CapabilitySubjectUser, other, 1, true)
	rej := e.ask(uaSession(t, other, []string{}, false), imgRef, "")
	if w := e.call(e.deciderGroup, http.MethodPost, requestPath(rej.Pending.ID, "reject"), ""); w.Code != http.StatusOK {
		t.Fatalf("group reject = %d %s", w.Code, w.Body)
	}
	if rows := e.rowsFor("governance.image_request.reject", rej.Pending.ID.String()); len(rows) != 1 || rows[0]["unit_scope"] != "group:"+imgDeciders {
		t.Errorf("reject rows = %v, want unit_scope group:%s", rows, imgDeciders)
	}
}

func TestImageRequestAdminTokenBypassAudited(t *testing.T) {
	e := newImgEnv(t)
	got := e.ask(e.req, imgRef, "")
	if w := e.admin(http.MethodPost, requestPath(got.Pending.ID, "approve"), ""); w.Code != http.StatusOK {
		t.Fatalf("admin token approve = %d %s", w.Code, w.Body)
	}
	g := e.grantOf(imgReqSub, imgRef)
	if g == nil {
		t.Fatal("the admin token's approval wrote no grant")
	}
	found := false
	for _, ev := range e.audits("governance.change.bypass") {
		found = found || ev.Target == got.Pending.ID.String()
	}
	if !found {
		t.Error("no governance.change.bypass row for the admin token's approval")
	}
	if rows := e.rowsFor("governance.image_request.approve", g.ID.String()); len(rows) != 1 || rows[0]["unit_scope"] == nil || rows[0]["unit_scope"] == "" {
		t.Errorf("approve rows = %v, want one with a unit_scope", rows)
	}
}

func TestImageRequestGroupDeciderTokenNeedsStampTTL(t *testing.T) {
	e := newImgEnv(t)
	got := e.ask(e.req, imgRef, "")
	ch, err := e.pg.GetGovernanceChange(context.Background(), got.Pending.ID)
	if err != nil {
		t.Fatal(err)
	}
	approver := requestApprover(ch, types.DeciderKindImage)
	as := func(ctx context.Context) bool {
		return approver.allowed(e.srv, httptest.NewRequest(http.MethodPost, "/", nil).WithContext(ctx))
	}
	token := withAPITokenID(elevPersonCtx(imgDecider, []string{imgDeciders}, false), uuid.New())
	cli := withCLISessionID(withAPITokenID(elevPersonCtx(imgDecider, []string{imgDeciders}, false), uuid.New()), uuid.New())
	if as(token) {
		t.Error("TTL off: a bare API token in the deciders group decides")
	}
	if !as(cli) {
		t.Error("TTL off: a CLI session in the deciders group does not decide")
	}
	e.srv.cfg.RoleStampTTL = time.Hour
	if !as(token) {
		t.Error("TTL on: a token in the deciders group does not decide")
	}
	if !as(cli) {
		t.Error("TTL on: a CLI session in the deciders group does not decide")
	}
}

func TestImageRequestChainMeet(t *testing.T) {
	e := newImgEnv(t)
	// The child first: a parent's write never checks below it.
	e.setRules(e.a1, imageRule(func(r *types.DeciderRule) { r.Who = []string{"group:img-x"} }))
	e.setRules(e.a, imageRule(func(r *types.DeciderRule) { r.Who = []string{types.DeciderUnitAdmins} }))
	got := e.ask(e.req, imgRef, "")
	x := uaSession(t, "sub-img-x", []string{"img-x"}, false)
	for name, c := range map[string]*http.Cookie{"a unit admin": e.adminA, "the child's group": x} {
		if w := e.call(c, http.MethodPost, requestPath(got.Pending.ID, "approve"), ""); !scopeRefused(t, w) && w.Code != http.StatusNotFound {
			t.Errorf("%s approving under the met rule = %d %s, want 404 or 403", name, w.Code, w.Body)
		}
	}
	if e.grantOf(imgReqSub, imgRef) != nil {
		t.Fatal("a refused approval wrote a grant")
	}
	if w := e.call(e.alice, http.MethodPost, requestPath(got.Pending.ID, "approve"), ""); w.Code != http.StatusOK {
		t.Errorf("security approving = %d %s", w.Code, w.Body)
	}
	if e.grantOf(imgReqSub, imgRef) == nil {
		t.Error("security's approval wrote no grant")
	}
}

func TestImageRuleRefusesElevationFields(t *testing.T) {
	e := newImgEnv(t)
	for field, v := range map[string]any{
		"eligible_requesters": []string{"group:img-team"},
		"renew_after":         "1h",
		"ticket_pattern":      "CHG-[0-9]+",
	} {
		rule := map[string]any{"kinds": []string{"image"}, "who": []string{"unit_admins"}, field: v}
		if w := e.call(e.alice, http.MethodPut, decidersPath(e.a), decidersBody(rule)); w.Code != http.StatusBadRequest || wireReason(t, w) != "invalid_deciders" {
			t.Errorf("an image rule with %s = %d %s, want 400 invalid_deciders", field, w.Code, w.Body)
		}
	}
}
