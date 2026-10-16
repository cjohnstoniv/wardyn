// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/recording"
)

// recordingRelayFixture is the binding fixture with its server rebuilt on rec
// (nil: an organisation with recording off).
func recordingRelayFixture(t *testing.T, rec recording.Store) *bindingFixture {
	t.Helper()
	b := newBindingFixture(t)
	cfg := b.srv.cfg
	cfg.RecordingStore = rec
	b.srv = New(cfg)
	return b
}

// relayPut sends one PUT relayed by runner over a real connection (an upload
// needs a read deadline, which a ResponseRecorder cannot take).
func (b *bindingFixture) relayPut(t *testing.T, runner, runID uuid.UUID, path, body string) (int, string) {
	t.Helper()
	ts := httptest.NewUnstartedServer(b.srv.relayRoutes(b.srv.InternalHandler()))
	ts.Config.ConnContext = func(ctx context.Context, c net.Conn) context.Context {
		return relayConnContext(ctx, relayedConn{Conn: c, runner: runner})
	}
	ts.Start()
	t.Cleanup(ts.Close)
	req, _ := http.NewRequest(http.MethodPut, ts.URL+path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+runToken(t, b.srv, runID))
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	got, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(got)
}

// TestRelay_RecordingUploadByOrgMode is a laptop run's wardyn-rec upload, as
// its proxy relays it: PUT /api/v1/internal/recordings/{run} over the runner's
// relay, with the run's own token. With the organisation's recording off it is
// answered recording_off (before 0.9.3 the route did not exist without a
// store, and the relay's route filter answered relay_route_refused); with it
// on, the cast is kept and its audit row says the runner asserted it.
func TestRelay_RecordingUploadByOrgMode(t *testing.T) {
	fs, err := recording.NewFSStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		rec    recording.Store
		status int
		reason string
	}{{"store off", nil, http.StatusConflict, "recording_off"}, {"store on", fs, http.StatusNoContent, ""}} {
		t.Run(tc.name, func(t *testing.T) {
			b := recordingRelayFixture(t, tc.rec)
			if _, err := b.pool.Exec(b.ctx, `UPDATE agent_runs SET evidence_source = 'runner_asserted' WHERE id = $1`, b.run.ID); err != nil {
				t.Fatal(err)
			}
			path := "/api/v1/internal/recordings/" + b.run.ID.String()
			code, body := b.relayPut(t, b.rn.ID, b.run.ID, path, "{\"version\":2}\n")
			t.Logf("%s: PUT %s relayed by runner %s -> %d %s", tc.name, path, b.rn.ID, code, body)
			if code != tc.status || (tc.reason != "" && !strings.Contains(body, `"reason":"`+tc.reason+`"`)) {
				t.Fatalf("= %d %s, want %d %s", code, body, tc.status, tc.reason)
			}
			if tc.rec == nil {
				return
			}
			rc, err := fs.OpenCast(b.ctx, b.run.ID.String())
			if err != nil {
				t.Fatalf("the relayed cast was not kept: %v", err)
			}
			_ = rc.Close()
			ev, ok := relayAudit(b.srv, b.run.ID, "recording.upload")
			if !ok || ev.Outcome != "success" || auditData(t, ev)["evidence_source"] != "runner_asserted" {
				t.Fatalf("recording.upload row = %+v, %v; want a success marked runner_asserted", ev, ok)
			}
		})
	}
}

// TestRelay_RecordingOffStaysBehindAuth: the no-store answer is given only to
// a request internalAuth and the relay binding admit; nothing is answered
// recording_off to a caller with no token, another runner's run, or a local
// run's upload that skipped its relay.
func TestRelay_RecordingOffStaysBehindAuth(t *testing.T) {
	b := recordingRelayFixture(t, nil)
	path := func(id uuid.UUID) string { return "/api/v1/internal/recordings/" + id.String() }

	req := httptest.NewRequest(http.MethodPut, path(b.run.ID), strings.NewReader("{}"))
	req = req.WithContext(relayConnContext(req.Context(), relayedConn{runner: b.rn.ID}))
	w := httptest.NewRecorder()
	b.srv.relayRoutes(b.srv.InternalHandler()).ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("no token: %d %s, want 401", w.Code, w.Body.String())
	}
	for _, tc := range []struct {
		name   string
		runner uuid.UUID
		run    uuid.UUID
		reason string
	}{
		{"another runner's run", b.rn.ID, b.other.ID, "relay_runner_mismatch"},
		{"a local run not relayed", uuid.Nil, b.run.ID, "relay_required"},
	} {
		w := b.do(t, tc.runner, http.MethodPut, path(tc.run), tc.run, "{}")
		if w.Code != http.StatusForbidden || reasonOf(t, w) != tc.reason {
			t.Errorf("%s: %d %s, want 403 %s", tc.name, w.Code, w.Body.String(), tc.reason)
		}
	}
}
