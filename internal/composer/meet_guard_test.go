// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package composer

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// This guard answers one question: does the overlay meet have a rule for EVERY
// field a profile can carry?
//
// RunPolicySpec and GovernanceLimits are open to growth, and a field added to
// either without a decision here is the one that composes wrongly: dropped by a
// meet that never reads it, or inherited from the base where the overlay meant
// to narrow it. The table below is the decision, in code: each leaf field has a
// row saying what UNSET means to the runtime and how two values meet. A field
// with no row reds this test, and so does an overlay type that does not mirror
// the spec, and so does a row whose field the meet source never reads.
//
// It walks the types recursively (RunPolicySpec and GovernanceLimits down
// through PushRulesSpec, ResourceLimits, AutonomyRubric and the embedded
// RunLimits) so a field added to a NESTED struct is caught as well as one added
// at the top. Modelled on internal/api's policy_field_coverage_guard_test.go.
type meetRule struct {
	unset string // what the runtime does with zero or absent
	meet  string // how two values combine
}

var meetRules = map[string]meetRule{
	// RunPolicySpec
	"allowed_domains":                 {"nothing allowed (default deny)", "pairwise intersection of the two lists under internal/egress/domainmatch; an overlay entry the base's own list does not cover is invalid at write"},
	"denied_domains":                  {"none", "union"},
	"allow_all_egress":                {"false", "AND"},
	"first_use_approval":              {"always_deny (Normalize)", "stricter by firstUseApprovalRank"},
	"first_use_hold_seconds":          {"30 s, cut to 600 s", "smaller after normalising"},
	"max_holds":                       {"16, cut to 256", "smaller after normalising"},
	"allowed_methods":                 {"all methods", "intersection; empty on one side yields the other; disjoint is unsatisfiable; empty overlay refused"},
	"min_confinement_class":           {"lowest rank", "higher by confinementRank"},
	"eligible_grants":                 {"none", "the overlay's pass through; the resolver re-intersects against the resolved base"},
	"auto_stop_after_sec":             {"<= 0 never reaped", "smaller positive"},
	"workspace_mounts":                {"none", "intersection by (source, target); read-only if either side is"},
	"workspace_repos":                 {"none", "intersection by identity"},
	"llm_inspection":                  {"none", "present on one side yields that side; both present and unequal is refused"},
	"ui_apps":                         {"none", "intersection by (name, port, path)"},
	"resources.cpu_millis":            {"the deployment size", "smaller after normalising"},
	"resources.memory_mib":            {"the deployment size", "smaller after normalising"},
	"resources.pids_limit":            {"the deployment size", "smaller after normalising"},
	"resources.disk_mib":              {"unbounded", "smaller positive"},
	"tool_rules":                      {"unnamed tool is hold, \"*\" is the default", "per tool named on either side plus \"*\": stricter by toolStrictness, spelled out"},
	"git_push_any_branch":             {"false", "AND"},
	"push_rules.deny_paths":           {"none", "union"},
	"push_rules.max_inspect_pack_mib": {"32 MiB under an active rule", "smaller after normalising against the composed rule"},
	"push_rules.require_review_paths": {"none", "union"},
	"push_rules.hold_seconds":         {"120 s, cut to 600 s", "smaller after normalising"},
	"push_rules.deny_new_executables": {"false", "OR"},
	"push_rules.max_file_size_mib":    {"off", "smaller positive"},
	"azure_devops_capabilities":       {"the provider row's default profile", "intersection; a list under an empty base is a widening and stays empty; disjoint is unsatisfiable; empty overlay refused"},
	"github_capabilities":             {"the provider row's default profile", "intersection; a list under an empty base is a widening and stays empty; disjoint is unsatisfiable; empty overlay refused"},

	// GovernanceLimits
	"deny_task_mode_exec":              {"false", "OR"},
	"deny_interactive":                 {"false", "OR"},
	"deny_ui_apps":                     {"false", "OR"},
	"max_concurrent_runs":              {"0 unlimited", "smaller positive"},
	"deny_user_drive":                  {"false", "OR"},
	"max_cpu_millis":                   {"0 unlimited", "smaller positive"},
	"max_memory_mib":                   {"0 unlimited", "smaller positive"},
	"max_ephemeral_disk_mib":           {"0 unlimited", "smaller positive"},
	"max_drive_size_mib":               {"0 unlimited", "smaller positive"},
	"autonomy_rubric.egress_open":      {"caps nothing", "the lower level"},
	"autonomy_rubric.egress_reviewed":  {"caps nothing", "the lower level"},
	"autonomy_rubric.egress_sealed":    {"caps nothing", "the lower level"},
	"autonomy_rubric.secrets_powerful": {"caps nothing", "the lower level"},
	"autonomy_rubric.secrets_baseline": {"caps nothing", "the lower level"},
	"autonomy_rubric.secrets_none":     {"caps nothing", "the lower level"},
	"autonomy_rubric.confinement_cc1":  {"caps nothing", "the lower level"},
	"autonomy_rubric.confinement_cc2":  {"caps nothing", "the lower level"},
	"autonomy_rubric.confinement_cc3":  {"caps nothing", "the lower level"},
	"max_end_ahead_sec":                {"0 no limit", "smaller positive (TightenRunLimits)"},
	"default_end_sec":                  {"0 is max_end_ahead_sec", "smaller after normalising, clamped to the composed max"},
	"allow_no_end":                     {"false", "AND (TightenRunLimits)"},
	"max_wait_sec":                     {"0 the deployment's approval expiry", "smaller positive (TightenRunLimits)"},
	"default_wait_sec":                 {"0 is max_wait_sec", "smaller after normalising, clamped to the composed max"},
	"user_changes_limits":              {"false", "AND (TightenRunLimits)"},
	"pause_idle_after_sec":             {"0 pauses only runs waiting for a decision", "smaller positive (TightenRunLimits)"},
}

// atomicMeetTypes are the struct types a meet rule treats as one value (compared
// or intersected whole). Any OTHER struct is walked into, so a field added to it
// needs its own row.
var atomicMeetTypes = map[reflect.Type]bool{
	reflect.TypeOf(types.LLMInspectionSpec{}): true,
	reflect.TypeOf(types.GrantSpec{}):         true,
	reflect.TypeOf(types.WorkspaceMount{}):    true,
	reflect.TypeOf(types.WorkspaceRepo{}):     true,
	reflect.TypeOf(types.UIApp{}):             true,
	reflect.TypeOf(types.ToolRule{}):          true,
}

// leafField is one meetable field: its wire path and the Go name the meet reads.
type leafField struct{ path, goName string }

// walkLeaves lists every leaf field under t. An embedded struct with no json
// tag (RunLimits in GovernanceLimits) is flattened, as encoding/json does.
func walkLeaves(t reflect.Type, prefix string) []leafField {
	var out []leafField
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name == "-" {
			continue
		}
		ft := f.Type
		for ft.Kind() == reflect.Pointer || ft.Kind() == reflect.Slice || ft.Kind() == reflect.Array {
			ft = ft.Elem()
		}
		switch {
		case f.Anonymous && name == "":
			out = append(out, walkLeaves(ft, prefix)...)
		case ft.Kind() == reflect.Struct && !atomicMeetTypes[ft]:
			out = append(out, walkLeaves(ft, prefix+name+".")...)
		default:
			out = append(out, leafField{prefix + name, f.Name})
		}
	}
	return out
}

func paths(ls []leafField) []string {
	out := make([]string, 0, len(ls))
	for _, l := range ls {
		out = append(out, l.path)
	}
	slices.Sort(out)
	return out
}

// meetSelectors is every identifier the meet source reads off a value or names in
// a composite literal, from the AST, so a field mentioned only in a comment
// does not count as handled.
func meetSelectors(t *testing.T) map[string]bool { return sourceSelectors(t, "meet*.go") }

// sourceSelectors is meetSelectors over any set of source files.
func sourceSelectors(t *testing.T, glob string) map[string]bool {
	t.Helper()
	files, err := filepath.Glob(glob)
	if err != nil || len(files) == 0 {
		t.Fatalf("no %s source found: %v", glob, err)
	}
	seen := map[string]bool{}
	fset := token.NewFileSet()
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		file, perr := parser.ParseFile(fset, f, nil, 0) // no ParseComments: comments never reach the AST
		if perr != nil {
			t.Fatal(perr)
		}
		ast.Inspect(file, func(n ast.Node) bool {
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

// unhandledFields reports, for every leaf of spec, what is missing: a row in
// rules, a mirror in the overlay type, or a read of the overlay field by the
// meet source.
func unhandledFields(spec, overlay reflect.Type, rules map[string]meetRule, selectors map[string]bool) []string {
	var missing []string
	specLeaves, overlayLeaves := walkLeaves(spec, ""), walkLeaves(overlay, "")
	overlayGo := map[string]string{}
	for _, l := range overlayLeaves {
		overlayGo[l.path] = l.goName
	}
	for _, l := range specLeaves {
		if _, ok := rules[l.path]; !ok {
			missing = append(missing, l.path+": no meet rule (add a row to meetRules and implement it)")
		}
		goName, ok := overlayGo[l.path]
		if !ok {
			missing = append(missing, l.path+": the overlay type does not mirror it")
			continue
		}
		if !selectors[goName] {
			missing = append(missing, l.path+": the meet source never reads "+goName)
		}
	}
	return missing
}

func TestMeetEveryFieldHasARule(t *testing.T) {
	sel := meetSelectors(t)
	for _, c := range []struct {
		spec, overlay reflect.Type
	}{
		{reflect.TypeOf(types.RunPolicySpec{}), reflect.TypeOf(types.CeilingOverlay{})},
		{reflect.TypeOf(types.GovernanceLimits{}), reflect.TypeOf(types.LimitsOverlay{})},
	} {
		for _, m := range unhandledFields(c.spec, c.overlay, meetRules, sel) {
			t.Errorf("%s: %s", c.spec.Name(), m)
		}
		// The overlay must not carry a field the spec lacks either.
		specPaths := paths(walkLeaves(c.spec, ""))
		for _, p := range paths(walkLeaves(c.overlay, "")) {
			if !slices.Contains(specPaths, p) {
				t.Errorf("%s has %q, which %s does not", c.overlay.Name(), p, c.spec.Name())
			}
		}
	}
	all := append(walkLeaves(reflect.TypeOf(types.RunPolicySpec{}), ""), walkLeaves(reflect.TypeOf(types.GovernanceLimits{}), "")...)
	for p, r := range meetRules {
		if r.unset == "" || r.meet == "" {
			t.Errorf("row %q needs both an unset reading and a meet rule", p)
		}
		if !slices.Contains(paths(all), p) {
			t.Errorf("row %q names no field: it was renamed or removed", p)
		}
	}
}

// TestMeetGuardFlagsAnUnhandledField is the guard's own check: a fixture with a
// field no rule covers must come back flagged, at the top level and nested.
func TestMeetGuardFlagsAnUnhandledField(t *testing.T) {
	type nested struct {
		Known int `json:"known"`
		Fresh int `json:"fresh"`
	}
	type fixture struct {
		Top    bool    `json:"top"`
		Newest string  `json:"newest"`
		Inner  *nested `json:"inner"`
	}
	type fixtureOverlay struct {
		Top    *bool   `json:"top"`
		Newest *string `json:"newest"`
		Inner  *struct {
			Known *int `json:"known"`
		} `json:"inner"`
	}
	rules := map[string]meetRule{"top": {"a", "b"}, "inner.known": {"a", "b"}}
	got := unhandledFields(reflect.TypeOf(fixture{}), reflect.TypeOf(fixtureOverlay{}), rules,
		map[string]bool{"Top": true, "Known": true, "Newest": true})
	want := []string{
		"newest: no meet rule (add a row to meetRules and implement it)",
		"inner.fresh: no meet rule (add a row to meetRules and implement it)",
		"inner.fresh: the overlay type does not mirror it",
	}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("guard reported\n %q\nwant\n %q", got, want)
	}
}
