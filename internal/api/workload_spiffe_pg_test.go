// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/identity"
)

// TestPG_Workload_RunsCarryTheWorkloadsSPIFFEID: an applied workload's run is minted under
// spiffe://<td>/workload/<owner>/<name>/run/<run id>, and its replacement under the same prefix with its own
// run id, so a relying party admits both by the prefix and tells them apart by the last segment.
func TestPG_Workload_RunsCarryTheWorkloadsSPIFFEID(t *testing.T) {
	e := newSvcEnv(t)
	ctx := context.Background()
	pol := e.plainPolicy(t, "wl-pol")
	first := launchedRun(t, decodeApply(t, doSSO(t, e.h.srv, http.MethodPut, "/api/v1/workloads/web", e.mem, workloadBody(pol, "t", "")), http.StatusCreated))
	wl, err := e.st.GetWorkloadByRun(ctx, first)
	if err != nil {
		t.Fatal(err)
	}
	w := identity.Workload{Owner: wl.Owner, Name: wl.Name}
	second := launchedRun(t, decodeApply(t, doSSO(t, e.h.srv, http.MethodPut, "/api/v1/workloads/web", e.mem, workloadBody(pol, "t2", "")), http.StatusOK))
	var ids []string
	for _, runID := range []uuid.UUID{first, second} {
		run, err := e.st.GetRun(ctx, runID)
		if err != nil {
			t.Fatal(err)
		}
		path, err := identity.RunPath(w, runID)
		if err != nil {
			t.Fatal(err)
		}
		if want := "spiffe://wardyn.local" + path; run.SPIFFEID != want {
			t.Errorf("run %s SPIFFE ID = %q, want %q", runID, run.SPIFFEID, want)
		}
		ids = append(ids, run.SPIFFEID)
	}
	if a, b := ids[0], ids[1]; !strings.HasPrefix(a, "spiffe://wardyn.local/workload/") || a == b || a[:strings.LastIndex(a, "/")] != b[:strings.LastIndex(b, "/")] {
		t.Errorf("the replaced run %q and its replacement %q do not share the workload's prefix with ids of their own", a, b)
	}
}

// TestPG_Workload_QueuedRunCarriesTheWorkloadsSPIFFEID: a workload's run that waits in a queue is minted
// under the workload's SPIFFE ID when it starts, as an unqueued one is at its create.
func TestPG_Workload_QueuedRunCarriesTheWorkloadsSPIFFEID(t *testing.T) {
	e := newQueueEnv(t)
	pool := e.cappedPool("Busy", 1)
	occupant, running := e.occupy(pool), e.occupy(e.cappedPool("Old", 1))
	queued := e.queuedWorkloadReplacement(pool, running)
	e.free(occupant)
	path, err := identity.RunPath(identity.Workload{Owner: e.personP, Name: "web"}, queued)
	if err != nil {
		t.Fatal(err)
	}
	if got := e.waitStarted(queued).SPIFFEID; got != "spiffe://wardyn.local"+path {
		t.Fatalf("the queued run's SPIFFE ID = %q, want spiffe://wardyn.local%s", got, path)
	}
}
