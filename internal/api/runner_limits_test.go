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

func (f *runnerRegistrationFake) SetRunnerArch(_ context.Context, id uuid.UUID, arch string) error {
	f.lock.Lock()
	defer f.lock.Unlock()
	if row, ok := f.rows[id]; ok && row.State != types.RunnerRevoked {
		row.Arch = arch
		f.rows[id] = row
	}
	return nil
}

func (f *runnerRegistrationFake) SetRunnerMachineName(_ context.Context, id uuid.UUID, name string) error {
	f.lock.Lock()
	defer f.lock.Unlock()
	if row, ok := f.rows[id]; ok && row.State != types.RunnerRevoked {
		row.MachineName = name
		f.rows[id] = row
	}
	return nil
}

func (f *runnerRegistrationFake) TouchRunner(_ context.Context, id uuid.UUID, version string, now time.Time) error {
	f.lock.Lock()
	defer f.lock.Unlock()
	if row, ok := f.rows[id]; ok {
		if version != "" {
			row.Version = version
		}
		row.LastSeenAt = &now
		f.rows[id] = row
	}
	return nil
}

// A caps event records the version the runner reports and a last_seen_at, whatever the row held.
func TestRunnerCapsRecordTheVersionAndLastSeen(t *testing.T) {
	srv, f, _ := newRunnerRegistrationServer(t)
	row := f.addRunner(srv, "alice", "laptop", types.RunnerClaimed, time.Hour)
	data, _ := json.Marshal(runnerwire.Caps{Version: "0.9.3-test"})
	if err := srv.EventFromRunner(context.Background(), row.ID, runnerwire.Event{Kind: runnerwire.EventCaps, Data: data}); err != nil {
		t.Fatal(err)
	}
	f.lock.Lock()
	got := f.rows[row.ID]
	f.lock.Unlock()
	if got.Version != "0.9.3-test" || got.LastSeenAt == nil {
		t.Fatalf("runner row = version %q, last seen %v; want 0.9.3-test and a time", got.Version, got.LastSeenAt)
	}
}

// testRunnerLimits is a valid limit set: 2 runs at once, each of them half the totals.
func testRunnerLimits() *types.RunnerLimits {
	return &types.RunnerLimits{RunsAtOnce: 2, Total: types.RunnerAmounts{CPUMillis: 4000, MemoryMiB: 8192, DiskMiB: 102400},
		PerRun: types.RunnerAmounts{CPUMillis: 2000, MemoryMiB: 4096, DiskMiB: 51200}}
}

// registerWith redeems a fresh token with limits and machine name, and returns the answer and whether
// the token is still unspent.
func registerWith(t *testing.T, srv *Server, f *runnerRegistrationFake, limits *types.RunnerLimits, machine string) (int, types.RunnerRegistration, bool, string) {
	t.Helper()
	return registerWithArch(t, srv, f, limits, machine, "")
}

func registerWithArch(t *testing.T, srv *Server, f *runnerRegistrationFake, limits *types.RunnerLimits, machine, arch string) (int, types.RunnerRegistration, bool, string) {
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
	body, _ := json.Marshal(types.RunnerRegisterRequest{Token: raw, PublicKey: pub, Name: "laptop", Limits: limits, MachineName: machine, Arch: arch})
	w := do(t, srv, http.MethodPost, "/api/v1/runners/register", "", string(body))
	var out types.RunnerRegistration
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	f.lock.Lock()
	unspent := f.tokens[raw].ConsumedAt == nil
	f.lock.Unlock()
	return w.Code, out, unspent, w.Body.String()
}

// Registration stores the limit set and machine name it is sent, and refuses a register that declares
// no limits (an older wardyn-runnerd's) or limits out of bounds before it spends the token.
func TestRunnerRegistrationRequiresLimits(t *testing.T) {
	srv, f, _ := newRunnerRegistrationServer(t)
	want := testRunnerLimits()
	code, reg, _, _ := registerWith(t, srv, f, want, "alice-mbp.corp")
	row := f.rows[reg.RunnerID]
	if code != http.StatusCreated || reg.Limits == nil || *reg.Limits != *want || row.Limits == nil || *row.Limits != *want || row.MachineName != "alice-mbp.corp" {
		t.Fatalf("explicit: %d %+v row %+v", code, reg, row)
	}
	past := *want
	past.RunsAtOnce = types.RunnerMaxRunsCeiling + 1
	wide := *want
	wide.PerRun.DiskMiB = wide.Total.DiskMiB + 1024
	for _, tc := range []struct {
		name    string
		limits  *types.RunnerLimits
		machine string
		reason  string
	}{
		{"no limits", nil, "", reasonRunnerLimitsRequired},
		{"runs at once past the ceiling", &past, "", reasonRunnerLimitsRequired},
		{"a per-run limit past its total", &wide, "", reasonRunnerLimitsRequired},
		{"over-long machine name", want, strings.Repeat("m", types.MachineNameMax+1), reasonRunnerRegistrationInvalid},
		{"control character in machine name", want, "box\x1b[31m", reasonRunnerRegistrationInvalid},
	} {
		code, _, unspent, body := registerWith(t, srv, f, tc.limits, tc.machine)
		if code != http.StatusUnprocessableEntity || !unspent || !strings.Contains(body, `"reason":"`+tc.reason+`"`) {
			t.Errorf("%s: %d %s, token unspent %v; want 422 %s keeping the token", tc.name, code, body, unspent, tc.reason)
		}
	}
}

// Only the runner's owner declares its limits, audited with the set before and after; another person,
// an administrator's own session and an administrator's token cannot.
func TestRunnerLimitsAreTheOwnersToChange(t *testing.T) {
	srv, f, h := newRunnerRegistrationServer(t)
	row := f.addRunner(srv, "alice", "laptop", types.RunnerClaimed, time.Hour)
	path := "/api/v1/me/runners/" + row.ID.String()
	want := testRunnerLimits()
	body, _ := json.Marshal(types.RunnerUpdateRequest{Limits: want})

	alice := memberModeSSOSession(t, "alice", "alice@example.com", oidc.RoleUser, false)
	w := doSSO(t, srv, http.MethodPut, path, alice, string(body))
	var v types.RunnerView
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &v) != nil || v.Limits == nil || *v.Limits != *want || *f.rows[row.ID].Limits != *want {
		t.Fatalf("owner update = %d %s", w.Code, w.Body.String())
	}
	ev := lastAuditEvent(t, h.audit.snapshot(), "runner.update")
	var data struct {
		Limits struct{ Before, After *types.RunnerLimits } `json:"limits"`
	}
	if err := json.Unmarshal(ev.Data, &data); err != nil || data.Limits.Before != nil || data.Limits.After == nil || *data.Limits.After != *want ||
		ev.Actor != "alice" || ev.Target != row.ID.String() {
		t.Fatalf("runner.update row = %+v %s", ev, ev.Data)
	}

	other := *want
	other.RunsAtOnce = 1
	otherBody, _ := json.Marshal(types.RunnerUpdateRequest{Limits: &other})
	for name, cookie := range map[string]*http.Cookie{
		"another person": memberModeSSOSession(t, "bob", "bob@example.com", oidc.RoleUser, false),
		"an admin":       memberModeSSOSession(t, "carol", "carol@example.com", oidc.RoleAdmin, false),
	} {
		if w := doSSO(t, srv, http.MethodPut, path, cookie, string(otherBody)); w.Code != http.StatusNotFound {
			t.Errorf("%s = %d %s, want the unknown-id 404", name, w.Code, w.Body.String())
		}
	}
	if w := do(t, srv, http.MethodPut, path, adminToken, string(otherBody)); w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), reasonRunnerSessionRequired) {
		t.Errorf("admin token = %d %s", w.Code, w.Body.String())
	}
	for _, bad := range []string{`{}`, `{"max_runs":2}`, `{"limits":{"runs_at_once":0}}`,
		`{"limits":{"runs_at_once":65,"total":{"cpu_millis":4000,"memory_mib":8192,"disk_mib":102400},"per_run":{"cpu_millis":2000,"memory_mib":4096,"disk_mib":51200}}}`,
		`{"limits":{"runs_at_once":2,"total":{"cpu_millis":4000,"memory_mib":8192,"disk_mib":102400},"per_run":{"cpu_millis":2000,"memory_mib":4096,"disk_mib":51201}}}`} {
		if w := doSSO(t, srv, http.MethodPut, path, alice, bad); w.Code != http.StatusBadRequest {
			t.Errorf("owner body %s = %d %s, want 400", bad, w.Code, w.Body.String())
		}
	}
	if *f.rows[row.ID].Limits != *want {
		t.Fatalf("a refused update changed the limits to %+v", f.rows[row.ID].Limits)
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

// Registration stores the architecture it is sent, "" for an older runner's; one the runner images are not
// built for is refused (422, like a bad machine name) before the token is spent.
func TestRunnerRegistrationStoresTheArch(t *testing.T) {
	srv, f, _ := newRunnerRegistrationServer(t)
	for _, arch := range []string{"amd64", "arm64", ""} {
		code, reg, _, body := registerWithArch(t, srv, f, testRunnerLimits(), "box", arch)
		if code != http.StatusCreated || f.rows[reg.RunnerID].Arch != arch {
			t.Fatalf("arch %q: %d %s, stored %q", arch, code, body, f.rows[reg.RunnerID].Arch)
		}
	}
	for _, arch := range []string{"x86_64", "AMD64", "riscv64", "arm64\n"} {
		code, _, unspent, body := registerWithArch(t, srv, f, testRunnerLimits(), "box", arch)
		if code != http.StatusUnprocessableEntity || !unspent || !strings.Contains(body, `"reason":"`+reasonRunnerRegistrationInvalid+`"`) || !strings.Contains(body, "arch must be") {
			t.Errorf("arch %q: %d %s, token unspent %v; want 422 keeping the token", arch, code, body, unspent)
		}
	}
}

// A caps event gives a runner registered before 0.9.1 its architecture on its next connect, the latest
// winning; an unknown one and an absent one change nothing and refuse nothing. Both views carry it.
func TestRunnerCapsUpdateTheArch(t *testing.T) {
	srv, f, _ := newRunnerRegistrationServer(t)
	row := f.addRunner(srv, "alice", "laptop", types.RunnerClaimed, time.Hour)
	for _, step := range []struct{ sent, want string }{
		{"amd64", "amd64"},
		{"", "amd64"},
		{"riscv64", "amd64"},
		{"arm64", "arm64"},
	} {
		data, _ := json.Marshal(runnerwire.Caps{Version: "0.9.1", Arch: step.sent})
		if err := srv.EventFromRunner(context.Background(), row.ID, runnerwire.Event{Kind: runnerwire.EventCaps, Data: data}); err != nil {
			t.Fatalf("caps arch %q refused: %v", step.sent, err)
		}
		if got := f.rows[row.ID].Arch; got != step.want {
			t.Fatalf("after caps arch %q the arch is %q, want %q", step.sent, got, step.want)
		}
	}
	alice := memberModeSSOSession(t, "alice", "alice@example.com", oidc.RoleUser, false)
	if v := viewIDs(runnerViewsOf(t, doSSO(t, srv, http.MethodGet, "/api/v1/me/runners", alice, "")))[row.ID]; v.Arch != "arm64" {
		t.Fatalf("own view arch = %q", v.Arch)
	}
	sec := memberModeSSOSession(t, "sam", "sam@example.com", oidc.RoleSecurityAdmin, false)
	if v := viewIDs(runnerViewsOf(t, doSSO(t, srv, http.MethodGet, "/api/v1/runners", sec, "")))[row.ID]; v.Arch != "arm64" {
		t.Fatalf("admin view arch = %q", v.Arch)
	}
}
