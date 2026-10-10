// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// contractDoors are the three doors the request contract must answer the same
// way at: create, Review and the policy preview.
var contractDoors = []struct{ name, path string }{
	{"create", "/api/v1/runs"},
	{"preflight", "/api/v1/runs/preflight"},
	{"preview", policyPreviewPath},
}

func contractBody(extra string) string {
	return `{"agent":"claude-code","task":"t","title":"t"` + extra + `}`
}

// TestRunContractRefusals pins every refusal the New Run request contract owns,
// at all three doors, by status, reason and (where the console shares it) the
// byte-exact sentence.
func TestRunContractRefusals(t *testing.T) {
	const ws1, ws2 = "11111111-1111-1111-1111-111111111111", "22222222-2222-2222-2222-222222222222"
	sel := func(id, target string) string { return `{"workspace_id":"` + id + `","target":"` + target + `"}` }
	for _, tc := range []struct {
		name, extra, reason, msg string
		status                   int
	}{
		{"placement unknown", `,"placement":"cloud"`, reasonPlacementInvalid, "", 400},
		{"runner id without local", `,"runner_id":"11111111-1111-1111-1111-111111111111"`, reasonPlacementInvalid, "", 400},
		{"runner id not a uuid", `,"placement":"local","runner_id":"x"`, reasonPlacementInvalid, "", 400},
		{"local is unavailable", `,"placement":"local"`, string(placement.ReasonPlacementUnavailable), "", 422},
		{"remote is today's behaviour", `,"placement":"remote"`, "", "", 200},
		{"allowed image is not dropped", `,"allowed_image":"ghcr.io/acme/dev:1"`, reasonRequestFieldUnavailable, "", 422},
		{"allowed image with spaces", `,"allowed_image":"a b"`, reasonAllowedImageInvalid, "", 400},
		{"negative cpu", `,"resources":{"cpu_millis":-1}`, reasonResourcesInvalid, "", 400},
		{"absurd memory", `,"resources":{"memory_mib":999999999}`, reasonResourcesInvalid, "", 400},
		{"resources accepted", `,"resources":{"cpu_millis":4000,"memory_mib":8192}`, "", "", 200},

		{"target relative", `,"workspaces":[` + sel(ws1, "work") + `]`, reasonWorkspaceTargetInvalid, "", 400},
		{"target dotdot", `,"workspaces":[` + sel(ws1, "/home/agent/../etc") + `]`, reasonWorkspaceTargetInvalid, "", 400},
		{"target not cleaned", `,"workspaces":[` + sel(ws1, "/home/agent/a/") + `]`, reasonWorkspaceTargetInvalid, "", 400},
		{"target outside home", `,"workspaces":[` + sel(ws1, "/etc/x") + `]`, reasonWorkspaceTargetInvalid, "", 400},
		{"target is the home", `,"workspaces":[` + sel(ws1, "/home/agent") + `]`, reasonWorkspaceTargetInvalid, "", 400},
		{"target sibling prefix", `,"workspaces":[` + sel(ws1, "/home/agentx/work") + `]`, reasonWorkspaceTargetInvalid, "", 400},
		{"target ok", `,"workspaces":[` + sel(ws1, "/home/agent/work") + `]`, "", "", 200},
		{"targets equal", `,"workspaces":[` + sel(ws1, "/home/agent/work") + `,` + sel(ws2, "/home/agent/work") + `]`,
			reasonWorkspaceTargetOverlap, "/home/agent/work is also where " + ws1 + " mounts. Give one of them another path.", 422},
		{"target nested inside", `,"workspaces":[` + sel(ws1, "/home/agent/work") + `,` + sel(ws2, "/home/agent/work/api") + `]`,
			reasonWorkspaceTargetOverlap, "/home/agent/work/api is inside /home/agent/work, where " + ws1 + " mounts. Two mounts can't nest.", 422},
		{"target nested around", `,"workspaces":[` + sel(ws1, "/home/agent/work/api") + `,` + sel(ws2, "/home/agent/work") + `]`,
			reasonWorkspaceTargetOverlap, "/home/agent/work/api is inside /home/agent/work, where " + ws2 + " mounts. Two mounts can't nest.", 422},
		{"siblings are fine", `,"workspaces":[` + sel(ws1, "/home/agent/work") + `,` + sel(ws2, "/home/agent/worker") + `]`, "", "", 200},
		{"one workspace may nest its own", `,"workspaces":[` + sel(ws1, "/home/agent/work") + `,` + sel(ws1, "/home/agent/work/api") + `]`, "", "", 200},
		{"drive equal", `,"drive":{"enabled":true},"workspaces":[` + sel(ws1, "/home/agent/drive") + `]`,
			reasonWorkspaceTargetOverlap, "/home/agent/drive is also where your drive mounts. Give one of them another path.", 422},
		{"drive nested", `,"drive":{"enabled":true},"workspaces":[` + sel(ws1, "/home/agent/drive/x") + `]`,
			reasonWorkspaceTargetOverlap, "/home/agent/drive/x is inside /home/agent/drive, where your drive mounts. Two mounts can't nest.", 422},
		{"drive path without the drive is reserved", `,"workspaces":[` + sel(ws1, "/home/agent/drive") + `]`, reasonWorkspaceTargetInvalid, "", 400},

		{"empty overrides are no edit", `,"overrides":{}`, "", "", 200},
		{"wildcard host", `,"overrides":{"agent":{"add_hosts":["*.example.com"]}}`, reasonOverrideRefused,
			"An override cannot add the wildcard host *.example.com. Name each host.", 422},
		{"ip host", `,"overrides":{"agent":{"add_hosts":["10.0.0.1"]}}`, reasonOverrideInvalid, "", 400},
		{"tool rule that allows", `,"overrides":{"agent":{"tool_rules":[{"tool":"Bash","effect":"allow"}]}}`, reasonOverrideRefused, "", 422},
		{"tool rule effect unknown", `,"overrides":{"agent":{"tool_rules":[{"tool":"Bash","effect":"maybe"}]}}`, reasonOverrideInvalid, "", 400},
		{"pat widening", `,"overrides":{"git_pat":[{"host":"git.example.com","access":"write"}]}`, reasonOverrideRefused, "", 422},
		{"pat api true", `,"overrides":{"git_pat":[{"host":"git.example.com","api":true}]}`, reasonOverrideRefused, "", 422},
		{"pat edit nothing", `,"overrides":{"git_pat":[{"host":"git.example.com"}]}`, reasonOverrideInvalid, "", 400},
		{"ado empty set", `,"overrides":{"azure_devops":{"capabilities":[]}}`, reasonOverrideInvalid, "", 400},
		{"ado unknown capability", `,"overrides":{"azure_devops":{"capabilities":["root"]}}`, reasonOverrideInvalid, "", 400},
		{"push without org", `,"overrides":{"push_rules":[{"provider":"github","deny_paths":["infra/"]}]}`, reasonOverrideInvalid, "", 400},
		{"push unknown provider", `,"overrides":{"push_rules":[{"provider":"gitlab","org":"acme","deny_paths":["infra/"]}]}`, reasonOverrideInvalid, "", 400},
		{"push key twice", `,"overrides":{"push_rules":[{"provider":"github","org":"acme","deny_paths":["a/"]},{"provider":"github","org":"acme","deny_paths":["b/"]}]}`, reasonOverrideInvalid, "", 400},
		{"push bad pattern", `,"overrides":{"push_rules":[{"provider":"github","org":"acme","deny_paths":["a//b"]}]}`, reasonOverrideInvalid, "", 400},

		// Allowed by the table and not yet applied: refused, never dropped.
		{"narrowing is not dropped", `,"overrides":{"azure_devops":{"capabilities":["code_read"]}}`, reasonRequestFieldUnavailable, "", 422},
		{"host add is not dropped", `,"overrides":{"agent":{"add_hosts":["api.example.com"]}}`, reasonRequestFieldUnavailable, "", 422},
		{"host removal is not dropped", `,"overrides":{"agent":{"remove_hosts":["api.example.com"]}}`, reasonRequestFieldUnavailable, "", 422},
		{"secret add is not dropped", `,"overrides":{"agent":{"add_secrets":[{"secret_name":"my-key","host":"api.example.com"}]}}`, reasonRequestFieldUnavailable, "", 422},
		{"tool hold is not dropped", `,"overrides":{"agent":{"tool_rules":[{"tool":"Bash","effect":"hold"}]}}`, reasonRequestFieldUnavailable, "", 422},
		{"pat narrowing is not dropped", `,"overrides":{"git_pat":[{"host":"git.example.com","access":"read"}]}`, reasonRequestFieldUnavailable, "", 422},
		{"push rules are not dropped", `,"overrides":{"push_rules":[{"provider":"github","org":"acme","deny_paths":["infra/"]},{"provider":"github","org":"other","require_review_paths":["ci/"]}]}`, reasonRequestFieldUnavailable, "", 422},

		{"builtin is not dropped", `,"components":[{"builtin":"github","org":"acme","repos":["acme/api"]}]`, reasonRequestFieldUnavailable, "", 422},
		{"builtin github needs repos", `,"components":[{"builtin":"github","org":"acme"}]`, reasonComponentRefInvalid, "", 400},
		{"builtin alone", `,"components":[{"builtin":"azure_devops","org":"acme","name":"x"}]`, reasonComponentRefInvalid, "", 400},
		{"builtin unknown", `,"components":[{"builtin":"gitlab","org":"acme"}]`, reasonComponentRefInvalid, "", 400},
		{"org without builtin", `,"components":[{"org":"acme"}]`, reasonComponentRefInvalid, "", 400},
	} {
		for _, door := range contractDoors {
			// The harness has no store to create a run in: the dry doors prove an
			// accepted field passes, and create proves every refusal.
			if tc.status == 200 && door.name == "create" {
				continue
			}
			t.Run(tc.name+"/"+door.name, func(t *testing.T) {
				h := newHarness(t)
				w := do(t, h.srv, http.MethodPost, door.path, adminToken, contractBody(tc.extra))
				if w.Code != tc.status || errorReason(w) != tc.reason {
					t.Fatalf("got %d %s, want %d reason %q", w.Code, w.Body.String(), tc.status, tc.reason)
				}
				if tc.msg != "" {
					var body struct{ Error string }
					if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || body.Error != tc.msg {
						t.Fatalf("sentence = %q, want %q", body.Error, tc.msg)
					}
				}
			})
		}
	}
}

// TestRunContractNarrowingTableDecidesTheServer pins that every table row is
// what the server answers: a refused row is override_refused, an allowed or
// clamped one passes the table (and meets the not-yet-applied stub).
func TestRunContractNarrowingTableDecidesTheServer(t *testing.T) {
	sample := map[types.OverrideKind]map[types.OverrideOp]client.RunOverrides{
		types.OverrideAgentHost: {
			types.OverrideAdd:    {Agent: &client.AgentOverrides{AddHosts: []string{"api.example.com"}}},
			types.OverrideRemove: {Agent: &client.AgentOverrides{RemoveHosts: []string{"api.example.com"}}},
		},
		types.OverrideAgentHostWildcard: {types.OverrideAdd: {Agent: &client.AgentOverrides{AddHosts: []string{"*.example.com"}}}},
		types.OverrideAgentSecret: {
			types.OverrideAdd:    {Agent: &client.AgentOverrides{AddSecrets: []client.SecretOverride{{SecretName: "k", Host: "api.example.com"}}}},
			types.OverrideRemove: {Agent: &client.AgentOverrides{RemoveSecrets: []string{"k"}}},
		},
		types.OverrideToolRuleRestrict: {types.OverrideAdd: {Agent: &client.AgentOverrides{ToolRules: []types.ToolRule{{Tool: "Bash", Effect: types.ToolDeny}}}}},
		types.OverrideToolRuleAllow:    {types.OverrideAdd: {Agent: &client.AgentOverrides{ToolRules: []types.ToolRule{{Tool: "Bash", Effect: types.ToolAllow}}}}},
		types.OverrideADOCapability: {
			types.OverrideNarrow: {AzureDevOps: &client.ADOOverrides{Capabilities: []string{"code_read"}}},
		},
		types.OverrideGitPATScope: {
			types.OverrideNarrow: {GitPAT: []client.GitPATOverride{{Host: "git.example.com", Access: "read"}}},
			types.OverrideAdd:    {GitPAT: []client.GitPATOverride{{Host: "git.example.com", Access: "write"}}},
		},
		types.OverridePushDeny:   {types.OverrideAdd: {PushRules: []client.PushRuleOverride{{Provider: "github", Org: "acme", DenyPaths: []string{"infra/"}}}}},
		types.OverridePushReview: {types.OverrideAdd: {PushRules: []client.PushRuleOverride{{Provider: "github", Org: "acme", RequireReviewPaths: []string{"ci/"}}}}},
	}
	for _, row := range types.OverrideNarrowingTable() {
		o, ok := sample[row.Kind][row.Op]
		if !ok {
			continue // an operation with no wire spelling: a removal of a push rule cannot be asked for
		}
		want := reasonRequestFieldUnavailable
		if row.Rule == types.OverrideRefused {
			want = reasonOverrideRefused
		}
		refusal := (&Server{}).runContractRefusal(createRunRequest{Overrides: &o})
		if refusal == nil || refusal.body.Reason != want {
			t.Errorf("%s %s (%s): got %+v, want reason %s", row.Kind, row.Op, row.Rule, refusal, want)
		}
	}
	for kind, ops := range sample {
		for op, o := range ops {
			if items := o.Items(); len(items) != 1 || items[0].Kind != kind || items[0].Op != op {
				t.Errorf("sample for %s %s lists %+v", kind, op, items)
			}
		}
	}
}

// TestPushRuleOverridesAreKeyedByProviderAndOrg pins the key: provider and
// organisation, not the provider alone and not one run-wide block.
func TestPushRuleOverridesAreKeyedByProviderAndOrg(t *testing.T) {
	rule := func(provider, org string) client.PushRuleOverride {
		return client.PushRuleOverride{Provider: provider, Org: org, DenyPaths: []string{"infra/"}}
	}
	for _, tc := range []struct {
		name  string
		rules []client.PushRuleOverride
		want  string // reason of the answer
	}{
		{"one provider, two organisations", []client.PushRuleOverride{rule("github", "acme"), rule("github", "other")}, reasonRequestFieldUnavailable},
		{"one organisation, two providers", []client.PushRuleOverride{rule("github", "acme"), rule("azure_devops", "acme")}, reasonRequestFieldUnavailable},
		{"one key twice", []client.PushRuleOverride{rule("github", "acme"), rule("github", "acme")}, reasonOverrideInvalid},
		{"no organisation", []client.PushRuleOverride{rule("github", "")}, reasonOverrideInvalid},
	} {
		refusal := (&Server{}).runContractRefusal(createRunRequest{Overrides: &client.RunOverrides{PushRules: tc.rules}})
		if refusal == nil || refusal.body.Reason != tc.want {
			t.Errorf("%s: got %+v, want %s", tc.name, refusal, tc.want)
		}
	}
	if k := rule("github", "acme").PushRuleKey(); k != "github/acme" {
		t.Errorf("key = %q", k)
	}
}

// TestPreviewAndPreflightCarryTheNewFacts pins the typed fields the New Run
// panels read: always arrays, empty until their lanes fill them.
func TestPreviewAndPreflightCarryTheNewFacts(t *testing.T) {
	for _, door := range contractDoors[1:] {
		h := newHarness(t)
		w := do(t, h.srv, http.MethodPost, door.path, adminToken, contractBody(""))
		if w.Code != http.StatusOK {
			t.Fatalf("%s = %d %s", door.name, w.Code, w.Body.String())
		}
		var body map[string]json.RawMessage
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		for _, field := range []string{"resources", "local_placement", "allowed_images"} {
			if string(body[field]) != "[]" {
				t.Errorf("%s %s = %s, want []", door.name, field, body[field])
			}
		}
	}
}

// TestWorkspaceRefusalSentencesMatchGolden pins the six sentences to the table
// the console's twin is pinned to.
func TestWorkspaceRefusalSentencesMatchGolden(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "ui", "src", "app", "lib", "workspace-refusals.golden.json"))
	if err != nil {
		t.Fatal(err)
	}
	var golden []struct {
		Key  string   `json:"key"`
		Args []string `json:"args"`
		Text string   `json:"text"`
	}
	if err := json.Unmarshal(raw, &golden); err != nil {
		t.Fatal(err)
	}
	a := func(args []string, n int) string { return args[n] }
	got := map[string]func(args []string) string{
		"TARGET_SHAPE":     func(g []string) string { return workspaceTargetShapeMsg(a(g, 0)) },
		"OVERLAP_EQUAL":    func(g []string) string { return workspaceOverlapEqualMsg(a(g, 0), a(g, 1)) },
		"OVERLAP_NESTED":   func(g []string) string { return workspaceOverlapNestedMsg(a(g, 0), a(g, 1), a(g, 2)) },
		"PIN_CONFLICT":     func(g []string) string { return workspacePinConflictMsg(a(g, 0), a(g, 1), a(g, 2), a(g, 3)) },
		"IMAGE_CONFLICT":   func(g []string) string { return imageConflictMsg(a(g, 0), a(g, 1)) },
		"ADO_ORG_CONFLICT": func(g []string) string { return workspaceADOOrgConflictMsg(a(g, 0), a(g, 1), a(g, 2), a(g, 3)) },
	}
	if len(golden) != len(got) {
		t.Fatalf("golden has %d sentences, the server has %d", len(golden), len(got))
	}
	for _, g := range golden {
		fn, ok := got[g.Key]
		if !ok {
			t.Fatalf("golden names %s, which the server has no sentence for", g.Key)
		}
		if s := fn(g.Args); s != g.Text {
			t.Errorf("%s = %q, golden %q", g.Key, s, g.Text)
		}
	}
}

// TestRunContractReasonsAreTheTypeScriptOnes pins the wire reasons the console
// matches on against the TypeScript table.
func TestRunContractReasonsAreTheTypeScriptOnes(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "ui", "src", "app", "lib", "new-run-refusals.ts"))
	if err != nil {
		t.Fatal(err)
	}
	for _, reason := range []string{
		reasonPlacementInvalid, reasonWorkspaceTargetInvalid, reasonWorkspaceTargetOverlap, reasonWorkspacePinConflict,
		reasonImageConflict, reasonWorkspaceADOOrgConflict, reasonResourcesInvalid, reasonOverrideInvalid,
		reasonAllowedImageInvalid,
		reasonOverrideRefused, reasonRequestFieldUnavailable, string(placement.ReasonPlacementUnavailable),
	} {
		if !strings.Contains(string(raw), `"`+reason+`"`) {
			t.Errorf("new-run-refusals.ts does not carry the reason %q", reason)
		}
	}
}

// TestAllowedImageIsNeverBesideABuiltImage pins the exclusivity at the contract
// step itself (the doors refuse image earlier when no builder is wired).
func TestAllowedImageIsNeverBesideABuiltImage(t *testing.T) {
	for _, req := range []createRunRequest{
		{AllowedImage: "ghcr.io/acme/dev:1", Image: "ghcr.io/acme/other:1"},
		{AllowedImage: "ghcr.io/acme/dev:1", DevcontainerRepo: "acme/dev"},
	} {
		refusal := (&Server{}).runContractRefusal(req)
		if refusal == nil || refusal.body.Reason != reasonAllowedImageInvalid {
			t.Errorf("got %+v, want %s", refusal, reasonAllowedImageInvalid)
		}
	}
}
