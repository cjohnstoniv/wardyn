// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package placement

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// Every action's verb is in the closed list docs/AUDIT-ACTIONS.md keeps; `claim` is the one added for runners.
func TestAuditActionsUseTheClosedVerbList(t *testing.T) {
	raw, err := os.ReadFile("../../docs/AUDIT-ACTIONS.md")
	if err != nil {
		t.Fatal(err)
	}
	verbs := map[string]bool{}
	for _, line := range strings.Split(string(raw), "\n") {
		if rest, ok := strings.CutPrefix(line, "**Verbs:**"); ok {
			for _, m := range regexp.MustCompile("`([a-z_]+)`").FindAllStringSubmatch(rest, -1) {
				verbs[m[1]] = true
			}
		}
	}
	if !verbs["claim"] {
		t.Fatal("the verb list has no `claim`")
	}
	seg := regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
	for _, a := range AuditActions {
		parts := strings.Split(a, ".")
		if len(parts) < 2 || len(parts) > 3 {
			t.Errorf("%s: not <noun>[.<sub>].<verb>", a)
			continue
		}
		for _, p := range parts {
			if !seg.MatchString(p) {
				t.Errorf("%s: segment %q is not snake_case", a, p)
			}
		}
		if v := parts[len(parts)-1]; !verbs[v] {
			t.Errorf("%s: verb %q is not in the closed list", a, v)
		}
	}
	if len(AuditActions) != 14 {
		t.Errorf("%d actions, design §12.2 lists 14", len(AuditActions))
	}
}
