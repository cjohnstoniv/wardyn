// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"cmp"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/federation"
	"github.com/cjohnstoniv/wardyn/internal/runner/remote"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// RecordRunnerCaps is store.PG's write with no sessions to fence: every report is the current one's.
func (f *runnerRegistrationFake) RecordRunnerCaps(_ context.Context, id uuid.UUID, _ string, r store.RunnerCapsReport, now time.Time) (bool, error) {
	f.lock.Lock()
	defer f.lock.Unlock()
	row, ok := f.rows[id]
	if !ok {
		return false, nil
	}
	if r.Version != "" {
		row.Version = r.Version
	}
	if row.State != types.RunnerRevoked {
		row.MachineName = cmp.Or(r.MachineName, row.MachineName)
		row.Arch = cmp.Or(r.Arch, row.Arch)
	}
	row.LastSeenAt = &now
	f.rows[id] = row
	return true, nil
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

// A superseded session's late caps leave the new session's owed runner.connect row for the new session's
// own caps, which writes it with its own version.
func TestRunnerCapsOfASupersededSessionLeaveTheOwedConnectRow(t *testing.T) {
	srv, f, h := newRunnerRegistrationServer(t)
	row := f.addRunner(srv, "alice", "laptop", types.RunnerClaimed, time.Hour)
	srv.runnerStreams.connectMu.Lock()
	srv.runnerStreams.connectOwed = map[uuid.UUID]string{row.ID: "new"} // the new session's Connected has run
	srv.runnerStreams.connectMu.Unlock()
	send := func(session, version string) {
		data, _ := json.Marshal(runnerwire.Caps{Version: version})
		if err := srv.eventFromRunner(context.Background(), row.ID, session, runnerwire.Event{Kind: runnerwire.EventCaps, Data: data}); err != nil {
			t.Fatal(err)
		}
	}
	connects := func() (n int, last string) {
		for _, ev := range h.audit.snapshot() {
			if ev.Action == "runner.connect" {
				n++
				last = string(ev.Data)
			}
		}
		return n, last
	}
	send("old", "0.9.2")
	if n, _ := connects(); n != 0 {
		t.Fatalf("a superseded session's caps wrote %d runner.connect rows", n)
	}
	send("new", "0.9.3-test")
	if n, last := connects(); n != 1 || !strings.Contains(last, `"version":"0.9.3-test"`) {
		t.Fatalf("runner.connect rows = %d, last %s; want one carrying 0.9.3-test", n, last)
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

// Changing a connected runner's per-run pair ends its session (GOAWAY), so the fresh one carries the new
// pair in AUTH_OK; without it the runner would keep the old pair for as long as it stayed up. An update
// that leaves the pair as it was (the same limits again, or only the runs at once) leaves the session,
// and the streams on it, alone.
func TestRunnerLimitsUpdateEndsTheRunnersSessionOnlyForAChangedPerRunPair(t *testing.T) {
	cases := []struct {
		name   string
		start  *types.RunnerLimits
		update func(types.RunnerLimits) types.RunnerLimits
		endsIt bool
	}{
		{"limits declared for the first time", nil, func(l types.RunnerLimits) types.RunnerLimits { return l }, true},
		{"a new per-run memory", testRunnerLimits(), func(l types.RunnerLimits) types.RunnerLimits { l.PerRun.MemoryMiB = 2048; return l }, true},
		{"a new per-run CPU", testRunnerLimits(), func(l types.RunnerLimits) types.RunnerLimits { l.PerRun.CPUMillis = 1000; return l }, true},
		{"the same limits again", testRunnerLimits(), func(l types.RunnerLimits) types.RunnerLimits { return l }, false},
		{"only the runs at once", testRunnerLimits(), func(l types.RunnerLimits) types.RunnerLimits { l.RunsAtOnce = 1; return l }, false},
		{"only the totals and the disk", testRunnerLimits(), func(l types.RunnerLimits) types.RunnerLimits {
			l.Total.MemoryMiB, l.PerRun.DiskMiB = 16384, 25600
			return l
		}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv, f, _ := newRunnerRegistrationServer(t)
			row := f.addRunner(srv, "alice", "laptop", types.RunnerClaimed, time.Hour)
			pub, key, _ := ed25519.GenerateKey(rand.Reader)
			row.PublicKey, row.Limits = pub, c.start
			f.lock.Lock()
			f.rows[row.ID] = row
			f.lock.Unlock()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			srv.cfg.RunnerHub.Connected = func(context.Context, uuid.UUID) error { return nil }
			srv.cfg.RunnerHub.Admit = func(context.Context, types.Runner) (remote.Admission, error) {
				link := &remote.Link{}
				return remote.Admission{Substrate: remote.New(row.ID.String(), link, remote.Options{}), Link: link,
					Pending:        func(context.Context) ([]runnerwire.PendingAction, error) { return nil, nil },
					BeginInventory: func(context.Context, string) error { return nil },
					StatePage:      func(context.Context, string, runnerwire.StatePage) error { return nil },
					Event:          func(context.Context, runnerwire.Event) error { return nil },
				}, nil
			}
			hub := srv.runnerHub(f)
			ready := make(chan struct{}, 1)
			client, err := runnerwire.NewClient(runnerwire.ClientConfig{OrgURL: srv.cfg.RunnerOrgURL, RunnerID: row.ID.String(), PrivateKey: key,
				Hooks: runnerwire.RunnerHooks{
					Call:    func(context.Context, *runnerwire.Peer, uint32, string, json.RawMessage) (any, error) { return nil, nil },
					Open:    func(*runnerwire.Peer, *runnerwire.Stream, runnerwire.Open) error { return runnerwire.ErrNotReady },
					Clock:   func(runnerwire.Hello, time.Time) error { return nil },
					Pending: func(context.Context, *runnerwire.Peer, []runnerwire.PendingAction) error { return nil },
					StatePages: func(_ context.Context, send func(runnerwire.StatePage) error) error {
						return send(runnerwire.StatePage{InventoryID: uuid.New(), Final: true})
					},
					Replay:      func(context.Context, *runnerwire.Peer) error { return nil },
					BeforeReady: func(context.Context, *runnerwire.Peer, bool) error { return nil },
					Lease:       func(context.Context, runnerwire.Lease) error { return nil },
					Revoked:     func(context.Context) error { return nil },
					Offline:     func(context.Context) {},
					Online:      func(*runnerwire.Peer, bool) { ready <- struct{}{} },
				},
				Dial: func(context.Context) (runnerwire.Conn, error) {
					org, run := runnerwire.Loopback()
					go func() { _ = hub.Serve(ctx, org, row.ID) }()
					return run, nil
				}})
			if err != nil {
				t.Fatal(err)
			}
			ended := make(chan error, 1)
			go func() { ended <- client.Connect(ctx) }()
			select {
			case <-ready:
			case <-ctx.Done():
				t.Fatal("the runner never reached READY")
			}
			start := *testRunnerLimits()
			if c.start != nil {
				start = *c.start
			}
			body, _ := json.Marshal(types.RunnerUpdateRequest{Limits: ptr(c.update(start))})
			alice := memberModeSSOSession(t, "alice", "alice@example.com", oidc.RoleUser, false)
			if w := doSSO(t, srv, http.MethodPut, "/api/v1/me/runners/"+row.ID.String(), alice, string(body)); w.Code != http.StatusOK {
				t.Fatalf("owner update = %d %s", w.Code, w.Body.String())
			}
			// The GOAWAY is written before the handler answers, so a session left alone shows nothing here.
			wait := 300 * time.Millisecond
			if c.endsIt {
				wait = 5 * time.Second
			}
			select {
			case err := <-ended:
				if !c.endsIt || !errors.Is(err, runnerwire.ErrGoaway) {
					t.Fatalf("the runner's session ended with %v (should end: %v, as a GOAWAY)", err, c.endsIt)
				}
			case <-time.After(wait):
				if c.endsIt {
					t.Fatal("updating the per-run pair did not end the runner's session")
				}
			}
		})
	}
}
