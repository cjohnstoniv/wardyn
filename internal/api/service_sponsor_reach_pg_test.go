// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/ssh"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The sponsor's reach over a service identity's runs (#1508, amended D11): read and stop, and entry only
// through an explicit service_attach grant, the run's own doors, and never over SSH.

const (
	bossSub = "boss-sub"
	bossBot = "ops-bot"
	bossPrn = "service:ops-bot"
)

// attentionApprovals gives the fake approval service the pending-approvals read the runs list's attention
// projection (view=...) asks of a real one.
type attentionApprovals struct {
	ApprovalService
	st store.PG
}

func (a attentionApprovals) ListPendingApprovalsForRuns(ctx context.Context, ids []uuid.UUID) ([]types.ApprovalRequest, error) {
	return a.st.ListPendingApprovalsForRuns(ctx, ids)
}

func (a attentionApprovals) CountPendingApprovals(ctx context.Context) (int, error) {
	return a.st.CountPendingApprovals(ctx)
}

func (a attentionApprovals) CountPendingApprovalsByRunCreator(ctx context.Context, by string) (int, error) {
	return a.st.CountPendingApprovalsByRunCreator(ctx, by)
}

// sponsorEnv is svcEnv with two service identities: ci-bot, sponsored by a member, and ops-bot, sponsored by a
// super admin.
type sponsorEnv struct {
	svcEnv
	boss   *http.Cookie
	runner *holderTestRunner
}

func newSponsorEnv(t *testing.T) sponsorEnv {
	t.Helper()
	e := sponsorEnv{svcEnv: newSvcEnv(t)}
	e.bind(t, bossSub, "https://idp.example", "", "")
	e.boss = accessSession(t, bossSub, "boss@corp.example", oidc.RoleAdmin, []string{})
	e.mustCreate(t, svcName, svcSponsor)
	e.mustCreate(t, bossBot, bossSub)
	ageSponsorships(t, e.pool)
	e.runner = &holderTestRunner{}
	e.h.srv.cfg.Runner = e.runner
	e.h.srv.cfg.Approvals = attentionApprovals{ApprovalService: e.h.srv.cfg.Approvals, st: e.st}
	return e
}

// ageSponsorships moves every sponsor_since an hour into the past. A sponsor's entry is admitted only when
// sponsor_since is not after the request's authority time, and that time is translated onto the database
// clock by an anchor that moves it EARLIER by however long the app took to read its clock after the database
// answered (a stall under load is tens of milliseconds, by design: toward revocation). A sponsorship written
// a few milliseconds before the request is then "not yet" the sponsor's, the mint is refused as
// attach_ticket_foreign_run, and a test asserting capability_feature flakes. Tests that exercise the grant
// switch, not the cutoff, age the sponsorship; the ones that exercise the cutoff do not call this until after.
func ageSponsorships(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ageSponsorshipsBy(t, pool, time.Hour)
}

// ageSponsorshipsBy is ageSponsorships by d, for a test whose own clock skew must stay larger than the age.
func ageSponsorshipsBy(t *testing.T, pool *pgxpool.Pool, d time.Duration) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `UPDATE service_identities SET sponsor_since = sponsor_since - make_interval(secs => $1)`, d.Seconds()); err != nil {
		t.Fatalf("age sponsorships: %v", err)
	}
}

// seedRun stores a RUNNING run of owner (a principal), with a sandbox, and applies mut before the write.
func (e sponsorEnv) seedRun(t *testing.T, owner string, mut func(*types.AgentRun)) types.AgentRun {
	t.Helper()
	id := uuid.New()
	run := types.AgentRun{
		ID: id, CreatedBy: owner, Agent: "claude-code", ConfinementClass: types.CC2, State: types.RunRunning,
		RunnerTarget: "docker", Task: "t", SPIFFEID: "spiffe://wardyn.local/agent-run/" + id.String(),
		SandboxRef: "wardyn-agent-" + id.String(), Interactive: true,
	}
	if mut != nil {
		mut(&run)
	}
	created, err := e.st.CreateRun(context.Background(), run)
	if err != nil {
		t.Fatalf("seed run for %s: %v", owner, err)
	}
	return created
}

// grant has the super admin write a feature service_attach grant and returns its id.
func (e sponsorEnv) grant(t *testing.T, subjectType, subject, effect, value string) uuid.UUID {
	t.Helper()
	body := fmt.Sprintf(`{"subject_type":%q,"subject":%q,"capability":"feature","value":%q,"effect":%q}`, subjectType, subject, value, effect)
	w := doSSO(t, e.h.srv, http.MethodPost, "/api/v1/permissions/grants", e.super, body)
	if w.Code/100 != 2 {
		t.Fatalf("grant %s %s %s %s: %d %s", effect, subjectType, subject, value, w.Code, w.Body.String())
	}
	var g types.CapabilityGrant
	if err := json.Unmarshal(w.Body.Bytes(), &g); err != nil || g.ID == uuid.Nil {
		t.Fatalf("grant response %s: %v", w.Body.String(), err)
	}
	return g.ID
}

func (e sponsorEnv) ungrant(t *testing.T, id uuid.UUID) {
	t.Helper()
	if w := doSSO(t, e.h.srv, http.MethodDelete, "/api/v1/permissions/grants/"+id.String(), e.super, ""); w.Code/100 != 2 {
		t.Fatalf("delete grant: %d %s", w.Code, w.Body.String())
	}
}

func (e sponsorEnv) mintTicket(t *testing.T, as *http.Cookie, run types.AgentRun) *httptest.ResponseRecorder {
	t.Helper()
	return doSSO(t, e.h.srv, http.MethodPost, "/api/v1/runs/"+run.ID.String()+"/attach-ticket", as, "")
}

func ticketOf(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var b struct {
		Ticket string `json:"ticket"`
	}
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &b) != nil || b.Ticket == "" {
		t.Fatalf("ticket mint: %d %s, want 200 with a ticket", w.Code, w.Body.String())
	}
	return b.Ticket
}

// refusedAsForeign reports the foreign-run 404 a member gets for a run that is not theirs.
func refusedAsForeign(w *httptest.ResponseRecorder) bool {
	return w.Code == http.StatusNotFound && errorReason(w) == reasonRunNotFound
}

// denialReasons are the reasons of the authz.denied rows written for run.
func (e sponsorEnv) denialReasons(run uuid.UUID) []string {
	var out []string
	for _, ev := range e.h.audit.snapshot() {
		if ev.Action != authz.AuditAction || ev.RunID == nil || *ev.RunID != run {
			continue
		}
		var d struct {
			Reason string `json:"reason"`
		}
		_ = json.Unmarshal(ev.Data, &d)
		out = append(out, d.Reason)
	}
	return out
}

// TestServiceIdentity_SponsorReadsAndKillsOnly: the current sponsor lists and reads the service's runs and can
// kill one; every other run route (files, resources, grants, run tokens, policy, profile, holder and take-over,
// and an approval's decision) stays the owner's or an admin's. Someone else's reach is unchanged.
func TestServiceIdentity_SponsorReadsAndKillsOnly(t *testing.T) {
	e := newSponsorEnv(t)
	run := e.seedRun(t, svcPrin, nil)
	done := e.seedRun(t, svcPrin, func(r *types.AgentRun) { r.State = types.RunCompleted })
	foreign := e.seedRun(t, bossPrn, nil)
	path := func(r types.AgentRun, suffix string) string { return "/api/v1/runs/" + r.ID.String() + suffix }

	// The list carries the service's runs for its sponsor, and only for its sponsor.
	listed := func(as *http.Cookie, query string) map[uuid.UUID]bool {
		w := doSSO(t, e.h.srv, http.MethodGet, "/api/v1/runs"+query, as, "")
		var runs []types.AgentRun
		if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &runs) != nil {
			t.Fatalf("GET /runs%s: %d %s", query, w.Code, w.Body.String())
		}
		got := map[uuid.UUID]bool{}
		for _, r := range runs {
			got[r.ID] = true
		}
		return got
	}
	// The console always sends a view; view=user (and a non-operator's view=admin, forced to user) is the
	// person's own list, which carries the identities they currently sponsor (design §8).
	for _, q := range []string{"", "?owner=me", "?owner=me&include_killed=1", "?view=user", "?view=user&include_killed=1", "?view=admin&include_killed=1"} {
		if got := listed(e.sponsor, q); !got[run.ID] || !got[done.ID] || got[foreign.ID] {
			t.Errorf("sponsor's GET /runs%s lists run=%v done=%v other service's run=%v, want true true false", q, got[run.ID], got[done.ID], got[foreign.ID])
		}
	}
	if got := listed(e.sponsor, "?owner=me&status=active"); !got[run.ID] || got[done.ID] || got[foreign.ID] {
		t.Errorf("sponsor's filtered list = %v, want the live run only", got)
	}
	for _, q := range []string{"", "?view=user&include_killed=1", "?owner=all&include_killed=1"} {
		if got := listed(e.other, q); got[run.ID] || got[done.ID] {
			t.Errorf("another member's GET /runs%s lists the service's runs", q)
		}
	}

	// Read.
	if w := doSSO(t, e.h.srv, http.MethodGet, path(run, ""), e.sponsor, ""); w.Code != http.StatusOK {
		t.Errorf("sponsor GET /runs/{id}: %d %s, want 200", w.Code, w.Body.String())
	}
	if w := doSSO(t, e.h.srv, http.MethodGet, path(done, "/output"), e.sponsor, ""); refusedAsForeign(w) {
		t.Errorf("sponsor GET /runs/{id}/output: %d %s, want past the read gate", w.Code, w.Body.String())
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if w := doSSOCtx(t, e.h.srv, ctx, http.MethodGet, path(done, "/events"), e.sponsor, ""); refusedAsForeign(w) {
		t.Errorf("sponsor GET /runs/{id}/events: %d %s, want past the read gate", w.Code, w.Body.String())
	}
	// A run of another service, and every route for another member, is the foreign-run 404.
	for _, as := range []*http.Cookie{e.other, e.sponsor2} {
		for _, p := range []string{path(run, ""), path(run, "/output"), path(run, "/events")} {
			if w := doSSO(t, e.h.srv, http.MethodGet, p, as, ""); !refusedAsForeign(w) {
				t.Errorf("a non-sponsor GET %s: %d %s, want the foreign-run 404", p, w.Code, w.Body.String())
			}
		}
		if w := doSSO(t, e.h.srv, http.MethodPost, path(run, "/kill"), as, ""); !refusedAsForeign(w) {
			t.Errorf("a non-sponsor kill: %d %s, want the foreign-run 404", w.Code, w.Body.String())
		}
	}
	if w := doSSO(t, e.h.srv, http.MethodGet, path(foreign, ""), e.sponsor, ""); !refusedAsForeign(w) {
		t.Errorf("the sponsor of one service reading another service's run: %d, want the foreign-run 404", w.Code)
	}

	// Nothing but read and stop.
	approval := seedEgressApproval(e.h, run.ID, "pypi.org")
	for _, c := range []struct{ method, p string }{
		{http.MethodGet, path(run, "/files")},
		{http.MethodGet, path(run, "/resources")},
		{http.MethodGet, path(run, "/grants")},
		{http.MethodGet, path(run, "/ado-tokens")},
		{http.MethodGet, path(run, "/policy")},
		{http.MethodPost, path(run, "/profile")},
		{http.MethodGet, path(run, "/attach-holder")},
		{http.MethodPost, path(run, "/attach/takeover")},
	} {
		if w := doSSO(t, e.h.srv, c.method, c.p, e.sponsor, ""); !refusedAsForeign(w) {
			t.Errorf("sponsor %s %s: %d %s, want the foreign-run 404", c.method, c.p, w.Code, w.Body.String())
		}
	}
	if w := doSSO(t, e.h.srv, http.MethodPost, "/api/v1/approvals/"+approval.String()+"/approve", e.sponsor, `{}`); w.Code/100 == 2 {
		t.Errorf("sponsor approving a held request: %d %s, want a refusal", w.Code, w.Body.String())
	}
	if ap := e.h.approvals.byID[approval]; ap.State == types.ApprovalApproved {
		t.Error("the sponsor's approve decided the hold")
	}

	// Kill.
	if w := doSSO(t, e.h.srv, http.MethodPost, path(run, "/kill"), e.sponsor, ""); w.Code != http.StatusAccepted {
		t.Fatalf("sponsor kill: %d %s, want 202", w.Code, w.Body.String())
	}
	if got, err := e.st.GetRun(context.Background(), run.ID); err != nil || got.State != types.RunKilled {
		t.Errorf("run after the sponsor's kill = %v (%v), want KILLED", got.State, err)
	}
	ev := waitForRecAudit(t, e.h.audit, run.ID, "run.kill", "success")
	if ev.ActorType != types.ActorHuman || ev.Actor != svcSponsor {
		t.Errorf("run.kill row actor_type=%q actor=%q, want the sponsor as a human", ev.ActorType, ev.Actor)
	}
}

// TestServiceIdentity_SponsorAttachIsOffByDefault: with no grant row a sponsor's ticket mint answers the
// foreign-run 404 and records capability_feature, a super admin sponsor's too (F6). Reading and stopping stay open.
func TestServiceIdentity_SponsorAttachIsOffByDefault(t *testing.T) {
	e := newSponsorEnv(t)
	for _, c := range []struct {
		name   string
		as     *http.Cookie
		owner  string
		reason string
	}{
		{"member sponsor", e.sponsor, svcPrin, "capability_feature"},
		{"super admin sponsor", e.boss, bossPrn, "capability_feature"},
	} {
		t.Run(c.name, func(t *testing.T) {
			run := e.seedRun(t, c.owner, nil)
			if w := e.mintTicket(t, c.as, run); !refusedAsForeign(w) {
				t.Fatalf("ticket mint with no grant: %d %s, want the foreign-run 404", w.Code, w.Body.String())
			}
			if got := e.denialReasons(run.ID); len(got) != 1 || got[0] != c.reason {
				t.Errorf("authz.denied reasons = %v, want [%s]", got, c.reason)
			}
			if w := doSSO(t, e.h.srv, http.MethodGet, "/api/v1/runs/"+run.ID.String(), c.as, ""); w.Code != http.StatusOK {
				t.Errorf("the same sponsor reading the run: %d, want 200", w.Code)
			}
		})
	}
	// Not a restriction a super admin's role lifts: the restriction row is what is read.
	restricted, err := e.st.ListCapabilityRestrictions(context.Background())
	if err != nil || !restricted["feature"][featureServiceAttach] {
		t.Errorf("capability_restrictions = %v (%v), want feature/service_attach restricted by the migration", restricted, err)
	}
}

// TestServiceIdentity_SponsorAttachFollowsTheGrant: a person grant, a group grant and a user-type grant each
// admit the sponsor; a `*` allow admits nobody; a user-type deny wins over an allow; a withdrawn grant refuses
// the next ticket. A grant never reaches someone who is not the sponsor.
func TestServiceIdentity_SponsorAttachFollowsTheGrant(t *testing.T) {
	e := newSponsorEnv(t)
	run := e.seedRun(t, svcPrin, nil)
	grouped := accessSession(t, svcSponsor, "sponsor@corp.example", oidc.RoleUser, []string{"svc-ops"})
	refused := func(t *testing.T, as *http.Cookie, why string) {
		t.Helper()
		if w := e.mintTicket(t, as, run); !refusedAsForeign(w) {
			t.Fatalf("%s: ticket mint %d %s, want the foreign-run 404", why, w.Code, w.Body.String())
		}
	}
	allowed := func(t *testing.T, as *http.Cookie, why string) {
		t.Helper()
		if w := e.mintTicket(t, as, run); w.Code != http.StatusOK {
			t.Fatalf("%s: ticket mint %d %s, want 200", why, w.Code, w.Body.String())
		}
	}

	wild := e.grant(t, "all", "", "allow", "*")
	refused(t, e.sponsor, "a * allow")
	refused(t, e.sponsor, "a * allow, asked again")
	e.ungrant(t, wild)

	person := e.grant(t, "user", "sponsor@corp.example", "allow", featureServiceAttach)
	allowed(t, e.sponsor, "a person grant")
	// Someone who is not the sponsor reaches nothing through the sponsor's grant.
	e.grant(t, "user", "other@corp.example", "allow", featureServiceAttach)
	if w := e.mintTicket(t, e.other, run); !refusedAsForeign(w) {
		t.Errorf("a granted non-sponsor: %d %s, want the foreign-run 404", w.Code, w.Body.String())
	}
	e.ungrant(t, person)
	refused(t, e.sponsor, "the person grant withdrawn")

	group := e.grant(t, "group", "svc-ops", "allow", featureServiceAttach)
	allowed(t, grouped, "a group grant")
	refused(t, e.sponsor, "a group grant, the sponsor's session without the group")
	e.ungrant(t, group)

	userType := e.grant(t, "user_type", types.UserTypeStandard, "allow", featureServiceAttach)
	allowed(t, e.sponsor, "a user-type grant")
	e.ungrant(t, userType)
	refused(t, e.sponsor, "the user-type grant withdrawn")

	// A user-type deny walls out a person allow.
	e.grant(t, "user", "sponsor@corp.example", "allow", featureServiceAttach)
	deny := e.grant(t, "user_type", types.UserTypeStandard, "deny", featureServiceAttach)
	refused(t, e.sponsor, "a user-type deny beside a person allow")
	e.ungrant(t, deny)
	allowed(t, e.sponsor, "the deny withdrawn")
}

// TestServiceIdentity_SponsorAttachKeepsTheDoors: a super admin sponsor with the grant enters through the run's
// own doors: the run's deny_interactive refuses them, with WARDYN_GOVERN_ADMIN_RUNS off, where a super admin's own
// run would pass; a super admin's cookie lane never reaches the run; a member sponsor is refused alike.
func TestServiceIdentity_SponsorAttachKeepsTheDoors(t *testing.T) {
	e := newSponsorEnv(t)
	if e.h.srv.cfg.GovernAdminRuns {
		t.Fatal("the fixture must run with WARDYN_GOVERN_ADMIN_RUNS off")
	}
	profile, err := e.st.UpsertGovernanceProfile(context.Background(), types.GovernanceProfile{
		ID: uuid.New(), Name: "walled", Ceiling: govProfileSpec(), Limits: types.GovernanceLimits{DenyInteractive: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	walled := func(r *types.AgentRun) { r.GovernanceProfileID = &profile.ID }
	e.grant(t, "user", "boss@corp.example", "allow", featureServiceAttach)
	e.grant(t, "user", "sponsor@corp.example", "allow", featureServiceAttach)
	attach := func(run types.AgentRun, tok string) *httptest.ResponseRecorder {
		return do(t, e.h.srv, http.MethodGet, "/api/v1/runs/"+run.ID.String()+"/attach?ticket="+tok, "", "")
	}
	refusedByProfile := func(w *httptest.ResponseRecorder) bool {
		return w.Code == http.StatusForbidden && errorReason(w) == string(authz.ReasonGovernanceProfile)
	}

	for _, c := range []struct {
		name  string
		as    *http.Cookie
		owner string
	}{{"super admin sponsor", e.boss, bossPrn}, {"member sponsor", e.sponsor, svcPrin}} {
		t.Run(c.name, func(t *testing.T) {
			closed := e.seedRun(t, c.owner, walled)
			if w := attach(closed, ticketOf(t, e.mintTicket(t, c.as, closed))); !refusedByProfile(w) {
				t.Errorf("deny_interactive run: %d %s, want 403 governance_profile", w.Code, w.Body.String())
			}
			open := e.seedRun(t, c.owner, nil)
			if w := attach(open, ticketOf(t, e.mintTicket(t, c.as, open))); w.Code != http.StatusUpgradeRequired {
				t.Errorf("run with no limit: %d %s, want the 426 of a gate that passed", w.Code, w.Body.String())
			}
		})
	}
	// The cookie lane (no ticket) is the super admin's break-glass for operator-owned runs only.
	run := e.seedRun(t, bossPrn, nil)
	if w := doSSO(t, e.h.srv, http.MethodGet, "/api/v1/runs/"+run.ID.String()+"/attach", e.boss, ""); w.Code != http.StatusForbidden || errorReason(w) != string(authz.ReasonRunOwnerOnly) {
		t.Errorf("a super admin sponsor's cookie lane: %d %s, want 403 run_owner_only", w.Code, w.Body.String())
	}
	// The same super admin on their own run, with the same limit and the switch off, takes the exemption:
	// the sponsor arm's refusal is the stamp, not the profile.
	own := e.seedRun(t, "boss-sub", walled)
	tok, err := mintAttachTicket(context.Background(), e.st, own.ID, types.ActorHuman, "boss-sub", oidc.RoleAdmin, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if w := attach(own, tok); w.Code != http.StatusUpgradeRequired {
		t.Errorf("a super admin's own deny_interactive run, switch off: %d %s, want the exemption (426)", w.Code, w.Body.String())
	}
}

// TestServiceIdentity_SponsorAttachRecordsTheSponsor: the attach rows name the sponsor as the human actor,
// with the service identity and entry=sponsor, and the ticket is stamped member whatever the minter's role.
func TestServiceIdentity_SponsorAttachRecordsTheSponsor(t *testing.T) {
	e := newSponsorEnv(t)
	e.grant(t, "user", "boss@corp.example", "allow", featureServiceAttach)
	run := e.seedRun(t, bossPrn, nil)
	tok := ticketOf(t, e.mintTicket(t, e.boss, run))
	ts := httptest.NewServer(panicFails(t, e.h.srv.Handler()))
	defer ts.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(ts.URL, "http")+"/api/v1/runs/"+run.ID.String()+"/attach?ticket="+tok, nil)
	if err != nil {
		t.Fatalf("attach with the sponsor's ticket: %v", err)
	}
	defer func() { _ = c.CloseNow() }()

	ev := waitForRecAudit(t, e.h.audit, run.ID, "session.attach", "success")
	var d struct {
		ServiceIdentity string `json:"service_identity"`
		Entry           string `json:"entry"`
	}
	_ = json.Unmarshal(ev.Data, &d)
	if ev.ActorType != types.ActorHuman || ev.Actor != bossSub || d.ServiceIdentity != bossBot || d.Entry != "sponsor" {
		t.Errorf("session.attach row actor_type=%q actor=%q service_identity=%q entry=%q, want human, %q, %q, sponsor",
			ev.ActorType, ev.Actor, d.ServiceIdentity, d.Entry, bossSub, bossBot)
	}
	// An ordinary owner's rows carry neither key.
	mine := e.seedRun(t, bossSub, nil)
	if got := withSponsorEntry(mine, map[string]any{"a": 1}); len(got) != 1 {
		t.Errorf("a person's run adds %v to its attach rows, want nothing", got)
	}
}

// TestServiceIdentity_SponsorEntryEndsWithSponsorship: a ticket redeemed after the sponsor changed, or the
// identity was suspended or retired, is refused; so is a ticket minted before a sponsor change that lands in
// the same instant window. A withdrawn grant refuses the next ticket and ends nothing already minted.
func TestServiceIdentity_SponsorEntryEndsWithSponsorship(t *testing.T) {
	e := newSponsorEnv(t)
	grant := e.grant(t, "user", "sponsor@corp.example", "allow", featureServiceAttach)
	run := e.seedRun(t, svcPrin, nil)
	attach := func(tok string) int {
		return do(t, e.h.srv, http.MethodGet, "/api/v1/runs/"+run.ID.String()+"/attach?ticket="+tok, "", "").Code
	}

	// A ticket already minted outlives a withdrawn grant, which only refuses the next mint.
	live := ticketOf(t, e.mintTicket(t, e.sponsor, run))
	e.ungrant(t, grant)
	if got := attach(live); got != http.StatusUpgradeRequired {
		t.Errorf("a minted ticket after the grant was withdrawn: %d, want 426 (the grant is asked at the mint only)", got)
	}
	e.grant(t, "user", "sponsor@corp.example", "allow", featureServiceAttach)

	// Sponsor change A to B and back to A: the person is the sponsor again, but not the one the ticket was
	// admitted under, so the redemption is refused. The authority time is the ticket's.
	roundTrip := ticketOf(t, e.mintTicket(t, e.sponsor, run))
	time.Sleep(20 * time.Millisecond)
	for _, to := range []string{svcSponsor2, svcSponsor} {
		if w := doSSO(t, e.h.srv, http.MethodPatch, "/api/v1/service-identities/"+svcName, e.sec, `{"sponsor":"`+to+`"}`); w.Code != http.StatusOK {
			t.Fatalf("sponsor change to %s: %d %s", to, w.Code, w.Body.String())
		}
	}
	if got := attach(roundTrip); got != http.StatusForbidden {
		t.Errorf("a ticket redeemed after A to B to A: %d, want 403", got)
	}
	ageSponsorships(t, e.pool)
	if w := e.mintTicket(t, e.sponsor, run); w.Code != http.StatusOK {
		t.Errorf("the sponsor's fresh mint after A to B to A: %d %s, want 200", w.Code, w.Body.String())
	}

	// Sponsor change.
	changed := ticketOf(t, e.mintTicket(t, e.sponsor, run))
	time.Sleep(20 * time.Millisecond)
	if w := doSSO(t, e.h.srv, http.MethodPatch, "/api/v1/service-identities/"+svcName, e.sec, `{"sponsor":"`+svcSponsor2+`"}`); w.Code != http.StatusOK {
		t.Fatalf("sponsor change: %d %s", w.Code, w.Body.String())
	}
	if got := attach(changed); got != http.StatusForbidden {
		t.Errorf("a ticket redeemed after the sponsor changed: %d, want 403", got)
	}
	ageSponsorships(t, e.pool)
	if w := e.mintTicket(t, e.sponsor, run); !refusedAsForeign(w) {
		t.Errorf("the previous sponsor's mint: %d %s, want the foreign-run 404", w.Code, w.Body.String())
	}

	// Suspension.
	e.grant(t, "user", "sponsor2@corp.example", "allow", featureServiceAttach)
	suspended := ticketOf(t, e.mintTicket(t, e.sponsor2, run))
	if w := doSSO(t, e.h.srv, http.MethodPatch, "/api/v1/service-identities/"+svcName, e.sec, `{"suspended":true}`); w.Code != http.StatusOK {
		t.Fatalf("suspend: %d %s", w.Code, w.Body.String())
	}
	if got := attach(suspended); got != http.StatusForbidden {
		t.Errorf("a ticket redeemed after the identity was suspended: %d, want 403", got)
	}
	if w := e.mintTicket(t, e.sponsor2, run); !refusedAsForeign(w) {
		t.Errorf("a mint for a suspended identity: %d, want the foreign-run 404", w.Code)
	}
	// Reading and stopping do not need the identity active: the sponsor can still stop its live runs.
	if w := doSSO(t, e.h.srv, http.MethodPost, "/api/v1/runs/"+run.ID.String()+"/kill", e.sponsor2, ""); w.Code != http.StatusAccepted {
		t.Errorf("the current sponsor's kill under a suspended identity: %d %s, want 202", w.Code, w.Body.String())
	}
}

// fakeSSHConn is the connection metadata sshAuth reads: the user name is the run id.
type fakeSSHConn struct{ user string }

func (c fakeSSHConn) User() string         { return c.user }
func (fakeSSHConn) SessionID() []byte      { return []byte("session") }
func (fakeSSHConn) ClientVersion() []byte  { return []byte("SSH-2.0-test") }
func (fakeSSHConn) ServerVersion() []byte  { return []byte("SSH-2.0-wardyn") }
func (fakeSSHConn) RemoteAddr() net.Addr   { return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 2222} }
func (fakeSSHConn) LocalAddr() net.Addr    { return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 22} }
func sshKeyFor(t *testing.T) ssh.PublicKey { _, pub := mustSSHKeypair(t); return pub }

// TestServiceIdentity_SSHRefusesSponsorEntry: whatever grant the sponsor holds, and whatever their key's role,
// SSH into a service identity's run is refused run_owner_only, and the failure row carries no sponsor-entry data.
func TestServiceIdentity_SSHRefusesSponsorEntry(t *testing.T) {
	e := newSponsorEnv(t)
	e.grant(t, "user", "sponsor@corp.example", "allow", featureServiceAttach)
	e.grant(t, "user", "boss@corp.example", "allow", featureServiceAttach)
	fresh := time.Now()
	for _, c := range []struct {
		name, principal, role, owner string
	}{
		{"member sponsor", svcSponsor, oidc.RoleUser, svcPrin},
		{"super admin sponsor", bossSub, oidc.RoleAdmin, bossPrn},
	} {
		t.Run(c.name, func(t *testing.T) {
			run := e.seedRun(t, c.owner, nil)
			pub := sshKeyFor(t)
			if _, err := e.st.AddSSHKey(context.Background(), types.SSHPublicKey{
				Fingerprint: ssh.FingerprintSHA256(pub), Principal: c.principal, PublicKey: string(ssh.MarshalAuthorizedKey(pub)),
				Role: c.role, RoleCheckedAt: &fresh,
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := e.h.srv.sshAuth(fakeSSHConn{user: run.ID.String()}, pub); err == nil {
				t.Fatal("sshAuth admitted the sponsor of a service identity's run")
			}
			ev := waitForRecAudit(t, e.h.audit, run.ID, "ssh.authenticate", "failure")
			var d map[string]any
			_ = json.Unmarshal(ev.Data, &d)
			if d["reason"] != runOwnerOnlyReason || len(d) != 1 {
				t.Errorf("ssh.authenticate failure data = %v, want only reason=%s", d, runOwnerOnlyReason)
			}
		})
	}
}

// TestServiceIdentity_SponsoredRunNeedsNoActionFromTheSponsor: the user view lists a sponsored run, but only
// the run's owner can revive it or clear its credential holds, so its attention is "owner", never "you":
// the sponsor's Revive button does not appear, `status=needs` omits the run, and the nav badge never counted
// it. The sponsor's own lost run is still "you", listed under needs and counted.
func TestServiceIdentity_SponsoredRunNeedsNoActionFromTheSponsor(t *testing.T) {
	e := newSponsorEnv(t)
	lose := func(r types.AgentRun) {
		t.Helper()
		if _, err := e.pool.Exec(context.Background(), `UPDATE agent_runs SET lost_at = now(), lost_reason = 'outage' WHERE id = $1`, r.ID); err != nil {
			t.Fatal(err)
		}
	}
	sponsored := e.seedRun(t, svcPrin, nil)
	own := e.seedRun(t, svcSponsor, nil)
	lose(sponsored)
	lose(own)

	attention := func(path string) map[uuid.UUID]*types.RunAttention {
		t.Helper()
		w := doSSO(t, e.h.srv, http.MethodGet, path, e.sponsor, "")
		var runs []types.AgentRun
		if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &runs) != nil {
			t.Fatalf("GET %s: %d %s", path, w.Code, w.Body.String())
		}
		out := map[uuid.UUID]*types.RunAttention{}
		for _, r := range runs {
			out[r.ID] = r.Attention
		}
		return out
	}
	got := attention("/api/v1/runs?view=user")
	if a, ok := got[sponsored.ID]; !ok || a == nil || a.Kind != types.AttentionLost || a.By != types.AttentionOwner {
		t.Errorf("the sponsored lost run in view=user: listed=%v attention=%+v, want {lost owner}", ok, a)
	}
	if a := got[own.ID]; a == nil || a.By != types.AttentionYou {
		t.Errorf("the sponsor's own lost run: attention=%+v, want {lost you}", a)
	}
	needs := attention("/api/v1/runs?view=user&status=needs")
	if _, ok := needs[sponsored.ID]; ok {
		t.Error("status=needs lists the sponsored run: the sponsor can act on nothing there")
	}
	if _, ok := needs[own.ID]; !ok {
		t.Error("status=needs omits the sponsor's own lost run")
	}
	w := doSSO(t, e.h.srv, http.MethodGet, "/api/v1/me/attention", e.sponsor, "")
	var badge struct {
		NeedsYou int `json:"needs_you"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &badge); err != nil || w.Code != http.StatusOK || badge.NeedsYou != len(needs) {
		t.Errorf("/me/attention = %d %s, want needs_you equal to the %d runs under status=needs", w.Code, w.Body.String(), len(needs))
	}
	// The same rule for the credential holds, whose "you" was the owner's (rows 6 and 7).
	s := attentionFixture(t)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/runs", nil).WithContext(memberCtxFor(svcSponsor))
	for _, c := range []struct {
		name, scope string
		kind        types.AttentionKind
	}{
		{"ado consent", `{"lane":"azure_devops","mechanism":"entra_signin","owner":"service:ci-bot","provider_id":"p1"}`, types.AttentionADOConsent},
		{"reauth", `{"credential_source":"aws_sso"}`, types.AttentionReauth},
	} {
		pending := []types.ApprovalRequest{pendingRow(sponsored.ID, types.ApprovalCredentialReauth, c.scope, nil)}
		s.projectHolds(pending)
		if a := s.attention(req, sponsored, pending, false); a == nil || a.Kind != c.kind || a.By != types.AttentionOwner {
			t.Errorf("%s hold on a sponsored run: attention=%+v, want {%s owner}", c.name, a, c.kind)
		}
	}
}
