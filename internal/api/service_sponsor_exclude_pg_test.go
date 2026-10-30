// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// listedIDs is the run ids a person's GET /runs returns, in order.
func (e sponsorEnv) listedIDs(t *testing.T, query string) []uuid.UUID {
	t.Helper()
	w := doSSO(t, e.h.srv, http.MethodGet, "/api/v1/runs"+query, e.sponsor, "")
	var runs []types.AgentRun
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &runs) != nil {
		t.Fatalf("GET /runs%s: %d %s", query, w.Code, w.Body.String())
	}
	out := make([]uuid.UUID, 0, len(runs))
	for _, r := range runs {
		out = append(out, r.ID)
	}
	return out
}

// TestRunsList_SponsoredExclude: ?sponsored=exclude (#2513) turns off only the widening that adds the runs of
// the service identities a person sponsors to their own list. A person with no run of their own gets none
// back; a normal list still carries the service's run; an older run of their own, behind a newer service run
// and killed more than a day ago, is the one a limit=1 first-run check finds; a bad value is refused by name.
func TestRunsList_SponsoredExclude(t *testing.T) {
	e := newSponsorEnv(t)
	service := e.seedRun(t, svcPrin, nil)
	const first = "?view=user&limit=1&include_killed=1"

	if got := e.listedIDs(t, first+"&sponsored=exclude"); len(got) != 0 {
		t.Errorf("sponsored=exclude with no run of their own lists %v, want none", got)
	}
	if got := e.listedIDs(t, first); len(got) != 1 || got[0] != service.ID {
		t.Errorf("the default list = %v, want the sponsored service's run %s", got, service.ID)
	}

	ended := time.Now().Add(-48 * time.Hour)
	own := e.seedRun(t, svcSponsor, func(r *types.AgentRun) {
		r.State = types.RunKilled
		r.EndedAt = &ended
	})
	if got := e.listedIDs(t, first+"&sponsored=exclude"); len(got) != 1 || got[0] != own.ID {
		t.Errorf("sponsored=exclude, limit 1 = %v, want their own killed run %s behind the newer service run", got, own.ID)
	}
	if got := e.listedIDs(t, first); len(got) != 1 || got[0] != service.ID {
		t.Errorf("the default list, limit 1 = %v, want the service's run %s first", got, service.ID)
	}

	w := doSSO(t, e.h.srv, http.MethodGet, "/api/v1/runs?view=user&sponsored=nonsense", e.sponsor, "")
	if w.Code != http.StatusBadRequest || errorReason(w) != reasonInvalidSponsoredParam {
		t.Errorf("sponsored=nonsense: %d reason %q, want 400 %q; body=%s", w.Code, errorReason(w), reasonInvalidSponsoredParam, w.Body.String())
	}
}
