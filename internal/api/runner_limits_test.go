// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/federation"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func (f *runnerRegistrationFake) SetRunnerMachineName(_ context.Context, id uuid.UUID, name string) error {
	f.lock.Lock()
	defer f.lock.Unlock()
	if row, ok := f.rows[id]; ok && row.State != types.RunnerRevoked {
		row.MachineName = name
		f.rows[id] = row
	}
	return nil
}

// registerWith redeems a fresh token with req's max_runs and machine name, and returns the answer and
// whether the token is still unspent.
func registerWith(t *testing.T, srv *Server, f *runnerRegistrationFake, maxRuns int, machine string) (int, types.RunnerRegistration, bool, string) {
	t.Helper()
	raw := newBearer("wdr_")
	now := time.Now().UTC()
	f.lock.Lock()
	f.tokens[raw] = types.RunnerRegistrationToken{ID: uuid.New(), Owner: "alice", CreatedAt: now, ExpiresAt: now.Add(time.Hour), OrgURLSHA256: federation.OrgURLSHA256(srv.cfg.RunnerOrgURL)}
	f.lock.Unlock()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(types.RunnerRegisterRequest{Token: raw, PublicKey: pub, Name: "laptop", Limits: &types.RunnerLimits{RunsAtOnce: maxRuns}, MachineName: machine})
	w := do(t, srv, http.MethodPost, "/api/v1/runners/register", "", string(body))
	var out types.RunnerRegistration
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	f.lock.Lock()
	unspent := f.tokens[raw].ConsumedAt == nil
	f.lock.Unlock()
	return w.Code, out, unspent, w.Body.String()
}

// Registration stores the runs at once and machine name it is sent, the default when an older runner
// sends none, and refuses a value out of bounds before it spends the token.
func TestRunnerRegistrationStoresMaxRunsAndMachineName(t *testing.T) {
	srv, f, _ := newRunnerRegistrationServer(t)
	code, reg, _, _ := registerWith(t, srv, f, 5, "alice-mbp.corp")
	if code != http.StatusCreated || f.rows[reg.RunnerID].MaxRuns != 5 || f.rows[reg.RunnerID].MachineName != "alice-mbp.corp" {
		t.Fatalf("explicit: %d %+v row %+v", code, reg, f.rows[reg.RunnerID])
	}
	code, reg, _, _ = registerWith(t, srv, f, 0, "")
	if code != http.StatusCreated || f.rows[reg.RunnerID].MaxRuns != types.RunnerMaxRunsDefault {
		t.Fatalf("omitted max_runs: %d, stored %d, want the default", code, f.rows[reg.RunnerID].MaxRuns)
	}
	for _, tc := range []struct {
		name    string
		maxRuns int
		machine string
		reason  string
	}{
		{"max_runs past the ceiling", types.RunnerMaxRunsCeiling + 1, "", reasonRunnerRegistrationInvalid},
		{"negative max_runs", -1, "", reasonRunnerRegistrationInvalid},
		{"over-long machine name", 2, strings.Repeat("m", types.MachineNameMax+1), reasonRunnerRegistrationInvalid},
		{"control character in machine name", 2, "box\x1b[31m", reasonRunnerRegistrationInvalid},
	} {
		code, _, unspent, body := registerWith(t, srv, f, tc.maxRuns, tc.machine)
		if code != http.StatusUnprocessableEntity || !unspent || !strings.Contains(body, `"reason":"`+tc.reason+`"`) {
			t.Errorf("%s: %d %s, token unspent %v; want 422 %s keeping the token", tc.name, code, body, unspent, tc.reason)
		}
	}
}

// Only the runner's owner changes its runs at once, audited with the value before and after; another
// person, an administrator's own session and an administrator's token cannot.
func TestRunnerMaxRunsIsTheOwnersToChange(t *testing.T) {
	srv, f, h := newRunnerRegistrationServer(t)
	row := f.addRunner(srv, "alice", "laptop", types.RunnerClaimed, time.Hour)
	f.lock.Lock()
	r := f.rows[row.ID]
	r.MaxRuns = 2
	f.rows[row.ID] = r
	f.lock.Unlock()
	path := "/api/v1/me/runners/" + row.ID.String()

	alice := memberModeSSOSession(t, "alice", "alice@example.com", oidc.RoleUser, false)
	w := doSSO(t, srv, http.MethodPut, path, alice, `{"max_runs":4}`)
	var v types.RunnerView
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &v) != nil || f.rows[row.ID].MaxRuns != 4 {
		t.Fatalf("owner update = %d %s", w.Code, w.Body.String())
	}
	ev := lastAuditEvent(t, h.audit.snapshot(), "runner.update")
	var data struct {
		MaxRuns struct{ Before, After int } `json:"max_runs"`
	}
	if err := json.Unmarshal(ev.Data, &data); err != nil || data.MaxRuns.Before != 2 || data.MaxRuns.After != 4 || ev.Actor != "alice" || ev.Target != row.ID.String() {
		t.Fatalf("runner.update row = %+v %s", ev, ev.Data)
	}

	for name, cookie := range map[string]*http.Cookie{
		"another person": memberModeSSOSession(t, "bob", "bob@example.com", oidc.RoleUser, false),
		"an admin":       memberModeSSOSession(t, "carol", "carol@example.com", oidc.RoleAdmin, false),
	} {
		if w := doSSO(t, srv, http.MethodPut, path, cookie, `{"max_runs":9}`); w.Code != http.StatusNotFound {
			t.Errorf("%s = %d %s, want the unknown-id 404", name, w.Code, w.Body.String())
		}
	}
	if w := do(t, srv, http.MethodPut, path, adminToken, `{"max_runs":9}`); w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), reasonRunnerSessionRequired) {
		t.Errorf("admin token = %d %s", w.Code, w.Body.String())
	}
	for _, body := range []string{`{}`, `{"max_runs":0}`, `{"max_runs":65}`, `{"max_runs":2,"name":"x"}`} {
		if w := doSSO(t, srv, http.MethodPut, path, alice, body); w.Code != http.StatusBadRequest {
			t.Errorf("owner body %s = %d %s, want 400", body, w.Code, w.Body.String())
		}
	}
	if f.rows[row.ID].MaxRuns != 4 {
		t.Fatalf("a refused update changed max_runs to %d", f.rows[row.ID].MaxRuns)
	}
}

// Each connect's caps event updates the machine name, the latest winning; a name over the bound or with a
// control character is not stored and does not refuse the event.
func TestRunnerCapsUpdateTheMachineName(t *testing.T) {
	srv, f, _ := newRunnerRegistrationServer(t)
	row := f.addRunner(srv, "alice", "laptop", types.RunnerClaimed, time.Hour)
	caps := func(name string) runnerwire.Event {
		data, _ := json.Marshal(runnerwire.Caps{Version: "0.9.0", MachineName: name})
		return runnerwire.Event{Kind: runnerwire.EventCaps, Data: data}
	}
	for _, step := range []struct{ sent, want string }{
		{"old-name", "old-name"},
		{"new-name.corp", "new-name.corp"},
		{strings.Repeat("x", types.MachineNameMax+1), "new-name.corp"},
		{"bad\nname", "new-name.corp"},
		{"  spaced.corp \t", "spaced.corp"},
		{"", "spaced.corp"},
	} {
		if err := srv.EventFromRunner(context.Background(), row.ID, caps(step.sent)); err != nil {
			t.Fatalf("caps %q refused: %v", step.sent, err)
		}
		if got := f.rows[row.ID].MachineName; got != step.want {
			t.Fatalf("after caps %q the machine name is %q, want %q", step.sent, got, step.want)
		}
	}
	alice := memberModeSSOSession(t, "alice", "alice@example.com", oidc.RoleUser, false)
	if v := viewIDs(runnerViewsOf(t, doSSO(t, srv, http.MethodGet, "/api/v1/me/runners", alice, "")))[row.ID]; v.MachineName != "spaced.corp" {
		t.Fatalf("own view machine_name = %q", v.MachineName)
	}
	sec := memberModeSSOSession(t, "sam", "sam@example.com", oidc.RoleSecurityAdmin, false)
	if v := viewIDs(runnerViewsOf(t, doSSO(t, srv, http.MethodGet, "/api/v1/runners", sec, "")))[row.ID]; v.MachineName != "spaced.corp" {
		t.Fatalf("admin view machine_name = %q", v.MachineName)
	}
}
