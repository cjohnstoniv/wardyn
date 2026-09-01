// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// governance_limits_test.go owns the CREATE-PATH half of the governance spine:
// the autonomy derivation (effectiveToolApprovals), the two GovernanceLimits
// booleans, and the two live capability bugs the same lane closes — G3's
// seeded-image capImage bypass (PF-34) and G4's member-authored llm_cred
// (PF-35).
//
// The ceiling resolver itself is governance_ceiling_test.go's; the escape table
// is governance_nonescape_test.go's. What is here is everything decided about
// the request SHAPE, which is the half no policy clamp can reach.

// ─── fixtures ─────────────────────────────────────────────────────────────────

// holdProfile is an assigned profile whose tool_rules demand supervision, with
// NO limits set — the limits and the derivation are independent mechanisms and a
// fixture that carried both could not tell which one refused.
func holdProfile(name string) *types.GovernanceProfile {
	p := &types.GovernanceProfile{ID: uuid.New(), Name: name, Ceiling: govProfileSpec()}
	p.Ceiling.ToolRules = []types.ToolRule{
		{Tool: "Read", Effect: types.ToolAllow},
		{Tool: "Bash", Effect: types.ToolHold},
	}
	return p
}

// limitsProfile is an assigned profile that carries limits and NO tool rules —
// the mirror fixture.
func limitsProfile(name string, limits types.GovernanceLimits) *types.GovernanceProfile {
	return &types.GovernanceProfile{
		ID: uuid.New(), Name: name, Ceiling: govProfileSpec(), Limits: limits,
	}
}

func assignedStore(p *types.GovernanceProfile) *capStore {
	return &capStore{govProfile: p, govTier: types.CapabilitySubjectGroup, govHasGroupTier: true}
}

// refusalBody decodes writeError's {"error": …} envelope so the frozen §7.7
// strings can be compared for EQUALITY. Substring-matching the raw body would
// silently pass on the JSON-escaped form of a quoted profile name, which is
// exactly the character these strings put around it.
func refusalBody(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var got struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode error body: %v (%s)", err, w.Body.String())
	}
	return got.Error
}

// ─── the derivation ───────────────────────────────────────────────────────────

// TestEffectiveToolApprovals is the autonomy derivation and its three scopings.
//
// The last case is THE counterfactual: a deployment whose Config.DefaultPolicy
// already carries hold/deny tool_rules — consulted today only when a request
// explicitly asks for `hold` — and NO assignment at all. If derivation keyed on
// the RULES rather than on the assignment, that deployment would flip every run
// into the hold lane and fire the codex refusal deployment-wide, on an upgrade
// that changed no configuration. Absent row, absent behaviour change.
func TestEffectiveToolApprovals(t *testing.T) {
	assigned := governanceCeiling{
		Spec:    holdProfile("walled").Ceiling,
		Profile: holdProfile("walled"),
	}
	// The same rules, reached through Config.DefaultPolicy with no profile bound.
	unassigned := governanceCeiling{Spec: holdProfile("walled").Ceiling}
	allowOnly := governanceCeiling{
		Spec:    types.RunPolicySpec{ToolRules: []types.ToolRule{{Tool: "*", Effect: types.ToolAllow}}},
		Profile: holdProfile("yolo"),
	}

	for _, tc := range []struct {
		name    string
		req     createRunRequest
		ceiling governanceCeiling
		want    string
	}{
		{
			name: "non-interactive under a hold-deriving profile: derived",
			req:  createRunRequest{Agent: "claude-code", Task: "t"}, ceiling: assigned, want: "hold",
		},
		{
			// The escape the override closes: one field would otherwise make a
			// supervision-demanding profile advisory.
			name: "an explicit auto does NOT beat the profile",
			req:  createRunRequest{Agent: "claude-code", Task: "t", ToolApprovals: "auto"}, ceiling: assigned, want: "hold",
		},
		{
			// PF-27: a human at the attach pane IS the supervision, and dispatch
			// writes WARDYN_TOOL_APPROVALS for non-interactive runs only — so
			// deriving here would brick the console flow and supervise nothing.
			name: "interactive: never derived",
			req:  createRunRequest{Agent: "claude-code", Task: "t", Interactive: true}, ceiling: assigned, want: "",
		},
		{
			// The SAME request shape a task-less create coerces into. Read
			// post-coercion, so it is interactive here too.
			name: "task-less (coerces to interactive): never derived",
			req:  createRunRequest{Agent: "claude-code"}, ceiling: assigned, want: "",
		},
		{
			// The north-star yolo profile stays fully autonomous.
			name: "an all-allow profile derives nothing",
			req:  createRunRequest{Agent: "claude-code", Task: "t"}, ceiling: allowOnly, want: "",
		},
		{
			name: "THE COUNTERFACTUAL: identical hold rules, NO assignment ⇒ no derivation",
			req:  createRunRequest{Agent: "claude-code", Task: "t"}, ceiling: unassigned, want: "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := effectiveToolApprovals(tc.req, tc.ceiling); got != tc.want {
				t.Errorf("effectiveToolApprovals = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestGovernanceToolApprovalsOnCreate walks the derivation and its two refusals
// through the real POST /runs door, because the pure function above cannot show
// that the derived value actually SURVIVES to where dispatch reads it.
//
// The oracle is the run.create audit event's tool_approvals key: runs.go records
// it whenever the field is non-empty, and it is the only durable record that an
// autonomous run's tool calls were routed to Wardyn approvals rather than
// running unsupervised.
func TestGovernanceToolApprovalsOnCreate(t *testing.T) {
	toolApprovalsOf := func(t *testing.T, audit *recRecorder) string {
		t.Helper()
		for _, ev := range audit.events {
			if ev.Action != "run.create" {
				continue
			}
			var d struct {
				ToolApprovals string `json:"tool_approvals"`
			}
			if err := json.Unmarshal(ev.Data, &d); err != nil {
				t.Fatalf("unmarshal run.create data: %v", err)
			}
			return d.ToolApprovals
		}
		t.Fatal("no run.create audit event")
		return ""
	}

	t.Run("a hold-deriving profile puts a non-interactive run in the hold lane", func(t *testing.T) {
		srv, st, audit := govEscapeFixture(t, assignedStore(holdProfile("walled")))
		govCreateAndDispatch(t, srv, st, audit, govSession(t, "sub-walled", []string{"eng"}, false),
			`{"agent":"claude-code","task":"t"}`)
		if got := toolApprovalsOf(t, audit); got != "hold" {
			t.Errorf("run.create tool_approvals = %q, want %q — the derivation never reached the run", got, "hold")
		}
	})

	t.Run("the same rules with NO assignment leave the run alone", func(t *testing.T) {
		// The counterfactual again, end to end: only the assignment differs.
		srv, st, audit := govEscapeFixture(t, &capStore{})
		srv.cfg.DefaultPolicy.ToolRules = holdProfile("x").Ceiling.ToolRules
		govCreateAndDispatch(t, srv, st, audit, govSession(t, "sub-plain", []string{"eng"}, false),
			`{"agent":"claude-code","task":"t"}`)
		if got := toolApprovalsOf(t, audit); got != "" {
			t.Errorf("run.create tool_approvals = %q for an UNASSIGNED member, want empty — that is not byte-for-byte today", got)
		}
	})

	t.Run("codex-cli is REFUSED, not silently downgraded (PF-18)", func(t *testing.T) {
		srv, _, _ := govEscapeFixture(t, assignedStore(holdProfile("walled")))
		w := doSSO(t, srv, http.MethodPost, "/api/v1/runs",
			govSession(t, "sub-walled", []string{"eng"}, false), `{"agent":"codex-cli","task":"t"}`)
		if w.Code != http.StatusForbidden {
			t.Fatalf("create = %d, want 403: %s", w.Code, w.Body.String())
		}
		// BYTE-EXACT frozen copy (docs/design/governance-prompt.md §7.7,
		// DENIED_CODEX_HOLD): the console never rewords a server refusal.
		const want = `codex-cli is not supported under your governance profile "walled": its tool rules hold or deny, ` +
			`and codex-cli has no external tool-approval contract. Launch a different agent.`
		if got := refusalBody(t, w); got != want {
			t.Errorf("body  = %s\nwant §7.7 BYTE-EXACT: %s", got, want)
		}
		if r := auditReasons(t, srv, "authz.denied"); !slices.Contains(r, "governance_profile") {
			t.Errorf("authz.denied reasons = %v, want a governance_profile row", r)
		}
		// And an INTERACTIVE codex run is untouched: no hold is derived there, so
		// there is no contradiction to refuse (PF-27's own rationale).
		w = doSSO(t, srv, http.MethodPost, "/api/v1/runs",
			govSession(t, "sub-walled", []string{"eng"}, false), `{"agent":"codex-cli","task":"t","interactive":true}`)
		if w.Code != http.StatusCreated {
			t.Errorf("interactive codex-cli = %d, want 201 — nothing was derived, so nothing contradicts: %s", w.Code, w.Body.String())
		}
	})

	t.Run("seed_auto_tools is REFUSED under a hold-deriving profile (PF-31)", func(t *testing.T) {
		srv, _, _ := govEscapeFixture(t, assignedStore(holdProfile("walled")))
		w := doSSO(t, srv, http.MethodPost, "/api/v1/runs",
			govSession(t, "sub-walled", []string{"eng"}, false),
			`{"agent":"claude-code","task":"t","interactive":true,"seed_auto_tools":true}`)
		if w.Code != http.StatusForbidden {
			t.Fatalf("create = %d, want 403: %s", w.Code, w.Body.String())
		}
		const want = "`seed_auto_tools` is not allowed by your governance profile \"walled\": its tool rules hold or deny, " +
			"and the pre-attach seed runs before any human is at the pane. Launch without it."
		if got := refusalBody(t, w); got != want {
			t.Errorf("body  = %s\nwant §7.7 BYTE-EXACT: %s", got, want)
		}
		// NOT scoped to the derivation's non-interactive lane, deliberately: the
		// pre-attach span runs skip-permissions with no toolgate and no human at
		// the pane, so PF-27's interactive-is-supervised rationale is false for it
		// — and this request IS interactive.
	})

	t.Run("an unassigned member keeps seed_auto_tools and codex-cli", func(t *testing.T) {
		srv, _, _ := govEscapeFixture(t, &capStore{})
		srv.cfg.DefaultPolicy.ToolRules = holdProfile("x").Ceiling.ToolRules
		for _, body := range []string{
			`{"agent":"codex-cli","task":"t"}`,
			`{"agent":"claude-code","task":"t","interactive":true,"seed_auto_tools":true}`,
		} {
			w := doSSO(t, srv, http.MethodPost, "/api/v1/runs",
				govSession(t, "sub-plain", []string{"eng"}, false), body)
			if w.Code != http.StatusCreated {
				t.Errorf("%s = %d, want 201 for an UNASSIGNED member: %s", body, w.Code, w.Body.String())
			}
		}
	})
}

// ─── the limits ───────────────────────────────────────────────────────────────

// TestGovernanceLimits covers the two GovernanceLimits booleans, and the
// interactive one is why this test exists.
//
// req.Interactive is COERCED from the request shape — a task-less request
// becomes interactive at runs_create_validate.go's coercion, which runs AFTER
// denyMemberRequest — so a gate reading the raw field is evaded by simply
// omitting the task, which is exactly the request a deny_interactive profile
// most needs to refuse.
func TestGovernanceLimits(t *testing.T) {
	t.Run("deny_task_mode_exec", func(t *testing.T) {
		srv, _, _ := govEscapeFixture(t, assignedStore(limitsProfile("no-exec",
			types.GovernanceLimits{DenyTaskModeExec: true})))
		w := doSSO(t, srv, http.MethodPost, "/api/v1/runs",
			govSession(t, "sub-walled", []string{"eng"}, false),
			`{"agent":"claude-code","task":"echo hi","task_mode":"exec"}`)
		if w.Code != http.StatusForbidden {
			t.Fatalf("exec create = %d, want 403: %s", w.Code, w.Body.String())
		}
		const want = "`task_mode=exec` is not allowed by your governance profile \"no-exec\" — an exec run carries no agent " +
			"and no tool approvals, so nothing supervises it. Launch with an agent instead."
		if got := refusalBody(t, w); got != want {
			t.Errorf("body  = %s\nwant §7.7 BYTE-EXACT: %s", got, want)
		}
		// A harness run under the same profile is untouched — the limit names one
		// door, not the member.
		w = doSSO(t, srv, http.MethodPost, "/api/v1/runs",
			govSession(t, "sub-walled", []string{"eng"}, false), `{"agent":"claude-code","task":"t"}`)
		if w.Code != http.StatusCreated {
			t.Errorf("harness create = %d, want 201: %s", w.Code, w.Body.String())
		}
	})

	t.Run("deny_interactive refuses BOTH the explicit flag and the omitted task", func(t *testing.T) {
		srv, _, _ := govEscapeFixture(t, assignedStore(limitsProfile("no-interactive",
			types.GovernanceLimits{DenyInteractive: true})))
		const want = `interactive runs are not allowed by your governance profile "no-interactive", and a request with no task ` +
			"comes up interactive too. Launch with a task, and without `--interactive`."
		for _, tc := range []struct{ name, body string }{
			{"explicit --interactive", `{"agent":"claude-code","task":"t","interactive":true}`},
			// THE case. Counterfactual: read req.Interactive raw (it is still
			// false here — the coercion has not run) and this create 201s, which
			// is the deny_interactive profile evaded by leaving a field out.
			{"omitted task (coerces to interactive)", `{"agent":"claude-code"}`},
			{"blank task", `{"agent":"claude-code","task":"   "}`},
		} {
			t.Run(tc.name, func(t *testing.T) {
				w := doSSO(t, srv, http.MethodPost, "/api/v1/runs",
					govSession(t, "sub-walled", []string{"eng"}, false), tc.body)
				if w.Code != http.StatusForbidden {
					t.Fatalf("create = %d, want 403: %s", w.Code, w.Body.String())
				}
				if got := refusalBody(t, w); got != want {
					t.Errorf("body  = %s\nwant §7.7 BYTE-EXACT: %s", got, want)
				}
			})
		}
		// A task-carrying non-interactive run still launches.
		w := doSSO(t, srv, http.MethodPost, "/api/v1/runs",
			govSession(t, "sub-walled", []string{"eng"}, false), `{"agent":"claude-code","task":"t"}`)
		if w.Code != http.StatusCreated {
			t.Errorf("non-interactive create = %d, want 201: %s", w.Code, w.Body.String())
		}
	})

	t.Run("an UNASSIGNED member is untouched by either limit", func(t *testing.T) {
		// The absent-row doctrine: limits live on a profile, so with no
		// assignment there is nothing to read and nothing changes.
		srv, _, _ := govEscapeFixture(t, &capStore{})
		for _, body := range []string{
			`{"agent":"claude-code","task":"echo hi","task_mode":"exec"}`,
			`{"agent":"claude-code"}`,
		} {
			w := doSSO(t, srv, http.MethodPost, "/api/v1/runs",
				govSession(t, "sub-plain", []string{"eng"}, false), body)
			if w.Code != http.StatusCreated {
				t.Errorf("%s = %d, want 201 for an UNASSIGNED member: %s", body, w.Code, w.Body.String())
			}
		}
	})

	t.Run("an OPERATOR short-circuits before the limits are ever read", func(t *testing.T) {
		// denyMemberRequest's first line. An admin under an `all` assignment must
		// not be bound by a row a security admin can write — and the exemption has
		// to be the FIRST thing, not a check after the resolve.
		srv, _, _ := govEscapeFixture(t, assignedStore(limitsProfile("everyone",
			types.GovernanceLimits{DenyTaskModeExec: true, DenyInteractive: true})))
		w := doSSO(t, srv, http.MethodPost, "/api/v1/runs",
			ssoSession(t, "sub-admin", "admin@corp.example", oidc.RoleAdmin), `{"agent":"claude-code"}`)
		if w.Code != http.StatusCreated {
			t.Errorf("admin create = %d, want 201: %s", w.Code, w.Body.String())
		}
	})
}

// ─── G3: the seeded-image capImage bypass (PF-34) ─────────────────────────────

// seedImageStore is govEscapeStore plus the one read the workspace_id door
// needs: GetWorkspace.
type seedImageStore struct {
	*govEscapeStore
	ws types.Workspace
}

func (s *seedImageStore) GetWorkspace(_ context.Context, id uuid.UUID) (types.Workspace, error) {
	if id != s.ws.ID {
		return types.Workspace{}, store.ErrNotFound
	}
	return s.ws, nil
}

func seedImageFixture(t *testing.T, cs *capStore, ws types.Workspace) (*Server, *recRecorder) {
	t.Helper()
	h := newHarness(t)
	st := &seedImageStore{govEscapeStore: newGovEscapeStore(cs), ws: ws}
	st.workspaces = []types.Workspace{ws}
	audit := &recRecorder{}
	cfg := baseTestConfig(h, st)
	cfg.Audit = audit
	cfg.Broker = h.broker
	cfg.Runner = &fakeRunner{}
	// A builder must be wired or validateImageBuildRequest 400s every seeded
	// image before the capability question is even asked.
	cfg.ImageBuilder = fakeImageBuilder{}
	cfg.Secrets = &memSecrets{m: map[string][]byte{}}
	cfg.OIDC = &oidc.Authenticator{}
	cfg.DefaultPolicy = govDeployment()
	return New(cfg), audit
}

// baseImageWorkspace is an ephemeral-only workspace carrying a chosen base
// image — the exact shape seedRequestWorkspace copies into req.Image.
func baseImageWorkspace(owner, image string) types.Workspace {
	return types.Workspace{
		ID:        uuid.New(),
		Name:      "ws",
		OwnedBy:   owner,
		Sources:   []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeEphemeral, Target: "/home/agent/work"}},
		BaseImage: &types.WorkspaceBaseImage{Kind: "custom", Image: image},
		Status:    types.WorkspaceScanned,
	}
}

// TestSeededImageCapabilityBypass is G3 (PF-34), live since 0.6.0: a member
// creates a workspace whose base_image is any ref they like, launches against
// it, and seedRequestWorkspace copies that ref into req.Image AFTER
// denyMemberRequest has already run — reaching the product's one WIDENING
// capability with no grant for it. The follow-up re-validation only re-checks
// the XOR and the builder, never capGranted.
//
// Three legs, and the THIRD is the one without which the next refactor
// reintroduces the catastrophic variant: capGranted REFUSES on !enforced, so an
// unconditional post-seed re-check would 403 every member run against every
// base-image workspace on every deployment that has not enforced capImage.
func TestSeededImageCapabilityBypass(t *testing.T) {
	const ref = "ghcr.io/attacker/anything:latest"
	const memberSub = "sub-walled"

	launch := func(t *testing.T, srv *Server, ws types.Workspace) *httptest.ResponseRecorder {
		t.Helper()
		return doSSO(t, srv, http.MethodPost, "/api/v1/runs",
			govSession(t, memberSub, []string{"eng"}, false),
			`{"agent":"claude-code","task":"t","workspace_id":"`+ws.ID.String()+`"}`)
	}

	t.Run("enforced + no grant: 403 byoi_member (the bug)", func(t *testing.T) {
		// RED on today's tree: before the fix this is a 201 and the member is
		// running an arbitrary image they hold no grant for.
		ws := baseImageWorkspace(memberSub, ref)
		srv, _ := seedImageFixture(t, &capStore{enf: map[string]bool{capImage: true}}, ws)
		w := launch(t, srv, ws)
		if w.Code != http.StatusForbidden {
			t.Fatalf("create = %d, want 403 — a member's own workspace base_image reached req.Image ungated: %s", w.Code, w.Body.String())
		}
		if r := auditReasons(t, srv, "authz.denied"); !slices.Contains(r, "byoi_member") {
			t.Errorf("authz.denied reasons = %v, want byoi_member — the SAME answer the explicit --image door gives", r)
		}
		if !strings.Contains(w.Body.String(), ref) {
			t.Errorf("body = %s, want the refused image ref named", w.Body.String())
		}
	})

	t.Run("enforced + an exact-ref grant: 201", func(t *testing.T) {
		ws := baseImageWorkspace(memberSub, ref)
		srv, _ := seedImageFixture(t, &capStore{
			enf:    map[string]bool{capImage: true},
			grants: []types.CapabilityGrant{grant(types.CapabilitySubjectUser, memberSub, capImage, ref, types.CapabilityAllow)},
		}, ws)
		if w := launch(t, srv, ws); w.Code != http.StatusCreated {
			t.Fatalf("create = %d, want 201 — a granted ref must still launch: %s", w.Code, w.Body.String())
		}
	})

	t.Run("NO REGRESSION: operator-owned workspace, capImage UNENFORCED: 201", func(t *testing.T) {
		// The pin. capGranted refuses on !enforced, so an UNCONDITIONAL re-check
		// here would refuse this launch — and this is the shape of every
		// base-image workspace on every deployment that has not adopted
		// capabilities, i.e. all of them on upgrade day.
		ws := baseImageWorkspace("", ref) // owned_by "" — operator-authored
		srv, _ := seedImageFixture(t, &capStore{}, ws)
		if w := launch(t, srv, ws); w.Code != http.StatusCreated {
			t.Fatalf("create = %d, want 201 — an operator-authored base image is not a member's free-text choice: %s", w.Code, w.Body.String())
		}
	})

	t.Run("preflight answers the SAME 403 launch does", func(t *testing.T) {
		// The sibling site the ticket forgets: without it Review previews a green
		// checklist for a launch that will 403.
		ws := baseImageWorkspace(memberSub, ref)
		srv, _ := seedImageFixture(t, &capStore{enf: map[string]bool{capImage: true}}, ws)
		w := doSSO(t, srv, http.MethodPost, "/api/v1/runs/preflight",
			govSession(t, memberSub, []string{"eng"}, false),
			`{"agent":"claude-code","task":"t","workspace_id":"`+ws.ID.String()+`"}`)
		if w.Code != http.StatusForbidden {
			t.Fatalf("preflight = %d, want the same 403 launch gives: %s", w.Code, w.Body.String())
		}
	})
}

// ─── the create path → dispatch hand-off ──────────────────────────────────────

// TestCreatePathWiresCeilingToDispatch is the create half of the dispatch-time
// deny re-assertion, and it exists because NEITHER lane's own tests can see it:
// the dispatch phase is driven directly by its tests (so it passes whether or
// not anything populates its two dispatchParams fields), and every create-path
// assertion above reads a policy the create-time clamp already narrowed. One
// unset struct field between them makes the whole re-assertion inert.
//
// The oracle is the run.ceiling.reassert audit event, which the phase records
// whenever a profile applies — including with nothing to drop, precisely so
// "which ceiling did this run actually run under" has an answer.
//
// The pairing is the pin: ASSIGNED ⇒ the event names the profile; UNASSIGNED,
// with the IDENTICAL deny list reached through Config.DefaultPolicy ⇒ no event
// at all. Counterfactual: drop the two fields from handleCreateRun's
// dispatchParams and the first half goes red while every dispatch test stays
// green.
func TestCreatePathWiresCeilingToDispatch(t *testing.T) {
	reassertProfile := func(t *testing.T, audit *recRecorder) (string, bool) {
		t.Helper()
		for _, ev := range audit.events {
			if ev.Action != "run.ceiling.reassert" {
				continue
			}
			var d struct {
				Profile string `json:"profile"`
			}
			if err := json.Unmarshal(ev.Data, &d); err != nil {
				t.Fatalf("unmarshal run.ceiling.reassert data: %v", err)
			}
			return d.Profile, true
		}
		return "", false
	}

	t.Run("an ASSIGNED member's run reaches dispatch carrying its ceiling", func(t *testing.T) {
		profile := govProfile("walled") // its ceiling denies corp.internal
		srv, st, audit := govEscapeFixture(t, assignedStore(profile))
		govCreateAndDispatch(t, srv, st, audit, govSession(t, "sub-walled", []string{"eng"}, false),
			`{"agent":"claude-code","task":"t"}`)
		got, ok := reassertProfile(t, audit)
		if !ok {
			t.Fatal("dispatch recorded no run.ceiling.reassert — the create path never populated CeilingDeny/CeilingProfile, so the phase is inert")
		}
		if got != "walled" {
			t.Errorf("run.ceiling.reassert profile = %q, want %q", got, "walled")
		}
	})

	t.Run("an UNASSIGNED member's run carries none (absent-row)", func(t *testing.T) {
		srv, st, audit := govEscapeFixture(t, &capStore{})
		// The SAME denies, reached through the deployment default: only the
		// assignment differs, so nothing but the scoping can explain the split.
		srv.cfg.DefaultPolicy.DeniedDomains = govProfileSpec().DeniedDomains
		govCreateAndDispatch(t, srv, st, audit, govSession(t, "sub-plain", []string{"eng"}, false),
			`{"agent":"claude-code","task":"t"}`)
		if got, ok := reassertProfile(t, audit); ok {
			t.Errorf("run.ceiling.reassert fired for an UNASSIGNED member (profile %q) — the phase must be a provable no-op there", got)
		}
	})
}

// ─── the create-time workspace-egress warning ─────────────────────────────────

// TestCeilingDeniedWorkspaceEgressWarning: an operator APPROVED a host for a
// workspace and the caller's profile DENIES it. Both decisions stand — the union
// happens, the deny wins at the proxy — so the run launches and that one host is
// refused mid-run. Without the warning that arrives as a support ticket.
//
// WARN, NEVER REFUSE (PF-14): the member authored neither the workspace nor the
// ceiling and has nothing to correct.
func TestCeilingDeniedWorkspaceEgressWarning(t *testing.T) {
	// govProfileSpec denies corp.internal; the workspace approves a host UNDER
	// it, so the match has to come from the proxy's own wildcard-capable matcher
	// rather than a string compare.
	profile := govProfile("walled")
	profile.Ceiling.DeniedDomains = []string{"*.corp.internal", "flat.example"}

	ws := types.Workspace{
		ID: uuid.New(), Name: "hello", Status: types.WorkspaceScanned,
		Sources:        []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeRepo, Source: govWorkspaceRepo}},
		ApprovedEgress: []string{"pypi.org", "wiki.corp.internal", "flat.example"},
	}

	warningsOf := func(t *testing.T, srv *Server, sess *http.Cookie) []string {
		t.Helper()
		w := doSSO(t, srv, http.MethodPost, "/api/v1/runs", sess,
			`{"agent":"claude-code","task":"t","inline_policy":{"min_confinement_class":"CC2","allowed_domains":["api.anthropic.com"],`+
				`"workspace_repos":[{"repo":"`+govWorkspaceRepo+`"}]}}`)
		if w.Code != http.StatusCreated {
			t.Fatalf("create = %d, want 201 — this is a WARNING, never a refusal: %s", w.Code, w.Body.String())
		}
		var got createRunResponse
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return got.Warnings
	}

	srv, st, _ := govEscapeFixture(t, assignedStore(profile))
	st.workspaces = []types.Workspace{ws}
	got := strings.Join(warningsOf(t, srv, govSession(t, "sub-walled", []string{"eng"}, false)), "\n")

	// BYTE-EXACT frozen copy (§7.7, WARN_WORKSPACE_DENIED), one line per host.
	for _, host := range []string{"wiki.corp.internal", "flat.example"} {
		want := `workspace host "` + host + `" is denied by your governance profile "walled" — the run launches, but that host is refused at the proxy`
		if !strings.Contains(got, want) {
			t.Errorf("warnings =\n%s\nwant the frozen §7.7 line: %s", got, want)
		}
	}
	if strings.Contains(got, "pypi.org") {
		t.Errorf("warnings =\n%s\n— an approved host the ceiling does NOT deny was warned about", got)
	}

	// An UNASSIGNED member gets no such line: the deployment ceiling's own denies
	// are what unionWorkspaceEgress has always run against, so warning about them
	// would fire on every run of every workspace and say nothing new.
	srv2, st2, _ := govEscapeFixture(t, &capStore{})
	srv2.cfg.DefaultPolicy.DeniedDomains = profile.Ceiling.DeniedDomains
	st2.workspaces = []types.Workspace{ws}
	if got := strings.Join(warningsOf(t, srv2, govSession(t, "sub-plain", []string{"eng"}, false)), "\n"); strings.Contains(got, "governance profile") {
		t.Errorf("warnings =\n%s\n— an UNASSIGNED member was warned about a governance profile they do not have", got)
	}
}

// ─── G4: member-authored llm_cred at workspace create (PF-35) ─────────────────

// TestMemberWorkspaceLLMCredRefused is G4 (PF-35). handleCreateWorkspace
// assigned LLMCred with no gate at all, and the binding folds through
// resolveRunIntegration's TIER 2 — which carries no resident_host guard
// precisely because a workspace pin is treated as OPERATOR consent. The
// dedicated PUT is operatorOnly; create was the one unguarded door.
//
// REFUSED, never silently dropped: a member who sees a 201 believes the
// workspace is bound to the integration they named.
func TestMemberWorkspaceLLMCredRefused(t *testing.T) {
	srv, st, _ := ownerHarness(t, runner.MemberMountPolicy{})
	const body = `{"name":"mine","llm_cred":{"integration_ref":"corp-openai"}}`

	w := doSSO(t, srv, http.MethodPost, "/api/v1/workspaces",
		ssoSession(t, ownerMemberSub, "member@corp.example", oidc.RoleMember), body)
	if w.Code != http.StatusForbidden {
		t.Fatalf("member create with llm_cred = %d, want 403: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "llm_cred is operator-only") {
		t.Errorf("body = %s, want a refusal naming the field", w.Body.String())
	}
	if r := auditReasons(t, srv, "authz.denied"); !slices.Contains(r, "admin_surface") {
		t.Errorf("authz.denied reasons = %v, want admin_surface (an existing vocabulary entry)", r)
	}
	// Nothing was written: a refusal that half-creates the workspace is worse
	// than the silent drop it replaces.
	if all, _ := st.ListWorkspaces(context.Background()); len(all) != 0 {
		t.Errorf("workspaces = %+v, want none — the refusal must precede the write", all)
	}

	// The SAME body from an OPERATOR is the ordinary onboarding call.
	w = doSSO(t, srv, http.MethodPost, "/api/v1/workspaces",
		ssoSession(t, "sub-owner-admin", "admin@corp.example", oidc.RoleAdmin), body)
	if w.Code != http.StatusCreated {
		t.Fatalf("operator create with llm_cred = %d, want 201: %s", w.Code, w.Body.String())
	}
	var created types.Workspace
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if created.LLMCred == nil || created.LLMCred.IntegrationRef != "corp-openai" {
		t.Errorf("llm_cred = %+v, want the operator's binding persisted", created.LLMCred)
	}

	// A member creating a workspace WITHOUT the field is unaffected — the gate
	// names one field, not the member.
	w = doSSO(t, srv, http.MethodPost, "/api/v1/workspaces",
		ssoSession(t, ownerMemberSub, "member@corp.example", oidc.RoleMember), `{"name":"plain"}`)
	if w.Code != http.StatusCreated {
		t.Errorf("member create without llm_cred = %d, want 201: %s", w.Code, w.Body.String())
	}
}
