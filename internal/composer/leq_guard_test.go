// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package composer

import (
	"reflect"
	"slices"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The second half of the field coverage guard: meet_guard_test.go asks whether
// the overlay meet has a rule for every field a profile can carry, and this asks
// the same of Leq. A field added to RunPolicySpec or GovernanceLimits with no
// row here, or with a row but no read in leq.go, reds this test. Each row says
// what makes the field "at most as permissive" in a, relative to b.
var leqRules = map[string]string{
	// RunPolicySpec
	"allowed_domains":                 "every entry of a is covered by an entry of b under the proxy's matcher",
	"denied_domains":                  "every denial of b is covered by a denial of a",
	"allow_all_egress":                "a implies b",
	"first_use_approval":              "rank of a >= rank of b",
	"first_use_hold_seconds":          "a <= b after normalising",
	"max_holds":                       "a <= b after normalising",
	"allowed_methods":                 "b empty: any a; else a non-empty subset of b",
	"min_confinement_class":           "rank of a >= rank of b",
	"eligible_grants":                 "every grant of a is within b's grants (GrantWithin)",
	"auto_stop_after_sec":             "a is no looser bound than b",
	"workspace_mounts":                "every mount of a is a mount of b, read-only wherever b's is",
	"workspace_repos":                 "every repo of a is a repo of b",
	"llm_inspection":                  "b none: any a; a none: false; both: identical",
	"ui_apps":                         "every app of a is an app of b on the same path",
	"resources.cpu_millis":            "a <= b after normalising",
	"resources.memory_mib":            "a <= b after normalising",
	"resources.pids_limit":            "a <= b after normalising",
	"resources.disk_mib":              "a is no looser bound than b",
	"tool_rules":                      "per tool of either side and \"*\": a is at least as strict as b",
	"git_push_any_branch":             "a implies b",
	"push_rules.deny_paths":           "a keeps every path of b",
	"push_rules.max_inspect_pack_mib": "a is no looser cap than b, after the default under an active rule",
	"push_rules.require_review_paths": "a keeps every path of b",
	"push_rules.hold_seconds":         "a <= b after normalising",
	"push_rules.deny_new_executables": "b implies a",
	"push_rules.max_file_size_mib":    "a is no looser bound than b",
	"azure_devops_capabilities":       "b empty: only an empty a; else a non-empty subset of b",

	// GovernanceLimits
	"deny_task_mode_exec":              "b implies a",
	"deny_interactive":                 "b implies a",
	"deny_ui_apps":                     "b implies a",
	"max_concurrent_runs":              "a is no looser bound than b",
	"deny_user_drive":                  "b implies a",
	"max_cpu_millis":                   "a is no looser bound than b",
	"max_memory_mib":                   "a is no looser bound than b",
	"max_ephemeral_disk_mib":           "a is no looser bound than b",
	"max_drive_size_mib":               "a is no looser bound than b",
	"autonomy_rubric.egress_open":      "b caps nothing: any a; else a caps at a level <= b's",
	"autonomy_rubric.egress_reviewed":  "b caps nothing: any a; else a caps at a level <= b's",
	"autonomy_rubric.egress_sealed":    "b caps nothing: any a; else a caps at a level <= b's",
	"autonomy_rubric.secrets_powerful": "b caps nothing: any a; else a caps at a level <= b's",
	"autonomy_rubric.secrets_baseline": "b caps nothing: any a; else a caps at a level <= b's",
	"autonomy_rubric.secrets_none":     "b caps nothing: any a; else a caps at a level <= b's",
	"autonomy_rubric.confinement_cc1":  "b caps nothing: any a; else a caps at a level <= b's",
	"autonomy_rubric.confinement_cc2":  "b caps nothing: any a; else a caps at a level <= b's",
	"autonomy_rubric.confinement_cc3":  "b caps nothing: any a; else a caps at a level <= b's",
	"max_end_ahead_sec":                "a is no looser bound than b",
	"default_end_sec":                  "a is no looser bound than b, a 0 default read as the maximum",
	"allow_no_end":                     "a implies b",
	"max_wait_sec":                     "a is no looser bound than b",
	"default_wait_sec":                 "a is no looser bound than b, a 0 default read as the maximum",
	"user_changes_limits":              "a implies b",
	"pause_idle_after_sec":             "a is no looser bound than b",
}

// unleqedFields reports each leaf of spec that has no row in rules, or whose Go
// field the Leq source never reads.
func unleqedFields(spec reflect.Type, rules map[string]string, selectors map[string]bool) []string {
	var missing []string
	for _, l := range walkLeaves(spec, "") {
		if _, ok := rules[l.path]; !ok {
			missing = append(missing, l.path+": no Leq rule (add a row to leqRules and implement it in leq.go)")
		}
		if !selectors[l.goName] {
			missing = append(missing, l.path+": leq.go never reads "+l.goName)
		}
	}
	return missing
}

func TestLeqEveryFieldHasARule(t *testing.T) {
	sel := sourceSelectors(t, "leq.go")
	var all []leafField
	for _, spec := range []reflect.Type{reflect.TypeOf(types.RunPolicySpec{}), reflect.TypeOf(types.GovernanceLimits{})} {
		for _, m := range unleqedFields(spec, leqRules, sel) {
			t.Errorf("%s: %s", spec.Name(), m)
		}
		all = append(all, walkLeaves(spec, "")...)
	}
	for p, r := range leqRules {
		if r == "" {
			t.Errorf("row %q needs a rule", p)
		}
		if !slices.Contains(paths(all), p) {
			t.Errorf("row %q names no field: it was renamed or removed", p)
		}
	}
}

// TestLeqGuardFlagsAnUnhandledField is the guard's own check: a fixture field
// with no row, and one a row names but the source never reads, both come back.
func TestLeqGuardFlagsAnUnhandledField(t *testing.T) {
	type nested struct {
		Known int `json:"known"`
		Fresh int `json:"fresh"`
	}
	type fixture struct {
		Top    bool    `json:"top"`
		Newest string  `json:"newest"`
		Inner  *nested `json:"inner"`
	}
	rules := map[string]string{"top": "x", "inner.known": "x", "newest": "x"}
	got := unleqedFields(reflect.TypeOf(fixture{}), rules, map[string]bool{"Top": true, "Known": true, "Fresh": true})
	want := []string{
		"newest: leq.go never reads Newest",
		"inner.fresh: no Leq rule (add a row to leqRules and implement it in leq.go)",
	}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("guard reported\n %q\nwant\n %q", got, want)
	}
}

// fillLeaves sets every leaf under v to a non-zero value, so a clearing function
// that forgets a field leaves something behind.
func fillLeaves(v reflect.Value) {
	switch v.Kind() {
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			if v.Type().Field(i).IsExported() {
				fillLeaves(v.Field(i))
			}
		}
	case reflect.Pointer:
		v.Set(reflect.New(v.Type().Elem()))
		fillLeaves(v.Elem())
	case reflect.Slice:
		v.Set(reflect.MakeSlice(v.Type(), 1, 1))
		fillLeaves(v.Index(0))
	case reflect.Bool:
		v.SetBool(true)
	case reflect.Int, reflect.Int64:
		v.SetInt(7)
	case reflect.String:
		v.SetString("x")
	}
}

// TestLeqFallbackClearsEveryField pins the runtime half: the functions that
// clear the covered fields must leave a fully populated value empty, or a new
// field would be compared as covered without ever being looked at.
func TestLeqFallbackClearsEveryField(t *testing.T) {
	var s types.RunPolicySpec
	fillLeaves(reflect.ValueOf(&s).Elem())
	if got := withoutCeilingRules(s); !reflect.DeepEqual(got, types.RunPolicySpec{}) {
		t.Errorf("withoutCeilingRules left %+v behind: a field has no clearing line in leq_fallback.go", got)
	}
	var l types.GovernanceLimits
	fillLeaves(reflect.ValueOf(&l).Elem())
	if got := withoutLimitRules(l); !reflect.DeepEqual(got, types.GovernanceLimits{}) {
		t.Errorf("withoutLimitRules left %+v behind: a field has no clearing line in leq_fallback.go", got)
	}
}

// TestLeqFalseForAChangedFieldOutsideTheRules shows the fail-closed door: two
// values that differ only in a field the covering function does not clear are
// not equal outside the rules, so Leq could not prove narrowing.
func TestLeqFalseForAChangedFieldOutsideTheRules(t *testing.T) {
	type fixture struct {
		Covered   int
		Uncovered string
	}
	clearCovered := func(f fixture) fixture { f.Covered = 0; return f }
	if !equalOutside(fixture{Covered: 1}, fixture{Covered: 9}, clearCovered) {
		t.Error("a difference in a covered field must be left to its rule")
	}
	if equalOutside(fixture{Uncovered: "narrower?"}, fixture{Uncovered: "wider?"}, clearCovered) {
		t.Error("a difference in an uncovered field must not be treated as equal")
	}
}
