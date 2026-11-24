// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"strings"
	"testing"
)

// TestDocsSayWhatTheCodeDoes pins sentences that once said more than, or other than, the code does. Each case names
// the file, the old text that must stay gone (empty when the fix only added text) and the text that replaced it.
func TestDocsSayWhatTheCodeDoes(t *testing.T) {
	ops := []string{"docs", "OPERATIONS.md"}
	for _, c := range []struct {
		name string
		path []string
		gone string
		has  string
	}{
		{"backup reaches the named installation", ops, "docker exec -i wardyn-postgres psql", `exec -T postgres pg_dump -U wardyn wardyn > "$NS-$(date +%F).sql"`},
		{"recordings volume follows WARDYN_NS", ops, "-v wardyn-recordings:/", `docker volume inspect "$NS-recordings"`},
		{"helm upgrade keeps a pinned digest", ops, "", "--set image.digest=<new-digest> --wait --timeout 5m"},
		{"retention status cost", ops, "costs as much as the largest partition", "grows with the total rows"},
		{"own sign-out ends CLI sign-ins", ops, "ends their console sessions only", "ends their browser and CLI sign-ins"},
		{"re-sign-in wait counts answered requests only", ops, "The agent's SDK is giving up before the hold does", "samples only the requests a sign-in answered"},
		{"ha does not raise replicas", ops, "so that the loss of a", "the chart keeps your value, 1 by default"},
		{"desktop restore, both devices", ops, "so a desktop restore recovers runs, audit and drives", "Restored onto a replacement device, which mints its own `age.key`"},
		{"session-only cut", []string{"docs", "operations", "api-tokens.md"}, "No token-borne credential is refused by the cut", "each CLI sign-in ends at once with the tokens it holds"},
		{"session-only cut, CLI section", []string{"docs", "operations", "api-tokens.md"}, "own API tokens working, as before", "personal API tokens and SSH keys working"},
		{"session-only cut, audit catalogue", []string{"docs", "AUDIT-ACTIONS.md"}, "browser sessions end, API tokens and SSH keys keep working", "browser and CLI sign-ins end, with the CLI sign-ins' tokens"},
		{"session-only cut, reason table", []string{"docs", "sdk.md"}, "(browser sessions end; API tokens and SSH keys keep working)", "(browser and CLI sign-ins end; personal API tokens and SSH keys keep working)"},
		{"rehearsal opens credentials and principal keys", []string{"docs", "operations", "recovery-rehearsal.md"}, "open every stored row", "every row of the `secrets` table"},
		{"url flag", []string{"docs", "ENV.md"}, "flag `-url`", "flag `--url`"},
		{"doctor renews a sign-in", []string{"docs", "ENV.md"}, "writes nothing and exits 1", "renews a saved sign-in whose access token is spent"},
		{"doctor help", []string{"cmd", "wardyn", "doctor.go"}, "doctor writes", "renews a saved sign-in whose access token is spent"},
		{"chart README replicas", []string{"deploy", "helm", "wardyn", "README.md"}, "Runs two or more replicas so that one node failing", "It does not raise `replicas`"},
		{"chart values replicas", []string{"deploy", "helm", "wardyn", "values.yaml"}, "two or more wardynd replicas on this cluster, so one node", "does not raise `replicas`"},
		{"runner canary", []string{"docs", "operations", "k8s-runners.md"}, "so a running pod has passed it", "The chart sets no readiness probe"},
		{"checkpointer names the live check", []string{"internal", "auditsign", "checkpointer.go"}, "run wardyn audit chain verify", "GET /api/v1/audit/chain/verify"},
	} {
		t.Run(c.name, func(t *testing.T) {
			doc := readOpsDoc(t, c.path...)
			if c.gone != "" && strings.Contains(doc, c.gone) {
				t.Errorf("%s still says %q", strings.Join(c.path, "/"), c.gone)
			}
			if !strings.Contains(doc, c.has) {
				t.Errorf("%s no longer says %q", strings.Join(c.path, "/"), c.has)
			}
		})
	}
}
