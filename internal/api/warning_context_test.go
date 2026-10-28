// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Each of the two policy drop warnings gets the policy the member is bound by, at its own index;
// every other warning, a workspace's dropped clone host included, gets none, and an operator none at all.
func TestWarningContexts(t *testing.T) {
	srv := govServer(&capStore{site: types.SiteConfig{PolicyHelp: policyContactFor("deploy")}})
	warnings := []string{
		"confinement raised from \"CC1\" to operator minimum \"CC2\"",
		"dropped 2 egress domain(s) not in operator allowlist: a.test,b.test",
		`dropped clone host "10.0.0.5" of workspace "w": a private address not in operator allowlist; ask`,
		`dropped private address entry "10.0.0.6": not in operator allowlist, and allow_all_egress does not cover a private address`,
	}
	got := srv.warningContexts(context.Background(), governanceCeiling{}, warnings)
	if len(got) != 2 || got[0].WarningIndex != 1 || got[1].WarningIndex != 3 {
		t.Fatalf("contexts = %+v, want indexes 1 and 3", got)
	}
	for _, c := range got {
		if c.Kind != warningKindEgressPolicyDrop || c.Policy == nil || c.Policy.Owner != "deploy Owner" || c.Policy.Layer != "" {
			t.Errorf("context = %+v, want the deployment's policy and contact, no layer", c)
		}
	}
	if got := srv.warningContexts(context.Background(), governanceCeiling{Operator: true}, warnings); got != nil {
		t.Errorf("operator contexts = %+v, want none", got)
	}
	if got := srv.warningContexts(context.Background(), governanceCeiling{}, warnings[:1]); got != nil {
		t.Errorf("no drop warning: contexts = %+v, want none", got)
	}
}
