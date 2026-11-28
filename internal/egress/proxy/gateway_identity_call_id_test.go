// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"net/http"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/egress"
)

// Two sends on one route inside the token's life carry one identity token and two call
// ids, and each id is the one its allow row names. The token names the run, not the call.
func TestGatewayIdentity_OneTokenDistinctCallIDs(t *testing.T) {
	g := newGwidRig(t, gwidRoutes(), false)
	var tokens, ids []string
	for range 2 {
		if rec := gwidLanes(g, t)["brokered"](); rec.Code != http.StatusOK {
			t.Fatalf("status %d: %s", rec.Code, rec.Body)
		}
		req := g.gw.last(t)
		tokens = append(tokens, req.Header.Get(gatewayIdentityHeader))
		ids = append(ids, req.Header.Get(gatewayCallIDHeader))
	}
	if tokens[0] == "" || tokens[0] != tokens[1] {
		t.Errorf("the two sends carried tokens %q and %q, want one", tokens[0], tokens[1])
	}
	if ids[0] == "" || ids[0] == ids[1] {
		t.Errorf("the two sends carried call ids %q and %q, want two", ids[0], ids[1])
	}
	rowIDs := map[string]bool{}
	for _, row := range gwcidRows(t, g.buf) {
		if row["decision"] == string(egress.Allow) && (row["rule_source"] == ruleSourceLLM || row["rule_source"] == ruleSourceLLMMITM) {
			id, _ := row["gateway_call_id"].(string)
			rowIDs[id] = true
		}
	}
	for _, id := range ids {
		if !rowIDs[id] {
			t.Errorf("no allow row carries the sent call id %s: %v", id, rowIDs)
		}
	}
}
