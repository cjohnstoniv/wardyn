// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/test/adofake"
)

// When canary 2 leaves the lifespan policy unknown, the answer carries what
// Azure DevOps said (its patTokenError), for the admin's "Azure DevOps
// answered: …" line. An answer that settles the policy carries none.
func TestOrgCheck_UnknownLifespanCarriesTheAnswer(t *testing.T) {
	for _, tc := range []struct {
		name  string
		limit time.Duration
		err   adofake.PatTokenError
		want  string // "" = no lifespan_error
	}{
		{"invalidValidTo", 30 * 24 * time.Hour, adofake.PatTokenErrorInvalidValidTo, `"lifespan_error":"invalidValidTo"`},
		{"another refusal", 30 * 24 * time.Hour, adofake.PatTokenErrorFullScopePolicyViolation, `"lifespan_error":"fullScopePatPolicyViolation"`},
		{"policy on", 7 * 24 * time.Hour, adofake.PatTokenErrorLifespanPolicyViolation, ""},
		{"policy off", 0, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mf := newMintFixture(t)
			mf.connect(t)
			if tc.limit > 0 {
				mf.ado.SetPatLifespanLimit(tc.limit, tc.err)
			}
			w := mf.orgCheck(t, mf.cfg.RowID)
			if w.Code != http.StatusOK {
				t.Fatalf("org check: %d %s", w.Code, w.Body.String())
			}
			body := w.Body.String()
			if tc.want == "" && strings.Contains(body, `"lifespan_error"`) {
				t.Errorf("a settled lifespan carries lifespan_error: %s", body)
			}
			if tc.want != "" && !strings.Contains(body, tc.want) {
				t.Errorf("answer %s, want %s", body, tc.want)
			}
			if rows := mf.audit.find(adoPATAuditOrgCheck); len(rows) != 1 || (tc.want != "" && !strings.Contains(string(rows[0].Data), tc.want)) {
				t.Errorf("ado_pat.org_check rows = %+v, want one carrying %s", rows, tc.want)
			}
		})
	}
}
