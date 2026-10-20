// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// A runner's offline summary and relay status (#3296, #3298) against a real Postgres: what its caps carry
// is stored redacted, audited, and shown on a one-runner read to its owner and administrators only; a
// runner that sends neither changes nothing. Guarded by WARDYN_TEST_PG: skipped cleanly when unset, must
// PASS when set.

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func TestPG_RunnerHealthStoredRedactedAndShownToItsReaders(t *testing.T) {
	e := newPoolEnv(t, false)
	ctx := context.Background()
	if _, err := e.pg.PutSiteConfig(ctx, types.SiteConfig{Runners: &types.RunnerSettings{Enabled: true}}); err != nil {
		t.Fatal(err)
	}
	id := e.claimedRunner(e.personP)
	at := time.Now().UTC().Add(-time.Hour)
	const secret = "pw-hunter2"
	data := mustJSON(runnerwire.Caps{Version: "0.10.0",
		OfflineSummary: &types.RunnerOfflineSummary{Stage: types.RunnerStageProxy, FirstFailureAt: at, LastFailureAt: at.Add(time.Minute),
			Reason: "proxyconnect tcp: dial http://bob:" + secret + "@proxy.corp.example:3128: connection refused"},
		Relay: &types.RunnerRelayStatus{State: types.RelayUnreachable, Since: at, Reason: "relay: nothing answers at 172.17.0.1:8443"}})
	e.srv.capsFromRunner(ctx, id, "", runnerwire.Event{Kind: runnerwire.EventCaps, Data: data})

	rows := e.audits(placement.ActionRunnerOfflineSummary)
	if len(rows) != 1 || rows[0].Actor != "runner:"+id.String() || strings.Contains(string(rows[0].Data), secret) {
		t.Fatalf("audit rows = %+v; want one runner.offline_summary.record by the runner, with no credential", rows)
	}
	read := func(w interface{ Result() *http.Response }, code int) types.RunnerView {
		t.Helper()
		res := w.Result()
		defer res.Body.Close()
		if res.StatusCode != code {
			t.Fatalf("status %d, want %d", res.StatusCode, code)
		}
		var v types.RunnerView
		if code == http.StatusOK {
			if err := json.NewDecoder(res.Body).Decode(&v); err != nil {
				t.Fatal(err)
			}
		}
		return v
	}
	for name, v := range map[string]types.RunnerView{
		"owner": read(e.call(e.person, http.MethodGet, "/api/v1/me/runners/"+id.String(), ""), http.StatusOK),
		"admin": read(e.admin(http.MethodGet, "/api/v1/runners/"+id.String(), ""), http.StatusOK),
	} {
		s, r := v.OfflineSummary, v.RelayStatus
		if s == nil || s.Stage != types.RunnerStageProxy || strings.Contains(s.Reason, secret) || strings.Contains(s.Reason, "bob") || s.ReportedAt.IsZero() {
			t.Fatalf("%s read: offline_summary %+v; want the proxy stage, redacted, with its receipt time", name, s)
		}
		if r == nil || r.State != types.RelayUnreachable {
			t.Fatalf("%s read: relay_status %+v", name, r)
		}
	}
	read(e.call(e.other, http.MethodGet, "/api/v1/me/runners/"+id.String(), ""), http.StatusNotFound)
	if w := e.call(e.person, http.MethodGet, "/api/v1/runners/"+id.String(), ""); w.Code != http.StatusForbidden {
		t.Fatalf("a person on the administrators' read: %d, want 403", w.Code)
	}

	// An older runner's caps carry neither part: its version is kept and no health row appears.
	old := e.claimedRunner(e.personP)
	e.srv.capsFromRunner(ctx, old, "", runnerwire.Event{Kind: runnerwire.EventCaps, Data: []byte(`{"version":"0.9.6","retire":true}`)})
	v := read(e.call(e.person, http.MethodGet, "/api/v1/me/runners/"+old.String(), ""), http.StatusOK)
	if v.OfflineSummary != nil || v.RelayStatus != nil {
		t.Fatalf("an older runner shows health %+v %+v", v.OfflineSummary, v.RelayStatus)
	}
	if n := len(e.audits(placement.ActionRunnerOfflineSummary)); n != 1 {
		t.Fatalf("%d runner.offline_summary.record rows after an older runner's caps, want 1", n)
	}
}
