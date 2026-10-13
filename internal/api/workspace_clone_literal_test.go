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

// TestWorkspaceCloneHost_WorkspaceScanRunIsBounded launches a scan run of a member-owned
// workspace's repository source for real: its private-range clone host reaches the dispatched
// allowlist only where the member's ceiling lists it bare. At the library door (operator-only, no
// workspace) the host is kept.
func TestWorkspaceCloneHost_WorkspaceScanRunIsBounded(t *testing.T) {
	src := types.Source{ID: uuid.New(), Kind: types.SourceRepo, Locator: "https://10.0.0.5:8443/team/app.git"}
	ws := cloneLiteralWorkspace(govMemberSub)
	for _, tc := range []struct {
		name    string
		ceiling []string
		ws      *types.Workspace
		want    bool
	}{
		{"a member's workspace", nil, &ws, false},
		{"a ceiling entry for one port", []string{"10.0.0.5:8443"}, &ws, false},
		{"the ceiling lists the address", []string{"10.0.0.5"}, &ws, true},
		{"the library door", nil, nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, st, _ := govEscapeFixture(t, &capStore{})
			fr := &fakeRunner{}
			srv.cfg.Runner = fr
			srv.cfg.Store = &scanLaneStore{govEscapeStore: st}
			srv.cfg.DefaultPolicy.AllowAllEgress = true
			srv.cfg.DefaultPolicy.AllowedDomains = append(slices.Clone(srv.cfg.DefaultPolicy.AllowedDomains), tc.ceiling...)
			if _, err := srv.launchSourceScanRun(govMemberCtx([]string{"eng"}, false), "bob@corp.example", src, tc.ws); err != nil {
				t.Fatalf("launchSourceScanRun: %v", err)
			}
			if got := fr.lastSpec.ProxyConfig.Policy.AllowedDomains; slices.Contains(got, "10.0.0.5") != tc.want {
				t.Errorf("dispatched allowed_domains = %q, want 10.0.0.5 = %v", got, tc.want)
			}
		})
	}
}
