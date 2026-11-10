// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package composer

import (
	"go/ast"
	"go/parser"
	"go/token"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The field coverage guard for Clamp, the member seam, as leq_guard_test.go is
// for Leq: every RunPolicySpec field either has a row in clampRules, saying how
// Clamp bounds it by the ceiling and read by clamp.go outside cloneProposal, or a
// row in withoutClampRules saying why Clamp leaves it as proposed. A field added
// to RunPolicySpec with neither reds this test, so a new policy field cannot
// pass the member seam unbounded without someone writing down why.
var clampRules = map[string]string{
	"allowed_domains":                  "intersected with the ceiling's allowlist unless the ceiling allows all egress",
	"denied_domains":                   "the ceiling's denials unioned in",
	"allow_all_egress":                 "kept only where the ceiling sets it (clampOperatorSwitches)",
	"refuse_ip_literals":               "set where the ceiling sets it (clampOperatorSwitches)",
	"first_use_approval":               "raised to the ceiling's rank",
	"first_use_hold_seconds":           "lowered to the ceiling's",
	"max_holds":                        "lowered to the ceiling's",
	"allowed_methods":                  "intersected with the ceiling's (clampMethods)",
	"min_confinement_class":            "raised to the ceiling's minimum",
	"eligible_grants":                  "each grant bounded by the ceiling's grant of its kind (clampGrants)",
	"auto_stop_after_sec":              "capped at the ceiling's positive maximum",
	"workspace_mounts":                 "always dropped: host mounts are operator-authored",
	"llm_inspection":                   "the ceiling's mode stands; a proposal's is dropped where the ceiling sets none",
	"ui_apps":                          "kept only where the ceiling lists the app (clampUIApps)",
	"resources.cpu_millis":             "bounded by the member cap (clampResources)",
	"resources.memory_mib":             "bounded by the member cap (clampResources)",
	"resources.pids_limit":             "capped at the ceiling's (clampResources)",
	"resources.disk_mib":               "capped at the ceiling's (clampResources)",
	"tool_rules":                       "per tool, at least as strict as the ceiling (clampToolRules)",
	"git_push_any_branch":              "kept only where the ceiling sets it (clampOperatorSwitches)",
	"harness_inner_walls.sandbox_mode": "the ceiling's choice stands (clampInnerWalls)",
	"harness_inner_walls.approvals":    "the ceiling's choice stands (clampInnerWalls)",
	"desktop.paste_in":                 "blocked where the ceiling blocks it (clampDesktop)",
	"desktop.copy_out":                 "blocked where the ceiling blocks it (clampDesktop)",
	"desktop.upload":                   "blocked where the ceiling blocks it (clampDesktop)",
	"desktop.download":                 "blocked where the ceiling blocks it (clampDesktop)",
	"recording":                        "raised to the ceiling's level (clampRecording)",
	"data_out":                         "raised to the ceiling's rule, and blocked denies every push (clampDataOut)",
	"push_rules.deny_paths":            "the ceiling's unioned in (clampPushRules)",
	"push_rules.max_inspect_pack_mib":  "capped at the ceiling's (clampPushRules)",
	"push_rules.require_review_paths":  "the ceiling's unioned in (clampPushRules)",
	"push_rules.hold_seconds":          "the shorter of the two (clampPushRules)",
	"push_rules.deny_new_executables":  "set where the ceiling sets it (clampPushRules)",
	"push_rules.max_file_size_mib":     "capped at the ceiling's (clampPushRules)",
	"push_rules.deny_all":              "set where the ceiling sets it (clampPushRules)",
	"github_capabilities":              "kept only where the ceiling lists it; none named, or none kept, takes the ceiling's list (clampGitHubCapabilities)",
}

// withoutClampRules are the fields Clamp passes through as proposed, each with
// where it is bounded instead.
var withoutClampRules = map[string]string{
	// Bounded at dispatch, where the run's Azure DevOps row is known: a member's
	// entry stands only where it meets the row's default_profile or their
	// governance list, and a list meeting nothing refuses the launch
	// (internal/api adoStandingBound). Narrowing it here could leave an explicit
	// choice empty, which dispatch would read as the row's default.
	"azure_devops_capabilities": "bounded at dispatch against the run's Azure DevOps row",
	// Bounded by the member path in internal/api (inline_policy_bounds.go), which
	// knows the repositories the deployment has onboarded; the ceiling carries no
	// list to bound them by.
	"workspace_repos": "bounded by internal/api against the onboarded repositories",
}

// clampWhole are the structs Clamp bounds as one value through their own meet
// (DesktopControls.Meet), keyed by wire name with the Go name clamp.go reads.
// Each of their leaves still needs a row, so a new one is looked at.
var clampWhole = map[string]string{"desktop": "Desktop"}

// clampSelectors is every identifier clamp.go reads off a value or names in a
// composite literal, outside cloneProposal: copying a field is not a rule.
func clampSelectors(t *testing.T) map[string]bool {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), "clamp.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, d := range file.Decls {
		if fn, ok := d.(*ast.FuncDecl); ok && fn.Name.Name == "cloneProposal" {
			continue
		}
		ast.Inspect(d, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.SelectorExpr:
				seen[x.Sel.Name] = true
			case *ast.KeyValueExpr:
				if id, ok := x.Key.(*ast.Ident); ok {
					seen[id.Name] = true
				}
			}
			return true
		})
	}
	return seen
}

// unclampedFields reports each leaf of spec with no row in either list, a row in
// both, or a rule whose Go field clamp.go never reads.
func unclampedFields(spec reflect.Type, rules, exempt, whole map[string]string, selectors map[string]bool) []string {
	var missing []string
	for _, l := range walkLeaves(spec, "") {
		_, ruled := rules[l.path]
		_, skipped := exempt[l.path]
		goName := l.goName
		if parent, _, nested := strings.Cut(l.path, "."); nested && whole[parent] != "" {
			goName = whole[parent]
		}
		switch {
		case ruled && skipped:
			missing = append(missing, l.path+": both a clamp rule and an exemption")
		case !ruled && !skipped:
			missing = append(missing, l.path+": no clamp rule (bound it in clamp.go and add a row to clampRules, or say why not in withoutClampRules)")
		case ruled && !selectors[goName]:
			missing = append(missing, l.path+": clamp.go never reads "+goName)
		}
	}
	return missing
}

func TestClampEveryFieldHasARule(t *testing.T) {
	spec := reflect.TypeOf(types.RunPolicySpec{})
	for _, m := range unclampedFields(spec, clampRules, withoutClampRules, clampWhole, clampSelectors(t)) {
		t.Errorf("RunPolicySpec: %s", m)
	}
	all := paths(walkLeaves(spec, ""))
	for _, rows := range []map[string]string{clampRules, withoutClampRules} {
		for p, r := range rows {
			if r == "" {
				t.Errorf("row %q needs a reason", p)
			}
			if !slices.Contains(all, p) {
				t.Errorf("row %q names no field: it was renamed or removed", p)
			}
		}
	}
}

// TestClampGuardFlagsAnUnhandledField is the guard's own check: a field with no
// row, a rule the source never reads, a field on both lists, and a leaf of a
// whole value whose parent the source never reads all come back.
func TestClampGuardFlagsAnUnhandledField(t *testing.T) {
	type nested struct {
		Known int `json:"known"`
		Fresh int `json:"fresh"`
	}
	type fixture struct {
		Top    bool    `json:"top"`
		Newest string  `json:"newest"`
		Twice  int     `json:"twice"`
		Passed int     `json:"passed"`
		Inner  *nested `json:"inner"`
		Whole  *nested `json:"whole"`
	}
	rules := map[string]string{"top": "x", "inner.known": "x", "newest": "x", "twice": "x", "whole.known": "x"}
	exempt := map[string]string{"twice": "x", "passed": "x"}
	got := unclampedFields(reflect.TypeOf(fixture{}), rules, exempt, map[string]string{"whole": "Whole"},
		map[string]bool{"Top": true, "Known": true, "Twice": true})
	want := []string{
		"newest: clamp.go never reads Newest",
		"twice: both a clamp rule and an exemption",
		"inner.fresh: no clamp rule (bound it in clamp.go and add a row to clampRules, or say why not in withoutClampRules)",
		"whole.known: clamp.go never reads Whole",
		"whole.fresh: no clamp rule (bound it in clamp.go and add a row to clampRules, or say why not in withoutClampRules)",
	}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("guard reported\n %q\nwant\n %q", got, want)
	}
}
