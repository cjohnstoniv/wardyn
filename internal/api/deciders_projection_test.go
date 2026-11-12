// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestDecidersProjectionNamesTheRulesLimits: a PENDING row a rule governs names, beside its deciders, the rule's
// max_scope and second_human, so the console can grey out a wider scope and say a second person decides. A row
// no rule governs carries neither, nor deciders.
func TestDecidersProjectionNamesTheRulesLimits(t *testing.T) {
	f := newDCFix(t)
	egress := dcRule(types.DeciderKindEgressDomain, types.DeciderUnitAdmins)
	egress.MaxScope = types.ScopeOnce
	f.st.setRules(f.root.ID, egress, dcRule(types.DeciderKindCredential, types.DeciderUnitAdmins))
	all := f.seedAll()
	w, rows := f.inbox(dcSecurity(t, dcSec), "?state=PENDING")
	if w.Code != http.StatusOK {
		t.Fatalf("list: %d %s", w.Code, w.Body)
	}
	byID := map[string]map[string]any{}
	for _, r := range rows {
		byID[r["id"].(string)] = r
	}
	cases := []struct {
		kind        string
		maxScope    any
		secondHuman any
	}{
		{types.DeciderKindEgressDomain, string(types.ScopeOnce), nil},
		{types.DeciderKindCredential, nil, true},
		{"plain", nil, nil},
	}
	for _, c := range cases {
		r, ok := byID[all[c.kind].ID.String()]
		if !ok {
			t.Fatalf("%s: row not listed", c.kind)
		}
		if r["max_scope"] != c.maxScope || r["second_human"] != c.secondHuman {
			t.Errorf("%s: max_scope %v second_human %v, want %v %v", c.kind, r["max_scope"], r["second_human"], c.maxScope, c.secondHuman)
		}
	}
	if _, has := byID[all["plain"].ID.String()]["deciders"]; has {
		t.Errorf("a row no rule governs carries deciders")
	}
}
