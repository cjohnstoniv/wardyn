// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// security_admin_test.go pins the 0.7 THREE-TIER model's api-side seams: the
// second predicate (isSecurityOperator) and its middleware, the role-SNAPSHOT
// split that keeps a security admin off other people's runs, and the
// no-capability-reaches-admin invariant that makes handing them /permissions
// safe.
//
// The route-family matrix (which routes register on securityOps vs
// operatorOnly) is NOT here — it lives with the chi.Walk exhaustive pin in
// authz_test.go, which classifies every gated route.
package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const (
	secAdminSub  = "sub-secadmin"
	secAdminMail = "sec@corp.example"
)

// ─── the predicate ─────────────────────────────────────────────────────────

// TestIsSecurityOperator is isSecurityOperator's twin of TestIsOperator
// (rbac_test.go). The two predicates agree on everything EXCEPT the
// security_admin role, and that single disagreement is the whole feature: any
// change making them agree there has deleted the tier.
func TestIsSecurityOperator(t *testing.T) {
	for _, tc := range []struct {
		name             string
		ctx              context.Context
		wantSec, wantSup bool
	}{
		{"admin token: no session, both tiers", context.Background(), true, true},
		{"local mode: no session, both tiers", withLocalPrincipal(context.Background(), "local:alice"), true, true},
		{"sso admin: both tiers", operatorCtx("sub-1", rbacOperator, oidc.RoleAdmin), true, true},
		// THE distinguishing row.
		{"sso security_admin: security tier only", operatorCtx(secAdminSub, secAdminMail, oidc.RoleSecurityAdmin), true, false},
		{"sso member: neither", operatorCtx("sub-2", rbacViewer, oidc.RoleMember), false, false},
		// Defense-in-depth, same as isOperator's: decodeSession refuses an
		// empty role outright, but both predicates must fail CLOSED if it ever
		// reached them.
		{"sso empty role: fails closed on both", operatorCtx("sub-3", rbacViewer, ""), false, false},
		{"sso unknown role: fails closed on both", operatorCtx("sub-4", rbacViewer, "superadmin"), false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &Server{}
			if got := s.isSecurityOperator(tc.ctx); got != tc.wantSec {
				t.Errorf("isSecurityOperator = %v, want %v", got, tc.wantSec)
			}
			// Pinned side by side so a change that quietly widens isOperator to
			// cover the security tier (the ladder this design refuses) fails
			// HERE rather than silently at an ssh-key stamp.
			if got := s.isOperator(tc.ctx); got != tc.wantSup {
				t.Errorf("isOperator = %v, want %v", got, tc.wantSup)
			}
		})
	}
}

// TestRequireSecurityOperator drives the middleware directly — it gates the
// securityOps router group, whose route registrations land with the matrix
// slice, so the unit shape is what is provable here: who passes, the 403 body,
// and the audit reason that distinguishes a security-surface denial from an
// admin-surface one.
func TestRequireSecurityOperator(t *testing.T) {
	for _, tc := range []struct {
		name       string
		role       string
		wantStatus int
	}{
		{"admin passes", oidc.RoleAdmin, http.StatusOK},
		{"security_admin passes", oidc.RoleSecurityAdmin, http.StatusOK},
		{"member is refused", oidc.RoleMember, http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			srv := New(baseTestConfig(h, nil))
			var reached bool
			mw := srv.requireSecurityOperator(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				reached = true
				w.WriteHeader(http.StatusOK)
			}))
			r := httptest.NewRequest(http.MethodPost, "/api/v1/governance/profiles", nil)
			r = r.WithContext(operatorCtx("sub-x", "x@corp.example", tc.role))
			w := httptest.NewRecorder()
			mw.ServeHTTP(w, r)

			if w.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body %q)", w.Code, tc.wantStatus, w.Body.String())
			}
			if reached != (tc.wantStatus == http.StatusOK) {
				t.Fatalf("handler reached = %v at status %d", reached, w.Code)
			}
			if tc.wantStatus != http.StatusForbidden {
				return
			}
			// The body must NOT name which of the two tiers this route sits on
			// — that is a map of the deployment's admin surface. Byte-identical
			// to requireOperator's.
			if got := w.Body.String(); !json.Valid(w.Body.Bytes()) || !strings.Contains(got, "requires admin role") {
				t.Fatalf("403 body = %q, want requireOperator's exact wording", got)
			}
			var found bool
			for _, ev := range h.audit.events {
				if ev.Action != "authz.denied" {
					continue
				}
				var d map[string]any
				if err := json.Unmarshal(ev.Data, &d); err != nil {
					t.Fatalf("decode audit data: %v", err)
				}
				if d["reason"] == "security_admin_surface" {
					found = true
				}
			}
			if !found {
				t.Fatalf("no authz.denied with reason security_admin_surface; events=%+v", h.audit.events)
			}
		})
	}
}

// ─── the role-snapshot split (PF-7 / PF-19) ────────────────────────────────

// TestAPITokenStampsRealSecurityAdminRole: the API token carries the human's
// WHOLE session identity, so it stamps security_admin verbatim (PF-19).
// Without this, every securityOps route — including the governance family that
// ships API/CLI-first — is unreachable by token for exactly the persona it is
// built for, because the token would replay as a member.
func TestAPITokenStampsRealSecurityAdminRole(t *testing.T) {
	srv, _, _ := apiTokenTestServer(t)
	for _, tc := range []struct{ role string }{
		{oidc.RoleAdmin}, {oidc.RoleSecurityAdmin}, {oidc.RoleMember},
	} {
		t.Run(tc.role, func(t *testing.T) {
			sess := ssoSession(t, "sub-"+tc.role, tc.role+"@corp.example", tc.role)
			_, created := mintToken(t, srv, sess, "cli")
			if created.Role != tc.role {
				t.Fatalf("stamped role = %q, want the session's own %q", created.Role, tc.role)
			}
		})
	}
}

// TestSecurityAdminTokenGrantsNoForeignRunReach is PF-19's SAFETY half. The
// three consumers that turn a stamped role into reach INTO someone else's run
// all compare against oidc.RoleAdmin exactly (attach.go's ticket lane,
// uigateway.go, sshgateway.go), so stamping the real three-valued role gains
// the security admin zero run reach. Pinned as the comparison itself: the
// route-level proof rides the matrix slice, but the invariant those three
// checks depend on is that security_admin is NOT RoleAdmin, ever.
func TestSecurityAdminTokenGrantsNoForeignRunReach(t *testing.T) {
	if oidc.RoleSecurityAdmin == oidc.RoleAdmin {
		t.Fatal("security_admin collapsed onto admin: every == RoleAdmin foreign-run check would now pass")
	}
	// The context a security-admin TOKEN publishes (withHumanIdentity, the
	// same function the session branch uses) must read as not-an-operator, so
	// every isOperator-gated lane refuses it identically to a member's.
	ctx := withHumanIdentity(context.Background(), secAdminSub, secAdminMail, oidc.RoleSecurityAdmin, nil, false)
	s := &Server{}
	if s.isOperator(ctx) {
		t.Fatal("a security_admin token context reads as a super admin")
	}
	if !s.isSecurityOperator(ctx) {
		t.Fatal("a security_admin token context does not reach the security tier")
	}
}

// secAdminRunStore is the minimal store the ssh-key and attach-ticket stamp
// paths touch: one run, plus the ssh-key write. Every other method panics via
// the embedded nil store.Store, this package's fake convention.
type secAdminRunStore struct {
	store.Store
	mu   sync.Mutex
	run  types.AgentRun
	keys []types.SSHPublicKey
}

func (s *secAdminRunStore) GetRun(_ context.Context, id uuid.UUID) (types.AgentRun, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if id != s.run.ID {
		return types.AgentRun{}, store.ErrNotFound
	}
	return s.run, nil
}

func (s *secAdminRunStore) AddSSHKey(_ context.Context, k types.SSHPublicKey) (types.SSHPublicKey, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.keys = append(s.keys, k)
	return k, nil
}

func (s *secAdminRunStore) ListSSHKeysByPrincipal(context.Context, string) ([]types.SSHPublicKey, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.keys, nil
}

func (s *secAdminRunStore) MintAttachTicket(context.Context, string, store.AttachTicket, time.Time, time.Time) error {
	return nil
}

// TestSSHKeyNeverStampsSecurityAdmin: the key's role field means exactly
// "reaches runs its holder does not own" (sshgateway's == RoleAdmin check), so
// a security admin's key stamps MEMBER. This is the ladder-killing asymmetry —
// a tier ladder here would put an interactive shell in every developer's
// sandbox.
func TestSSHKeyNeverStampsSecurityAdmin(t *testing.T) {
	h := newHarness(t)
	st := &secAdminRunStore{}
	cfg := baseTestConfig(h, st)
	cfg.OIDC = &oidc.Authenticator{}
	srv := New(cfg)

	sess := ssoSession(t, secAdminSub, secAdminMail, oidc.RoleSecurityAdmin)
	body := `{"name":"laptop","public_key":"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIBl3jvXfmZbBd3q5aLKZTv3rIcvKlfz2eYQpuYSGfCPT sec@laptop"}`
	w := doSSO(t, srv, http.MethodPost, "/api/v1/me/ssh-keys", sess, body)
	if w.Code != http.StatusCreated {
		t.Fatalf("add key: code = %d, want 201; body=%s", w.Code, w.Body.String())
	}
	var added types.SSHPublicKey
	if err := json.Unmarshal(w.Body.Bytes(), &added); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if added.Role != oidc.RoleMember {
		t.Fatalf("ssh key stamped role %q for a security admin, want %q (the field means foreign-run reach)", added.Role, oidc.RoleMember)
	}
}

// TestAttachTicketRefusesForeignRunForSecurityAdmin is PF-9. ownsRunOrAdmin
// deliberately passes a security admin on ANY run (kill = incident response),
// so WITHOUT the explicit strict guard in handleAttachTicket this route would
// hand them an interactive shell in a foreign sandbox. The refusal is the
// byte-identical 404 a foreign member gets — no existence oracle — under its
// own audit reason.
func TestAttachTicketRefusesForeignRunForSecurityAdmin(t *testing.T) {
	h := newHarness(t)
	runID := uuid.New()
	st := &secAdminRunStore{run: types.AgentRun{ID: runID, CreatedBy: "someone-else", State: types.RunRunning}}
	cfg := baseTestConfig(h, st)
	cfg.OIDC = &oidc.Authenticator{}
	srv := New(cfg)

	sess := ssoSession(t, secAdminSub, secAdminMail, oidc.RoleSecurityAdmin)
	w := doSSO(t, srv, http.MethodPost, "/api/v1/runs/"+runID.String()+"/attach-ticket", sess, "")
	if w.Code != http.StatusNotFound {
		t.Fatalf("foreign attach-ticket mint: code = %d, want 404 (no shell in a foreign sandbox); body=%s", w.Code, w.Body.String())
	}
	var found bool
	for _, ev := range h.audit.events {
		if ev.Action != "authz.denied" {
			continue
		}
		var d map[string]any
		if err := json.Unmarshal(ev.Data, &d); err == nil && d["reason"] == "attach_ticket_foreign_run" {
			found = true
		}
	}
	if !found {
		t.Fatalf("no authz.denied with reason attach_ticket_foreign_run; events=%+v", h.audit.events)
	}
}

// TestAttachTicketOwnRunStampsMemberForSecurityAdmin: their OWN run still
// mints (the strict guard is about FOREIGN runs), and the ticket carries
// member — the same never-stamp rule the ssh key follows, for the same reason.
func TestAttachTicketOwnRunStampsMemberForSecurityAdmin(t *testing.T) {
	h := newHarness(t)
	runID := uuid.New()
	st := &secAdminRunStore{run: types.AgentRun{ID: runID, CreatedBy: secAdminSub, State: types.RunRunning}}
	cfg := baseTestConfig(h, st)
	cfg.OIDC = &oidc.Authenticator{}
	srv := New(cfg)

	sess := ssoSession(t, secAdminSub, secAdminMail, oidc.RoleSecurityAdmin)
	w := doSSO(t, srv, http.MethodPost, "/api/v1/runs/"+runID.String()+"/attach-ticket", sess, "")
	if w.Code != http.StatusOK {
		t.Fatalf("own-run attach-ticket mint: code = %d, want 200; body=%s", w.Code, w.Body.String())
	}
}

// ─── the no-capability-reaches-admin invariant (PF-8) ──────────────────────

// TestCapabilityGrantsNeverReachTheAdminTier: capAllowed/capGranted short-
// circuit ONLY for a super admin. A security admin is capability-bounded like
// a member — they may self-grant through /permissions, audited, and still
// reach nothing the exemption would give them. This is what makes handing them
// /permissions safe at all: no capability kind, present or future, can widen
// the admin tier.
func TestCapabilityGrantsNeverReachTheAdminTier(t *testing.T) {
	s := &Server{} // no Store: a non-exempt caller CANNOT resolve to allowed here
	admin := operatorCtx("sub-a", rbacOperator, oidc.RoleAdmin)
	sec := operatorCtx(secAdminSub, secAdminMail, oidc.RoleSecurityAdmin)

	for _, kind := range capabilityKinds {
		if ok, err := s.capAllowed(admin, kind, "x"); !ok || err != nil {
			t.Fatalf("capAllowed(%s) for an admin = %v/%v, want the operator short-circuit", kind, ok, err)
		}
		// The security admin must fall THROUGH to grant resolution, not take
		// the exemption. With no store that is an error, never a silent allow.
		if ok, err := s.capAllowed(sec, kind, "x"); ok || err == nil {
			t.Fatalf("capAllowed(%s) for a security admin = %v/%v, want no exemption", kind, ok, err)
		}
		if ok, err := s.capGranted(sec, kind, "x"); ok || err != nil {
			t.Fatalf("capGranted(%s) for a security admin = %v/%v, want no exemption", kind, ok, err)
		}
	}
}

// recordTierStore serves one member-owned workspace, which is all the two
// authorization questions below need: may this tier reach the record route, and
// may it read the workspace it would be recording.
type recordTierStore struct {
	store.Store
	ws types.Workspace
}

func (s *recordTierStore) GetWorkspace(_ context.Context, id uuid.UUID) (types.Workspace, error) {
	if id != s.ws.ID {
		return types.Workspace{}, store.ErrNotFound
	}
	return s.ws, nil
}

// TestRecordWorkspaceIsSuperAdminOnly is the tier decision for
// POST /workspaces/{id}/record, and it is a TIER decision rather than a guard.
//
// The route sat on securityOps, grouped with the workspace EGRESS-DECISION lane
// because recording is how the hosts promote-egress promotes get observed. But
// record does not DECIDE anything — it LAUNCHES an interactive sandbox with
// open egress by default, the workspace's local_dir bind-mounted (read-write
// when the owning member ticked Writable), the repo clone credential minted, the
// workspace's required secret:/integration: rows folded into proxy-side
// injections, and the operator's LLM credential attached. Those are the three
// axes routes.go defines the security tier as never reaching (into a run,
// credential material, the host), and they are the exact criterion
// authz_test.go states for keeping llm-cred and requirements on classAdmin.
//
// Worse, the run was stamped CreatedBy = the CALLER, so every downstream guard
// that keys on run OWNERSHIP passed: handleAttachTicket's explicit strict
// re-check refuses a security admin a PTY in a FOREIGN sandbox
// (TestAttachTicketRefusesForeignRunForSecurityAdmin above), and a run they
// launched themselves is not foreign — TestAttachTicketOwnRunStampsMemberForSecurityAdmin
// asserts that own-run mint returns 200. The escalation was to become the owner.
//
// The second assertion is the one that settles it as a mistake rather than a
// trade-off: the SAME tier is refused a plain GET of that workspace
// (getWorkspaceReadable → ownsWorkspaceOrAdmin, deliberately isOperator). A
// surface that lets a principal launch a credentialed sandbox over a row they
// are answered 404 for is not a considered delegation.
func TestRecordWorkspaceIsSuperAdminOnly(t *testing.T) {
	h := newHarness(t)
	ws := types.Workspace{
		ID: uuid.New(), Name: "victim", OwnedBy: "sub-victim-member", Status: types.WorkspaceScanned,
		Sources: []types.WorkspaceSource{{
			Type: types.WorkspaceSourceTypeLocalDir, Path: "/home/victim/projects/app",
			Target: "/home/agent/work", Writable: true,
		}},
	}
	cfg := baseTestConfig(h, &recordTierStore{ws: ws})
	cfg.OIDC = &oidc.Authenticator{}
	srv := New(cfg)
	sess := ssoSession(t, secAdminSub, secAdminMail, oidc.RoleSecurityAdmin)
	path := "/api/v1/workspaces/" + ws.ID.String()

	// The tier is refused at the ROUTER, so the refusal is a constant 403 that
	// never varies with whether the workspace exists — no existence oracle, and
	// nothing in the handler to keep in step with it.
	if w := doSSO(t, srv, http.MethodPost, path+"/record", sess, `{"name":"exfil"}`); w.Code != http.StatusForbidden {
		t.Fatalf("security_admin POST record = %d, want 403 — this route LAUNCHES a credentialed, host-mounting, "+
			"open-egress sandbox and stamps the caller as its owner, which is the super admin's tier; body=%s",
			w.Code, w.Body.String())
	}
	// A workspace id that does not exist answers the SAME 403, proving the gate
	// is the router and not a handler that first looked the row up.
	if w := doSSO(t, srv, http.MethodPost, "/api/v1/workspaces/"+uuid.NewString()+"/record", sess, `{"name":"exfil"}`); w.Code != http.StatusForbidden {
		t.Errorf("security_admin POST record on a MISSING workspace = %d, want the same 403", w.Code)
	}
	// THE CORROBORATING LEG IS GONE, AND THE TIER STILL HOLDS — re-argued here
	// rather than assumed, as this comment's earlier revision asked.
	//
	// This used to assert that the same tier gets 404 on GET /workspaces/{id},
	// offered as a corroborating inconsistency. That read was WIDENED
	// deliberately (F015, ownsWorkspaceOrSecurityAdmin in helpers.go): a
	// security admin already listed every workspace and already rewrote any
	// workspace's approved/denied egress, so refusing it the row — and
	// especially /observed-egress, the traffic that is the INPUT to the egress
	// decision it makes — left the tier acting blind on its own stated purpose.
	//
	// The record tier does not depend on that leg and never did. It rests on
	// what the ROUTE does: POST .../record launches a credentialed,
	// host-mounting, open-egress sandbox and stamps the caller as its owner,
	// which is reach INTO a run and AT the host — the two axes securityOps is
	// defined never to reach (routes.go). Reading a row is not launching one.
	//
	// So the assertion inverts into a STRONGER one: the tier can now read the
	// workspace and is STILL refused the record route. That proves the record
	// gate is the router's tier check rather than a side effect of the caller
	// being unable to see the row — which the old assertion could not
	// distinguish.
	if w := doSSO(t, srv, http.MethodGet, path, sess, ""); w.Code == http.StatusNotFound {
		t.Errorf("security_admin GET workspace = 404; the read was widened for this tier (F015) — " +
			"if it has been narrowed again, that decision and this one need re-reconciling")
	}
	if w := doSSO(t, srv, http.MethodPost, path+"/record", sess, `{"name":"exfil"}`); w.Code != http.StatusForbidden {
		t.Errorf("security_admin POST record = %d AFTER being able to read the workspace, want 403 — the record "+
			"refusal must come from the tier gate, not from the caller's inability to see the row", w.Code)
	}
	// The EGRESS DECISION stays delegable: promote-egress writes a list and
	// launches nothing, so the security tier keeps it. Reaching the handler (any
	// non-403) is the assertion; what it then answers is that handler's own test.
	if w := doSSO(t, srv, http.MethodPost, path+"/record/build/promote-egress", sess, `{}`); w.Code == http.StatusForbidden {
		t.Errorf("security_admin promote-egress = 403 — the egress DECISION must stay on the security tier; "+
			"only the sandbox LAUNCH moved: %s", w.Body.String())
	}
	// And the super admin still records, over a member-owned workspace: this is
	// a re-tiering, not a removal. 503 (no runner in this harness) proves the
	// request passed authorization and reached the launch.
	admin := ssoSession(t, "sub-admin", "admin@corp.example", oidc.RoleAdmin)
	if w := doSSO(t, srv, http.MethodPost, path+"/record", admin, `{"name":"exfil"}`); w.Code == http.StatusForbidden {
		t.Errorf("admin POST record = 403 — the super admin must still be able to record any workspace: %s", w.Body.String())
	}
}

// TestSecurityAdminCanStopAForeignRun makes an in-tree PROSE CLAIM executable,
// which is the whole point of it existing.
//
// Two authoritative comments disagreed about the same tier. helpers.go's
// ownsRunOrAdmin says the security tier gets foreign-run KILL on purpose —
// "INSPECT-OR-STOP is the whole of that arm's warrant … kill", named as the
// tier's most time-critical act — while routes.go and the authz matrix
// justified the sandbox sweep's SUPER gate as protecting "the one axis the
// security tier never gets", meaning exactly that reach. An operator choosing
// who to trust with RoleSecurityAdmin reads the second pair and concludes the
// tier cannot terminate other people's runs. It can.
//
// So this pins the TRUE half, in the direction that keeps the two in step: if
// anyone later narrows kill to ownsRunOrSuperAdmin, this test goes red and the
// person doing it is told, at the point of the change, that the sweep's
// rationale depends on the answer. That is the repair for an invariant asserted
// in prose and enforced by nothing.
//
// The other half — that the sweep never touches a live run — is already pinned
// by TestSweepTerminalSandboxes_TearsDownOrphanedLiveSandbox, whose fixture
// carries a RUNNING run for exactly that purpose; it is not duplicated here.
func TestSecurityAdminCanStopAForeignRun(t *testing.T) {
	kill := func(t *testing.T, role string) (int, types.RunState) {
		t.Helper()
		srv, ast, _, _ := newAuthzMatrixServer(t)
		id := uuid.New()
		ast.mu.Lock()
		ast.runs[id] = types.AgentRun{ID: id, CreatedBy: "sub-someone-else", State: types.RunRunning, Agent: "claude-code"}
		ast.mu.Unlock()
		sess := ssoSession(t, "sub-killer", "killer@corp.example", role)
		w := doSSO(t, srv, http.MethodPost, "/api/v1/runs/"+id.String()+"/kill", sess, "")
		ast.mu.Lock()
		defer ast.mu.Unlock()
		return w.Code, ast.runs[id].State
	}

	// The claim under test: the SECURITY tier really does reach a run it does
	// not own, and the run really ends — status alone would not prove the
	// transition applied.
	if code, state := kill(t, oidc.RoleSecurityAdmin); code != http.StatusAccepted || state != types.RunKilled {
		t.Errorf("security_admin kill of a FOREIGN run = %d, run state %q; want 202 and KILLED. "+
			"If this is now refused, that is a deliberate narrowing — and routes.go's sandbox-sweep note plus "+
			"the authz matrix row must be revisited, because they no longer describe the tier", code, state)
	}
	// The super admin, for contrast: same answer, so the assertion above is
	// about the SECURITY tier rather than about kill being open to everyone.
	if code, state := kill(t, oidc.RoleAdmin); code != http.StatusAccepted || state != types.RunKilled {
		t.Errorf("admin kill of a foreign run = %d, state %q; want 202 and KILLED", code, state)
	}
	// And the member, which is what makes it a TIER statement: a plain member is
	// refused with the byte-identical 404 a missing run gives, and the run is
	// untouched.
	if code, state := kill(t, oidc.RoleMember); code != http.StatusNotFound || state != types.RunRunning {
		t.Errorf("member kill of a foreign run = %d, state %q; want 404 and the run still RUNNING", code, state)
	}
}
