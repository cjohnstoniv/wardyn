// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestGetGovernanceEnvelopeSetByForUnitAdmins: a unit admin reads who set the envelope above them as
// "security", never the setter's principal or address; the security tier reads the principal as stored.
func TestGetGovernanceEnvelopeSetByForUnitAdmins(t *testing.T) {
	e := newUnitAdminEnv(t, false)
	if w := e.call(e.alice, http.MethodPut, envelopePath(e.a.ID), envelopeBody("plat-a")); w.Code != http.StatusOK {
		t.Fatalf("set the envelope = %d %s", w.Code, w.Body)
	}
	setBy := func(name string, c *http.Cookie) string {
		w := e.call(c, http.MethodGet, "/api/v1/governance", "")
		var body struct {
			Envelopes []types.GovernanceEnvelope `json:"envelopes"`
		}
		if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &body) != nil || len(body.Envelopes) != 1 {
			t.Fatalf("%s: GET /governance = %d %s, want one envelope", name, w.Code, w.Body)
		}
		return body.Envelopes[0].SetBy
	}
	if got := setBy("security", e.alice); got != govSubAlice {
		t.Errorf("security reads set_by %q, want the principal %q", got, govSubAlice)
	}
	for name, c := range map[string]*http.Cookie{"admin of the root": e.adminA, "admin below the root": e.adminA1} {
		if got := setBy(name, c); got != "security" {
			t.Errorf("%s reads set_by %q, want \"security\"", name, got)
		}
	}
}
