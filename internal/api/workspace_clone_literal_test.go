// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// cloneLiteralWorkspace is a stored workspace whose repositories are a private-range address on a
// port, an ordinary name and a public literal.
func cloneLiteralWorkspace(owner string, approved ...string) types.Workspace {
	return types.Workspace{
		ID: uuid.New(), Name: "app", OwnedBy: owner, Status: types.WorkspaceScanned, ApprovedEgress: approved,
		Sources: []types.WorkspaceSource{
			{Type: types.WorkspaceSourceTypeRepo, Source: "https://10.0.0.5:8443/team/app.git", Target: "/work/app"},
			{Type: types.WorkspaceSourceTypeRepo, Source: "https://git.example/team/lib", Target: "/work/lib"},
			{Type: types.WorkspaceSourceTypeRepo, Source: "https://93.184.216.34/team/tool.git", Target: "/work/tool"},
		},
	}
}

func TestRunCloneEgress(t *testing.T) {
	for _, tc := range []struct {
		name     string
		owner    string
		approved []string
		trusted  []string
		want     []string
	}{
		{"a member's workspace, no authority", govMemberSub, nil, nil, []string{"git.example", "93.184.216.34"}},
		{"the ceiling lists the address", govMemberSub, nil, []string{"10.0.0.5"}, []string{"10.0.0.5", "git.example", "93.184.216.34"}},
		{"the ceiling lists it for one port only", govMemberSub, nil, []string{"10.0.0.5:8443"}, []string{"git.example", "93.184.216.34"}},
		{"the ceiling lists a wildcard and a name", govMemberSub, nil, []string{"*.0.0.5", "git.example"}, []string{"git.example", "93.184.216.34"}},
		{"the workspace's approved egress lists it", govMemberSub, []string{"10.0.0.5"}, nil, []string{"10.0.0.5", "git.example", "93.184.216.34"}},
		{"approved egress for one port only", govMemberSub, []string{"10.0.0.5:8443"}, nil, []string{"git.example", "93.184.216.34"}},
		{"an operator-owned workspace", "", nil, nil, []string{"10.0.0.5", "git.example", "93.184.216.34"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			kept, dropped := runCloneEgress(cloneLiteralWorkspace(tc.owner, tc.approved...), tc.trusted)
			if !slices.Equal(kept, tc.want) {
				t.Errorf("kept = %q, want %q", kept, tc.want)
			}
			if wantDrop := !slices.Contains(tc.want, "10.0.0.5"); wantDrop != slices.Equal(dropped, []string{"10.0.0.5"}) {
				t.Errorf("dropped = %q", dropped)
			}
		})
	}
	// The run's own spec is not authority: with nothing trusted, a spec that already names the address
	// (as typed, or in another spelling of it) gains nothing from the clone lane.
	for _, held := range []string{"10.0.0.5", "::ffff:10.0.0.5"} {
		spec := types.RunPolicySpec{AllowedDomains: []string{held}}
		derived := deriveRunEgress(&spec, []types.Workspace{cloneLiteralWorkspace(govMemberSub)}, "", types.SiteConfig{}, nil, nil)
		for _, d := range derived {
			if d.kind == derivedWorkspaceClone && slices.Contains(d.added, "10.0.0.5") {
				t.Errorf("spec %q: the clone lane added %q", held, d.added)
			}
		}
		if n := len(spec.AllowedDomains); n != 3 || spec.AllowedDomains[0] != held {
			t.Errorf("spec %q: allowed_domains = %q, want it and the two public clone hosts", held, spec.AllowedDomains)
		}
	}
}

// TestWorkspaceCloneHost_PrivateLiteralAtTheDoors runs a member's stored workspace through the
// preview, preflight and create doors under a restricted and an allow-all ceiling. Its private-range
// clone host is added only where the ceiling lists that address bare, or an operator approved it for
// the workspace; an authority for one port does not cover the bare host, and an operator-owned
// workspace's host is added as before. The name and the public literal are added every time.
func TestWorkspaceCloneHost_PrivateLiteralAtTheDoors(t *testing.T) {
	restricted := govDeployment()
	allowAll := govDeployment()
	allowAll.AllowAllEgress = true
	with := func(base types.RunPolicySpec, extra ...string) types.RunPolicySpec {
		base.AllowedDomains = append(slices.Clone(base.AllowedDomains), extra...)
		return base
	}
	for _, tc := range []struct {
		name     string
		ceiling  types.RunPolicySpec
		owner    string
		approved []string
		want     bool
	}{
		{"restricted ceiling", restricted, govMemberSub, nil, false},
		{"allow-all ceiling", allowAll, govMemberSub, nil, false},
		{"a ceiling entry for one port", with(allowAll, "10.0.0.5:8443"), govMemberSub, nil, false},
		{"the ceiling lists the address", with(restricted, "10.0.0.5"), govMemberSub, nil, true},
		{"approved egress lists the address", allowAll, govMemberSub, []string{"10.0.0.5"}, true},
		{"an operator-owned workspace", allowAll, "", nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, st, audit := govEscapeFixture(t, &capStore{})
			srv.cfg.DefaultPolicy = tc.ceiling
			ws := cloneLiteralWorkspace(tc.owner, tc.approved...)
			st.workspaces = []types.Workspace{ws}
			cookie := govSession(t, govMemberSub, nil, false)
			body := `{"agent":"claude-code","task":"t","inline_policy":{"min_confinement_class":"CC2"},"workspaces":[{"workspace_id":"` + ws.ID.String() + `"}]}`

			for _, door := range []string{previewDoor, preflightDoor} {
				w := doSSO(t, srv, http.MethodPost, door, cookie, body)
				rows := provRows(t, w)
				has := func(host string) bool {
					return slices.ContainsFunc(rows, func(r provenanceRow) bool { return r.Field == fieldAllowed && r.Value == host })
				}
				if has("10.0.0.5") != tc.want || !has("git.example") || !has("93.184.216.34") {
					t.Errorf("%s: allowed rows %+v; want 10.0.0.5 added = %v and the name and public literal added", door, rows, tc.want)
				}
				// Review names the drop; the preview, a read door, passes no warning that names a host.
				wantWarn := !tc.want && door == preflightDoor
				if warned := strings.Contains(w.Body.String(), `dropped clone host \"10.0.0.5\"`); warned != wantWarn {
					t.Errorf("%s: drop warning present = %v, want %v: %s", door, warned, wantWarn, w.Body)
				}
			}

			spec := govCreateAndDispatch(t, srv, st, audit, cookie, body)
			if got := slices.Contains(spec.AllowedDomains, "10.0.0.5"); got != tc.want ||
				!slices.Contains(spec.AllowedDomains, "git.example") || !slices.Contains(spec.AllowedDomains, "93.184.216.34") {
				t.Errorf("create: dispatched allowed_domains = %q, want 10.0.0.5 = %v and the name and public literal", spec.AllowedDomains, tc.want)
			}
		})
	}
}

// TestWorkspaceCloneHost_DispatchUnionAndPostureAreBounded pins the two unions that run without the
// fold's warnings: unionRunEgress, which create and a queued run's start both dispatch through, and
// the autonomy posture's projection. Each drops the member's private clone host unless trusted.
func TestWorkspaceCloneHost_DispatchUnionAndPostureAreBounded(t *testing.T) {
	ws := cloneLiteralWorkspace(govMemberSub)
	for _, tc := range []struct {
		trusted []string
		want    bool
	}{{nil, false}, {[]string{"10.0.0.5:8443"}, false}, {[]string{"10.0.0.5"}, true}} {
		dispatched := types.RunPolicySpec{AllowedDomains: []string{"api.anthropic.com"}}
		newComponentFixture(t).srv.unionRunEgress(t.Context(), uuid.New(), &dispatched, grantWiring{}, []types.Workspace{ws}, "", types.SiteConfig{}, nil, runComponents{}, tc.trusted)
		if slices.Contains(dispatched.AllowedDomains, "10.0.0.5") != tc.want || !slices.Contains(dispatched.AllowedDomains, "git.example") {
			t.Errorf("trusted %q: dispatched = %q, want 10.0.0.5 = %v", tc.trusted, dispatched.AllowedDomains, tc.want)
		}
		posture := autonomyPostureSpec(types.RunPolicySpec{}, []types.Workspace{ws}, "", types.SiteConfig{}, adoEntraUngraded(), bedrockCredUngraded(), tc.trusted)
		if slices.Contains(posture.AllowedDomains, "10.0.0.5") != tc.want {
			t.Errorf("trusted %q: posture = %q, want 10.0.0.5 = %v", tc.trusted, posture.AllowedDomains, tc.want)
		}
	}
	// The run's own spec is not authority: one that names the address in another spelling does not
	// let either union add the bare clone host.
	own := types.RunPolicySpec{AllowedDomains: []string{"::ffff:10.0.0.5"}}
	dispatched := own.Clone()
	newComponentFixture(t).srv.unionRunEgress(t.Context(), uuid.New(), &dispatched, grantWiring{}, []types.Workspace{ws}, "", types.SiteConfig{}, nil, runComponents{}, nil)
	posture := autonomyPostureSpec(own, []types.Workspace{ws}, "", types.SiteConfig{}, adoEntraUngraded(), bedrockCredUngraded(), nil)
	if slices.Contains(dispatched.AllowedDomains, "10.0.0.5") || slices.Contains(posture.AllowedDomains, "10.0.0.5") {
		t.Errorf("a spec naming the address was taken as authority: dispatched %q, posture %q", dispatched.AllowedDomains, posture.AllowedDomains)
	}
}

// TestWorkspaceCloneHost_TheRunsOwnSpecIsNotAuthorityAtTheDoors: a member with no assigned profile
// launches under a stored policy that already lists the address. The policy's own entry stays, and
// the member workspace's clone host is still dropped: preflight says so, and neither dry door
// attributes the address to the workspace.
func TestWorkspaceCloneHost_TheRunsOwnSpecIsNotAuthorityAtTheDoors(t *testing.T) {
	srv, st, audit := govEscapeFixture(t, &capStore{})
	srv.cfg.DefaultPolicy.AllowAllEgress = true
	ws := cloneLiteralWorkspace(govMemberSub)
	st.workspaces = []types.Workspace{ws}
	policyID := uuid.New()
	st.policies[policyID] = types.RunPolicy{ID: policyID, Name: "row", Spec: types.RunPolicySpec{MinConfinementClass: types.CC2, AllowedDomains: []string{"10.0.0.5"}}}
	cookie := govSession(t, govMemberSub, nil, false)
	body := `{"agent":"claude-code","task":"t","policy_id":"` + policyID.String() + `","workspaces":[{"workspace_id":"` + ws.ID.String() + `"}]}`
	for _, door := range []string{previewDoor, preflightDoor} {
		w := doSSO(t, srv, http.MethodPost, door, cookie, body)
		if slices.ContainsFunc(provRows(t, w), func(r provenanceRow) bool {
			return r.Field == fieldAllowed && r.Value == "10.0.0.5" && r.Source.Kind == provKindWorkspace
		}) {
			t.Errorf("%s: the address is attributed to the workspace: %s", door, w.Body)
		}
		if door == preflightDoor && !strings.Contains(w.Body.String(), cloneHostDropWarningJSON("10.0.0.5", "app")) {
			t.Errorf("%s: no drop warning: %s", door, w.Body)
		}
	}
	if spec := govCreateAndDispatch(t, srv, st, audit, cookie, body); !slices.Contains(spec.AllowedDomains, "10.0.0.5") {
		t.Errorf("create: the stored policy's own entry went: %q", spec.AllowedDomains)
	}
	for _, ev := range audit.events {
		if ev.Action == "run.egress.add" && strings.Contains(string(ev.Data), `"workspace_clone"`) && strings.Contains(string(ev.Data), `"10.0.0.5"`) {
			t.Errorf("create: the clone lane is audited as adding the address: %s", ev.Data)
		}
	}
}

// cloneHostDropWarningJSON is the workspace drop warning's opening as it reads inside a JSON body.
func cloneHostDropWarningJSON(host, ws string) string {
	return `dropped clone host \"` + host + `\" of workspace \"` + ws + `\"`
}

// TestCloneHostDropWarning: the warning names a place the host can actually be allowed. A
// workspace's approved egress takes dotted hosts, so an IPv6 literal and a library source's host
// are pointed at the ceiling.
func TestCloneHostDropWarning(t *testing.T) {
	for _, tc := range []struct{ host, kind, want string }{
		{"10.0.0.5", "workspace", `dropped clone host "10.0.0.5" of workspace "app": a private address not in operator allowlist; allow it in the workspace's approved egress`},
		{"fd12:3456::1", "workspace", `dropped clone host "fd12:3456::1" of workspace "app": a private address not in operator allowlist; list it in the ceiling's allowed_domains`},
		{"10.0.0.5", "source", `dropped clone host "10.0.0.5" of source "app": a private address not in operator allowlist; list it in the deployment ceiling's allowed_domains`},
	} {
		if got := cloneHostDropWarning(tc.host, tc.kind, "app"); got != tc.want {
			t.Errorf("warning = %q, want %q", got, tc.want)
		}
	}
	// And at the door: a member's workspace on an IPv6 unique-local address.
	srv, st, _ := govEscapeFixture(t, &capStore{})
	ws := types.Workspace{ID: uuid.New(), Name: "app", OwnedBy: govMemberSub, Status: types.WorkspaceScanned,
		Sources: []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeRepo, Source: "https://[fd12:3456::1]/team/app.git", Target: "/work/app"}}}
	st.workspaces = []types.Workspace{ws}
	body := `{"agent":"claude-code","task":"t","inline_policy":{"min_confinement_class":"CC2"},"workspaces":[{"workspace_id":"` + ws.ID.String() + `"}]}`
	w := doSSO(t, srv, http.MethodPost, preflightDoor, govSession(t, govMemberSub, nil, false), body)
	if !strings.Contains(w.Body.String(), `list it in the ceiling's allowed_domains`) || strings.Contains(w.Body.String(), "approved egress") {
		t.Errorf("preflight: the IPv6 drop warning does not name the ceiling: %s", w.Body)
	}
}

// TestWorkspaceCloneHost_RecordSessionIsBounded: a record session's allowlist (confined replay and
// the allow-all learning session alike) carries a member-owned workspace's private-range clone
// host only where the launching principal's ceiling lists it bare; launchRecordRun passes that
// ceiling's allowlist. An operator-owned workspace's host is carried as before.
func TestWorkspaceCloneHost_RecordSessionIsBounded(t *testing.T) {
	srv := New(Config{})
	for _, confined := range []bool{true, false} {
		for _, tc := range []struct {
			owner   string
			trusted []string
			want    bool
		}{{govMemberSub, nil, false}, {govMemberSub, []string{"10.0.0.5:8443"}, false}, {govMemberSub, []string{"10.0.0.5"}, true}, {"", nil, true}} {
			p := srv.recordSessionPolicy(cloneLiteralWorkspace(tc.owner), types.CC2, confined, tc.trusted)
			if slices.Contains(p.AllowedDomains, "10.0.0.5") != tc.want || !slices.Contains(p.AllowedDomains, "git.example") {
				t.Errorf("confined=%v owner=%q trusted=%q: allowed_domains = %q, want 10.0.0.5 = %v", confined, tc.owner, tc.trusted, p.AllowedDomains, tc.want)
			}
		}
	}
}

// TestWorkspaceCloneHost_ScanRunIsBounded launches a scan run of a repository source for real. For
// a member-owned workspace its private-range clone host reaches the dispatched allowlist only where
// the member's ceiling lists it bare. At the library door (no workspace, no owner on the source) the
// ceiling resolved for the scan is the only authority. A name and a public literal are unchanged.
func TestWorkspaceCloneHost_ScanRunIsBounded(t *testing.T) {
	private := "https://10.0.0.5:8443/team/app.git"
	ws := cloneLiteralWorkspace(govMemberSub)
	for _, tc := range []struct {
		name    string
		locator string
		ceiling []string
		ws      *types.Workspace
		want    []string
	}{
		{"a member's workspace", private, nil, &ws, nil},
		{"a member's workspace, a ceiling entry for one port", private, []string{"10.0.0.5:8443"}, &ws, nil},
		{"a member's workspace, the ceiling lists the address", private, []string{"10.0.0.5"}, &ws, []string{"10.0.0.5"}},
		{"the library door", private, nil, nil, nil},
		{"the library door, a ceiling entry for one port", private, []string{"10.0.0.5:8443"}, nil, nil},
		{"the library door, the ceiling lists the address", private, []string{"10.0.0.5"}, nil, []string{"10.0.0.5"}},
		{"the library door, a name", "https://git.example/team/lib", nil, nil, []string{"git.example"}},
		{"the library door, a public literal", "https://93.184.216.34/team/tool.git", nil, nil, []string{"93.184.216.34"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, st, _ := govEscapeFixture(t, &capStore{})
			fr := &fakeRunner{}
			srv.cfg.Runner = fr
			srv.cfg.Store = &scanLaneStore{govEscapeStore: st}
			srv.cfg.DefaultPolicy.AllowAllEgress = true
			srv.cfg.DefaultPolicy.AllowedDomains = append(slices.Clone(srv.cfg.DefaultPolicy.AllowedDomains), tc.ceiling...)
			src := types.Source{ID: uuid.New(), Kind: types.SourceRepo, Name: "app", Locator: tc.locator}
			if _, err := srv.launchSourceScanRun(govMemberCtx([]string{"eng"}, false), "bob@corp.example", src, tc.ws); err != nil {
				t.Fatalf("launchSourceScanRun: %v", err)
			}
			got := slices.DeleteFunc(slices.Clone(fr.lastSpec.ProxyConfig.Policy.AllowedDomains), func(h string) bool {
				return h != "10.0.0.5" && h != "git.example" && h != "93.184.216.34"
			})
			if !slices.Equal(got, tc.want) {
				t.Errorf("dispatched clone hosts = %q, want %q (allowed_domains %q)", got, tc.want, fr.lastSpec.ProxyConfig.Policy.AllowedDomains)
			}
		})
	}
}
