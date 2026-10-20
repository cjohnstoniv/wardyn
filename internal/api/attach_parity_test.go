// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/maskmanifest"
	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestAttachParityTable (DESIGN-ATTACH 4.4, C1): every refusal the attach socket gives on the relay it
// gives with transport=local, with the same status and reason, because the local checks run only after
// every one of them. Each row is set up so that the local preconditions hold (the flag on, alice's own
// person runner online and able): a local attempt that skipped a check would be served, not refused.
// Rows 1, 2, 4 and 8 of 4.4 are the mint's (POST /attach/ticket), which knows nothing of the
// transport; 16 and 17 are the caller's; 18 and 19 end open sessions (attach_local_test.go).
func TestAttachParityTable(t *testing.T) {
	profile := types.GovernanceProfile{ID: uuid.New(), Name: "ci", Limits: types.GovernanceLimits{DenyInteractive: true}}
	deadMasks := func(t *testing.T) func(*Config) {
		pool, err := pgxpool.New(context.Background(), "postgres://wardyn@127.0.0.1:1/wardyn?connect_timeout=1&sslmode=disable")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(pool.Close)
		return func(c *Config) { c.MaskManifests = maskmanifest.New(pool, noKeys{}, secretmask.NewRegistry()) }
	}
	rows := []struct {
		row       string
		opts      func(t *testing.T) localRigOpts
		principal string
		role      string
		ticket    string // overrides the minted ticket
		prepare   func(t *testing.T, rig *localRig)
		status    int
		reason    string
	}{
		{row: "3 super admin on a person's run", principal: holderSecond, role: oidc.RoleAdmin,
			status: http.StatusForbidden, reason: runOwnerOnlyReason},
		{row: "5 background-only run", opts: func(*testing.T) localRigOpts {
			return localRigOpts{run: func(r *types.AgentRun) { r.Experience = types.ExperienceBackground }}
		}, status: http.StatusConflict, reason: reasonRunBackgroundOnly},
		{row: "6 not running", opts: func(*testing.T) localRigOpts {
			return localRigOpts{run: func(r *types.AgentRun) { r.State = types.RunStarting }}
		}, status: http.StatusConflict, reason: reasonAttachNotRunning},
		{row: "6 kept", opts: func(*testing.T) localRigOpts {
			return localRigOpts{run: func(r *types.AgentRun) { now := time.Now(); r.LostAt = &now }}
		}, status: http.StatusConflict, reason: reasonAttachRunKept},
		{row: "6 no sandbox", opts: func(*testing.T) localRigOpts {
			return localRigOpts{run: func(r *types.AgentRun) { r.SandboxRef = "" }}
		}, status: http.StatusConflict, reason: reasonAttachNoSandbox},
		{row: "7 session cap", prepare: func(t *testing.T, rig *localRig) {
			for {
				release, ok := rig.srv.reserveAttachSlot(rig.run, holderOwner)
				if !ok {
					return
				}
				t.Cleanup(release)
			}
		}, status: http.StatusServiceUnavailable, reason: reasonAttachSessionCapReached},
		{row: "9 ticket invalid", ticket: "not-a-ticket", status: http.StatusForbidden, reason: reasonUIGatewayTicketInvalid},
		{row: "10 ticket principal not the owner", principal: holderSecond, role: oidc.RoleUser,
			status: http.StatusForbidden, reason: reasonAttachTicketNotYourRun},
		{row: "11 deny_interactive", opts: func(*testing.T) localRigOpts {
			return localRigOpts{run: func(r *types.AgentRun) { r.Task, r.GovernanceProfileID = "make test", &profile.ID }}
		}, prepare: func(_ *testing.T, rig *localRig) { rig.st.profiles = []types.GovernanceProfile{profile} },
			status: http.StatusForbidden, reason: string(authz.ReasonGovernanceProfile)},
		{row: "13 mask state unprovable", opts: func(t *testing.T) localRigOpts { return localRigOpts{cfg: deadMasks(t)} },
			status: http.StatusServiceUnavailable, reason: string(authz.ReasonMaskStateUnavailable)},
		{row: "14 thaw failed", opts: func(*testing.T) localRigOpts {
			return localRigOpts{wrap: func(ls *localAttachStore) store.Store { return localThawFailStore{localAttachStore: ls} }}
		}, status: http.StatusBadGateway, reason: reasonAttachResumeFailed},
		{row: "15 no runner", opts: func(*testing.T) localRigOpts {
			return localRigOpts{cfg: func(c *Config) { c.Runner = nil }}
		}, status: http.StatusServiceUnavailable, reason: reasonAttachNoRunner},
	}
	for _, row := range rows {
		t.Run(row.row, func(t *testing.T) {
			// "local, flag off" pins the C1 order: with local attach disabled a refused attach still
			// answers its own reason, never attach_local_unavailable / flag_off.
			for _, transport := range []string{"relay", "local", "local, flag off"} {
				o := localRigOpts{}
				if row.opts != nil {
					o = row.opts(t)
				}
				if transport == "local, flag off" {
					prev := o.cfg
					o.cfg = func(c *Config) {
						if prev != nil {
							prev(c)
						}
						c.AttachLocal = false
					}
				}
				rig := newLocalRig(t, o)
				if row.prepare != nil {
					row.prepare(t, rig)
				}
				principal, role := holderOwner, oidc.RoleUser
				if row.principal != "" {
					principal, role = row.principal, row.role
				}
				tk := row.ticket
				if tk == "" {
					tk = rig.ticket(t, principal, role)
				}
				query := ""
				if transport != "relay" {
					query = localQuery
				}
				w := do(t, rig.srv, http.MethodGet, "/api/v1/runs/"+rig.run.ID.String()+"/attach?ticket="+tk+query, "", "")
				if w.Code != row.status || reasonOf(t, w) != row.reason {
					t.Errorf("%s: %d %s, want %d %s", transport, w.Code, w.Body.String(), row.status, row.reason)
				}
				if strings.Contains(w.Body.String(), localAttachFlagOff) {
					t.Errorf("%s: answered %s, not its own reason", transport, localAttachFlagOff)
				}
				if n := rig.sub.calls(); n != 0 {
					t.Errorf("%s: %d local_attach calls for a refused attach", transport, n)
				}
			}
		})
	}
}

// TestAttachLocalUnavailableBeforeTheUpgrade: once every attach check has passed, a local attempt the
// local path cannot serve answers 409 attach_local_unavailable with its detail, before the upgrade, with
// no authz.denied row (a transport's answer) and no call to the runner. Break-glass, a sponsor and an
// organisation's runner are never local (C4).
func TestAttachLocalUnavailableBeforeTheUpgrade(t *testing.T) {
	cases := []struct {
		name   string
		opts   localRigOpts
		sub    func(*fakeLocalSub)
		bearer bool   // the admin token lane, no ticket (break-glass)
		role   string // the ticket's role (default member)
		detail string
	}{
		{name: "flag off", opts: localRigOpts{cfg: func(c *Config) { c.AttachLocal = false }}, detail: localAttachFlagOff},
		{name: "organisation's runner", opts: localRigOpts{rn: func(r *types.Runner) { r.OwnerKind, r.Owner = types.RunnerOwnerOrg, "" }}, detail: localAttachNotYourRunner},
		{name: "another person's runner", opts: localRigOpts{rn: func(r *types.Runner) { r.Owner = holderSecond }}, detail: localAttachNotYourRunner},
		{name: "unclaimed runner", opts: localRigOpts{rn: func(r *types.Runner) { r.State = types.RunnerUnclaimed }}, detail: localAttachNotYourRunner},
		{name: "remote placement", opts: localRigOpts{run: func(r *types.AgentRun) { r.Placement = types.PlacementRemote }}, detail: localAttachNotYourRunner},
		{name: "operator-owned run, admin's ticket", opts: localRigOpts{run: func(r *types.AgentRun) { r.OperatorOwned = true }}, role: oidc.RoleAdmin, detail: localAttachNotYourRunner},
		{name: "break-glass admin token", opts: localRigOpts{run: func(r *types.AgentRun) { r.OperatorOwned = true }}, bearer: true, detail: localAttachNotYourRunner},
		{name: "old runner", sub: func(f *fakeLocalSub) { f.caps.LocalAttach = false }, detail: localAttachRunnerTooOld},
		{name: "runner never reported", sub: func(f *fakeLocalSub) { f.reported = false }, detail: localAttachRunnerOffline},
		{name: "runner offline here", opts: localRigOpts{cfg: func(c *Config) { c.RunnerOnline = func(uuid.UUID) bool { return false } }}, detail: localAttachRunnerOffline},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rig := newLocalRig(t, c.opts)
			if c.sub != nil {
				c.sub(rig.sub)
			}
			path := "/api/v1/runs/" + rig.run.ID.String() + "/attach?"
			bearer := ""
			if c.bearer {
				bearer, path = adminToken, path+strings.TrimPrefix(localQuery, "&")
			} else {
				role := oidc.RoleUser
				if c.role != "" {
					role = c.role
				}
				path += "ticket=" + rig.ticket(t, holderOwner, role) + localQuery
			}
			w := do(t, rig.srv, http.MethodGet, path, bearer, "")
			var body errorBody
			_ = json.Unmarshal(w.Body.Bytes(), &body)
			if w.Code != http.StatusConflict || body.Reason != reasonAttachLocalUnavailable || body.Detail != c.detail {
				t.Fatalf("= %d %s, want 409 %s detail %s", w.Code, w.Body.String(), reasonAttachLocalUnavailable, c.detail)
			}
			if n := rig.sub.calls(); n != 0 {
				t.Errorf("%d local_attach calls", n)
			}
			for _, ev := range rig.audit.snapshot() {
				if ev.Action == "authz.denied" || strings.HasPrefix(ev.Action, "session.") {
					t.Errorf("a transport answer wrote %s %s", ev.Action, ev.Data)
				}
			}
		})
	}
}
