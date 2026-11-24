// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"
	"golang.org/x/crypto/ssh"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// GET /runs/{id}/ssh (sshgateway_ticket.go), through the real router and a served gateway: a coder/websocket client
// carries an x/crypto SSH client over NetConn.

// sshTicketStore is the SSH harness's store with attach tickets, and knobs to fail a ticket redemption or a run read.
type sshTicketStore struct {
	*sshMemStore
	tickets    *memTicketStore
	consumeErr atomic.Bool
	getRunErr  atomic.Bool
}

func (s *sshTicketStore) MintAttachTicket(ctx context.Context, token string, t store.AttachTicket, issued, expires time.Time) error {
	return s.tickets.MintAttachTicket(ctx, token, t, issued, expires)
}

func (s *sshTicketStore) ConsumeAttachTicket(ctx context.Context, token string, now time.Time) (store.AttachTicket, bool, error) {
	if s.consumeErr.Load() {
		return store.AttachTicket{}, false, errors.New("ticket store unavailable")
	}
	return s.tickets.ConsumeAttachTicket(ctx, token, now)
}

func (s *sshTicketStore) GetRun(ctx context.Context, id uuid.UUID) (types.AgentRun, error) {
	if s.getRunErr.Load() {
		return types.AgentRun{}, errors.New("run store unavailable")
	}
	return s.sshMemStore.GetRun(ctx, id)
}

type ticketDoorRig struct {
	h         *sshTestHarness
	st        *sshTicketStore
	rev       *attachRevocations
	run       types.AgentRun
	principal string
	web       *httptest.Server
}

func newTicketDoorRig(t *testing.T, configure ...func(*Config)) *ticketDoorRig {
	t.Helper()
	mem, run, principal := sshOwnedRunningRun(t)
	st := &sshTicketStore{sshMemStore: mem, tickets: newMemTicketStore()}
	rev := newAttachRevocations()
	fr := &sshFakeRunner{
		execFn:   func(runner.ExecSpec) (*runner.ExecSession, error) { return fakeExecSession("ok", "", 0), nil },
		attachFn: func() (runner.Session, error) { return newFakeShellSession(), nil },
	}
	base := func(c *Config) { c.SSHTickets, c.SessionRevocations = true, rev }
	h := newSSHTestHarness(t, st, fr, append([]func(*Config){base}, configure...)...)
	waitFor(t, "the gateway to serve", func() bool { return h.srv.metrics.sshLimits.Load() != nil })
	web := httptest.NewServer(h.srv.Handler())
	t.Cleanup(web.Close)
	return &ticketDoorRig{h: h, st: st, rev: rev, run: run, principal: principal, web: web}
}

// ticketAs stores a ticket on runID minted by principal with role, for purpose and ports.
func (rig *ticketDoorRig) ticketAs(t *testing.T, runID uuid.UUID, principal, role, purpose string, ports []int) string {
	t.Helper()
	tok, err := storeAttachTicket(context.Background(), rig.st, store.AttachTicket{RunID: runID, ActorType: types.ActorHuman,
		Principal: principal, Role: role, Purpose: purpose, Ports: ports}, oidc.Issue{At: time.Now(), Seq: rig.rev.current()})
	if err != nil {
		t.Fatalf("store a ticket: %v", err)
	}
	return tok
}

func (rig *ticketDoorRig) ticket(t *testing.T, purpose string, ports ...int) string {
	return rig.ticketAs(t, rig.run.ID, rig.principal, oidc.RoleUser, purpose, ports)
}

func (rig *ticketDoorRig) live(tok string) bool {
	rig.st.tickets.mu.Lock()
	defer rig.st.tickets.mu.Unlock()
	_, ok := rig.st.tickets.m[tok]
	return ok
}

func (rig *ticketDoorRig) doorURL(runID uuid.UUID) string {
	return rig.web.URL + "/api/v1/runs/" + runID.String() + "/ssh"
}

// get is a plain (non-upgrade) GET of the door with tok in the header ("" sends none) and query appended.
func (rig *ticketDoorRig) get(t *testing.T, runID uuid.UUID, tok, query string) (int, string, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, rig.doorURL(runID)+query, nil)
	if err != nil {
		t.Fatal(err)
	}
	if tok != "" {
		req.Header.Set("Wardyn-Ticket", tok)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var b errorBody
	_ = json.NewDecoder(resp.Body).Decode(&b)
	return resp.StatusCode, b.Reason, b.Error
}

// dial opens the WebSocket with tok in the header and an SSH client over it as user, pinning the gateway's key.
func (rig *ticketDoorRig) dial(t *testing.T, tok, user string) (*ssh.Client, error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	hdr := http.Header{}
	hdr.Set("Wardyn-Ticket", tok)
	dctx, dcancel := context.WithTimeout(ctx, 5*time.Second)
	defer dcancel()
	ws, _, err := websocket.Dial(dctx, "ws"+strings.TrimPrefix(rig.doorURL(rig.run.ID), "http"), &websocket.DialOptions{HTTPHeader: hdr})
	if err != nil {
		return nil, err
	}
	nc := websocket.NetConn(ctx, ws, websocket.MessageBinary)
	c, chans, reqs, err := ssh.NewClientConn(nc, "wardyn", &ssh.ClientConfig{User: user, HostKeyCallback: ssh.FixedHostKey(rig.h.hostPub), Timeout: 3 * time.Second})
	if err != nil {
		_ = nc.Close()
		return nil, err
	}
	client := ssh.NewClient(c, chans, reqs)
	t.Cleanup(func() { _ = client.Close() })
	return client, nil
}

// slotsFree waits for the gateway's handshake and source slots to all be given back.
func (rig *ticketDoorRig) slotsFree(t *testing.T) {
	t.Helper()
	lim := rig.h.srv.metrics.sshLimits.Load()
	waitFor(t, "the handshake slots to be given back", func() bool {
		lim.sourcesMu.Lock()
		defer lim.sourcesMu.Unlock()
		return len(lim.handshakes) == 0 && len(lim.sources) == 0
	})
}

// rows is the data of every event of action and outcome, with its actor and target under "_actor" and "_target".
func (rig *ticketDoorRig) rows(action, outcome string) []map[string]any {
	var out []map[string]any
	for _, ev := range rig.h.audit.snapshot() {
		if ev.Action != action || ev.Outcome != outcome {
			continue
		}
		data := map[string]any{}
		_ = json.Unmarshal(ev.Data, &data)
		data["_actor"], data["_target"], data["_source"] = ev.Actor, ev.Target, ev.SourceIP
		out = append(out, data)
	}
	return out
}

// waitRow waits for an event of action and outcome whose data reason is reason.
func (rig *ticketDoorRig) waitRow(t *testing.T, action, outcome, reason string) map[string]any {
	t.Helper()
	var got map[string]any
	waitFor(t, action+" "+reason, func() bool {
		for _, r := range rig.rows(action, outcome) {
			if r["reason"] == reason {
				got = r
				return true
			}
		}
		return false
	})
	return got
}

// TestSSHTicketDoor_Refusals: value rows D1-D13. Each refusal's status, reason and ssh.authenticate row, whether the
// ticket was spent, and that every one gave the handshake and source slots back.
func TestSSHTicketDoor_Refusals(t *testing.T) {
	const invalidRow = "invalid, expired, or already-used SSH ticket"
	profile := types.GovernanceUnit{ID: uuid.New(), Name: "no-shell", Limits: types.GovernanceLimits{DenyInteractive: true}}
	for _, tc := range []struct {
		name      string
		configure func(*Config)
		prep      func(*testing.T, *ticketDoorRig) (runID uuid.UUID, tok string)
		status    int
		reason    string
		rowActor  string // "" = no row
		rowReason string
		burned    bool
	}{
		{name: "tickets off", configure: func(c *Config) { c.SSHTickets = false },
			status: 409, reason: reasonSSHTicketsOff},
		{name: "gateway off", prep: func(t *testing.T, rig *ticketDoorRig) (uuid.UUID, string) {
			rig.h.srv.sshUnbound.Store(true)
			return rig.run.ID, rig.ticket(t, ticketPurposeSSHExec)
		}, status: 409, reason: reasonSSHGatewayOff},
		{name: "header absent", prep: func(*testing.T, *ticketDoorRig) (uuid.UUID, string) { return uuid.Nil, "" },
			status: 403, reason: reasonUIGatewayTicketInvalid, rowActor: "unknown", rowReason: invalidRow},
		{name: "unknown ticket", prep: func(_ *testing.T, rig *ticketDoorRig) (uuid.UUID, string) { return rig.run.ID, "not-a-ticket" },
			status: 403, reason: reasonUIGatewayTicketInvalid, rowActor: "unknown", rowReason: invalidRow},
		{name: "another run's ticket", prep: func(t *testing.T, rig *ticketDoorRig) (uuid.UUID, string) {
			other := types.AgentRun{ID: uuid.New(), CreatedBy: rig.principal, State: types.RunRunning, SandboxRef: "sbx-2"}
			rig.st.putRun(other)
			return rig.run.ID, rig.ticketAs(t, other.ID, rig.principal, oidc.RoleUser, ticketPurposeSSHExec, nil)
		}, status: 403, reason: reasonUIGatewayTicketInvalid, rowActor: "unknown", rowReason: invalidRow, burned: true},
		{name: "a terminal ticket", prep: func(t *testing.T, rig *ticketDoorRig) (uuid.UUID, string) { return rig.run.ID, rig.ticket(t, "") },
			status: 403, reason: reasonUIGatewayTicketInvalid, rowActor: "alice@example.com", rowReason: string(verdictPurposeMismatch), burned: true},
		{name: "revoked", prep: func(t *testing.T, rig *ticketDoorRig) (uuid.UUID, string) {
			tok := rig.ticket(t, ticketPurposeSSHExec)
			if err := rig.rev.RevokeSub(context.Background(), rig.principal); err != nil {
				t.Fatal(err)
			}
			return rig.run.ID, tok
		}, status: 403, reason: reasonUIGatewayTicketInvalid, rowActor: "alice@example.com", rowReason: string(verdictRevoked), burned: true},
		{name: "revocation unreadable", prep: func(t *testing.T, rig *ticketDoorRig) (uuid.UUID, string) {
			rig.rev.mu.Lock()
			rig.rev.failNext = 8
			rig.rev.mu.Unlock()
			return rig.run.ID, rig.ticket(t, ticketPurposeSSHExec)
		}, status: 503, reason: string(verdictRevocationUnavailable), rowActor: "alice@example.com", rowReason: string(verdictRevocationUnavailable), burned: true},
		{name: "ticket store fails", prep: func(t *testing.T, rig *ticketDoorRig) (uuid.UUID, string) {
			tok := rig.ticket(t, ticketPurposeSSHExec)
			rig.st.consumeErr.Store(true)
			return rig.run.ID, tok
		}, status: 500, reason: reasonUIGatewayTicketLookupFailed},
		{name: "run gone", prep: func(t *testing.T, rig *ticketDoorRig) (uuid.UUID, string) {
			gone := uuid.New()
			return gone, rig.ticketAs(t, gone, rig.principal, oidc.RoleUser, ticketPurposeSSHExec, nil)
		}, status: 403, reason: reasonUIGatewayTicketInvalid, rowActor: "alice@example.com", rowReason: "unknown run", burned: true},
		{name: "run unreadable", prep: func(t *testing.T, rig *ticketDoorRig) (uuid.UUID, string) {
			tok := rig.ticket(t, ticketPurposeSSHExec)
			rig.st.getRunErr.Store(true)
			return rig.run.ID, tok
		}, status: 503, reason: reasonSSHTicketUnverifiable, rowActor: "alice@example.com", rowReason: "run lookup failed", burned: true},
		{name: "not the owner", prep: func(t *testing.T, rig *ticketDoorRig) (uuid.UUID, string) {
			return rig.run.ID, rig.ticketAs(t, rig.run.ID, "bob@example.com", oidc.RoleUser, ticketPurposeSSHExec, nil)
		}, status: 403, reason: runOwnerOnlyReason, rowActor: "bob@example.com", rowReason: runOwnerOnlyReason, burned: true},
		{name: "an admin on a service identity's run", prep: func(t *testing.T, rig *ticketDoorRig) (uuid.UUID, string) {
			svc := types.AgentRun{ID: uuid.New(), CreatedBy: types.ServicePrincipalPrefix + "bot", State: types.RunRunning, SandboxRef: "sbx-3", OperatorOwned: true}
			rig.st.putRun(svc)
			return svc.ID, rig.ticketAs(t, svc.ID, "root@example.com", oidc.RoleAdmin, ticketPurposeSSHExec, nil)
		}, status: 403, reason: runOwnerOnlyReason, rowActor: "root@example.com", rowReason: runOwnerOnlyReason, burned: true},
		{name: "background task", prep: func(t *testing.T, rig *ticketDoorRig) (uuid.UUID, string) {
			bg := rig.run
			bg.ID, bg.Experience = uuid.New(), types.ExperienceBackground
			rig.st.putRun(bg)
			return bg.ID, rig.ticketAs(t, bg.ID, rig.principal, oidc.RoleUser, ticketPurposeSSHExec, nil)
		}, status: 409, reason: reasonRunBackgroundOnly, rowActor: "alice@example.com", rowReason: reasonRunBackgroundOnly, burned: true},
		{name: "deny_interactive", prep: func(t *testing.T, rig *ticketDoorRig) (uuid.UUID, string) {
			rig.st.mu.Lock()
			rig.st.profiles = []types.GovernanceUnit{profile}
			rig.st.mu.Unlock()
			gov := rig.run
			gov.ID, gov.UnitID = uuid.New(), &profile.ID
			rig.st.putRun(gov)
			return gov.ID, rig.ticketAs(t, gov.ID, rig.principal, oidc.RoleUser, ticketPurposeSSHExec, nil)
		}, status: 403, reason: string(authz.ReasonGovernanceUnit), rowActor: "alice@example.com",
			rowReason: `governance profile "no-shell" denies interactive sessions`, burned: true},
		{name: "profile unreadable", prep: func(t *testing.T, rig *ticketDoorRig) (uuid.UUID, string) {
			rig.st.mu.Lock()
			rig.st.profilesErr = errors.New("profiles unavailable")
			rig.st.mu.Unlock()
			gov := rig.run
			gov.ID, gov.UnitID = uuid.New(), &profile.ID
			rig.st.putRun(gov)
			return gov.ID, rig.ticketAs(t, gov.ID, rig.principal, oidc.RoleUser, ticketPurposeSSHExec, nil)
		}, status: 503, reason: reasonSSHTicketUnverifiable, rowActor: "alice@example.com", rowReason: "governance profile unreadable", burned: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var configure []func(*Config)
			if tc.configure != nil {
				configure = append(configure, tc.configure)
			}
			rig := newTicketDoorRig(t, configure...)
			runID, tok := rig.run.ID, rig.ticket(t, ticketPurposeSSHExec)
			if tc.prep != nil {
				runID, tok = tc.prep(t, rig)
				if runID == uuid.Nil {
					runID = rig.run.ID
				}
			}
			status, reason, _ := rig.get(t, runID, tok, "")
			if status != tc.status || reason != tc.reason {
				t.Fatalf("GET /ssh = %d %q, want %d %q", status, reason, tc.status, tc.reason)
			}
			rows := rig.rows("ssh.authenticate", "failure")
			if tc.rowActor == "" {
				if len(rows) != 0 {
					t.Errorf("rows = %v, want none", rows)
				}
			} else if len(rows) != 1 || rows[0]["_actor"] != tc.rowActor || rows[0]["reason"] != tc.rowReason ||
				rows[0]["auth"] != sshAuthTicket || rows[0]["_target"] != sshAuthTicket || !strings.HasPrefix(rows[0]["_source"].(string), "127.0.0.1:") {
				t.Errorf("rows = %v, want one ticket row by %s for %q from the request's address", rows, tc.rowActor, tc.rowReason)
			}
			if tok != "" && tok != "not-a-ticket" {
				rig.st.consumeErr.Store(false)
				if burned := !rig.live(tok); burned != tc.burned {
					t.Errorf("ticket burned = %v, want %v", burned, tc.burned)
				}
			}
			rig.slotsFree(t)
		})
	}
}

// TestSSHTicketDoor_GatewayBounds: value rows D3 and D4. A replica whose gateway is not serving yet, and a gateway
// whose handshake bound is full, answer 503 before any ticket work.
func TestSSHTicketDoor_GatewayBounds(t *testing.T) {
	t.Run("not serving", func(t *testing.T) {
		rig := newTicketDoorRig(t)
		lim := rig.h.srv.metrics.sshLimits.Swap(nil)
		defer rig.h.srv.metrics.sshLimits.Store(lim)
		tok := rig.ticket(t, ticketPurposeSSHExec)
		if status, reason, _ := rig.get(t, rig.run.ID, tok, ""); status != 503 || reason != reasonSSHGatewayUnavailable || !rig.live(tok) {
			t.Fatalf("GET /ssh = %d %q (ticket live %v), want 503 ssh_gateway_unavailable and the ticket unspent", status, reason, rig.live(tok))
		}
	})
	t.Run("handshakes full", func(t *testing.T) {
		rig := newTicketDoorRig(t)
		lim := rig.h.srv.metrics.sshLimits.Load()
		for range cap(lim.handshakes) {
			lim.handshakes <- struct{}{}
		}
		tok := rig.ticket(t, ticketPurposeSSHExec)
		status, reason, _ := rig.get(t, rig.run.ID, tok, "")
		for range cap(lim.handshakes) {
			<-lim.handshakes
		}
		if status != 503 || reason != reasonSSHGatewayBusy || !rig.live(tok) {
			t.Fatalf("GET /ssh = %d %q (ticket live %v), want 503 ssh_gateway_busy and the ticket unspent", status, reason, rig.live(tok))
		}
		rig.slotsFree(t)
	})
}

// TestSSHTicketDoor_SlotReleasedAfterFailedAccept: a valid ticket spent on a plain GET passes every check, then
// websocket.Accept refuses it; the ticket is spent and the handshake and source slots are given back.
func TestSSHTicketDoor_SlotReleasedAfterFailedAccept(t *testing.T) {
	rig := newTicketDoorRig(t)
	tok := rig.ticket(t, ticketPurposeSSHExec)
	if status, _, _ := rig.get(t, rig.run.ID, tok, ""); status < 400 {
		t.Fatalf("a plain GET = %d, want Accept's refusal", status)
	}
	if rig.live(tok) {
		t.Error("the ticket survived")
	}
	rig.slotsFree(t)
}

// TestSSHTicketDoor_AttachForwardTicketIsNotAnSSHTicket: value row F5's door half.
func TestSSHTicketDoor_AttachForwardTicketIsNotAnSSHTicket(t *testing.T) {
	rig := newTicketDoorRig(t)
	tok := rig.ticket(t, ticketPurposeAttachForward, 3000)
	if status, reason, _ := rig.get(t, rig.run.ID, tok, ""); status != 403 || reason != reasonUIGatewayTicketInvalid {
		t.Fatalf("GET /ssh = %d %q, want the bad-ticket 403", status, reason)
	}
	rows := rig.rows("ssh.authenticate", "failure")
	if len(rows) != 1 || rows[0]["reason"] != string(verdictPurposeMismatch) || rows[0]["purpose"] != ticketPurposeAttachForward || rig.live(tok) {
		t.Fatalf("rows = %v (ticket live %v), want one ticket_purpose_mismatch row and the ticket spent", rows, rig.live(tok))
	}
}

// TestSSHTicketDoor_IgnoresQueryTicket: the door reads its ticket from the header only, so one in the URL is no
// ticket and stays unspent.
func TestSSHTicketDoor_IgnoresQueryTicket(t *testing.T) {
	rig := newTicketDoorRig(t)
	tok := rig.ticket(t, ticketPurposeSSHExec)
	if status, reason, _ := rig.get(t, rig.run.ID, "", "?ticket="+tok); status != 403 || reason != reasonUIGatewayTicketInvalid {
		t.Fatalf("GET /ssh?ticket= = %d %q, want the bad-ticket 403", status, reason)
	}
	if !rig.live(tok) {
		t.Fatal("a ticket in the URL was read")
	}
}

// TestSSHTicketDoor_TCPConfigStaysPublicKeyOnly: the TCP listener's configuration offers public key only.
func TestSSHTicketDoor_TCPConfigStaysPublicKeyOnly(t *testing.T) {
	rig := newTicketDoorRig(t)
	signer, err := ssh.NewSignerFromKey(rig.h.srv.cfg.SSHHostKey)
	if err != nil {
		t.Fatal(err)
	}
	cfg := rig.h.srv.sshServerConfig(signer)
	if cfg.NoClientAuth || cfg.NoClientAuthCallback != nil || cfg.PublicKeyCallback == nil {
		t.Fatalf("TCP config: NoClientAuth %v, NoClientAuthCallback set %v, PublicKeyCallback set %v; want public key only",
			cfg.NoClientAuth, cfg.NoClientAuthCallback != nil, cfg.PublicKeyCallback != nil)
	}
}

// TestSSHTicketDoor_Admits: the admitted row and D14. The username must be the ticket's run id; the success row
// names the purpose, the connection counts against its person's share of the gateway, and the TCP key path's
// success row carries no auth.
func TestSSHTicketDoor_Admits(t *testing.T) {
	t.Run("username is another id", func(t *testing.T) {
		rig := newTicketDoorRig(t)
		if _, err := rig.dial(t, rig.ticket(t, ticketPurposeSSHExec), uuid.NewString()); err == nil {
			t.Fatal("an SSH username that is not the run id signed in")
		}
		row := rig.waitRow(t, "ssh.authenticate", "failure", "invalid username (want the ticket's run id)")
		if row["_actor"] != rig.principal || row["purpose"] != ticketPurposeSSHExec {
			t.Errorf("row = %v, want the ticket's principal and purpose", row)
		}
		if n := len(rig.rows("ssh.authenticate", "success")); n != 0 {
			t.Errorf("success rows = %d, want none", n)
		}
	})
	t.Run("admitted", func(t *testing.T) {
		rig := newTicketDoorRig(t)
		client, err := rig.dial(t, rig.ticket(t, ticketPurposeSSHForward, 3000, 5000), rig.run.ID.String())
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		var row map[string]any
		waitFor(t, "the success row", func() bool {
			rows := rig.rows("ssh.authenticate", "success")
			if len(rows) == 1 {
				row = rows[0]
			}
			return row != nil
		})
		if row["auth"] != sshAuthTicket || row["purpose"] != ticketPurposeSSHForward || row["_target"] != sshAuthTicket || row["_actor"] != rig.principal {
			t.Errorf("success row = %v, want auth ticket, purpose ssh-forward, target ticket, by the principal", row)
		}
		if ports, _ := row["ports"].([]any); len(ports) != 2 {
			t.Errorf("success row ports = %v, want [3000 5000]", row["ports"])
		}
		lim := rig.h.srv.metrics.sshLimits.Load()
		lim.principalsMu.Lock()
		n := lim.principals[rig.principal]
		lim.principalsMu.Unlock()
		if n != 1 {
			t.Errorf("the person's established connections = %d, want 1", n)
		}
		_ = client.Close()
	})
	t.Run("a key connection carries no auth", func(t *testing.T) {
		rig := newTicketDoorRig(t)
		priv, pub := mustSSHKeypair(t)
		rig.st.putKey(types.SSHPublicKey{Fingerprint: ssh.FingerprintSHA256(pub), Principal: rig.principal, PublicKey: string(ssh.MarshalAuthorizedKey(pub))})
		client, err := sshDial(t, rig.h, rig.run.ID.String(), priv)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer client.Close()
		ev := waitForAudit(t, rig.h.audit, rig.run.ID, "ssh.authenticate", "success")
		if ev == nil || strings.Contains(string(ev.Data), `"auth"`) || ev.Target == sshAuthTicket {
			t.Fatalf("key success row = %v, want no auth and the key's target", ev)
		}
	})
}

// TestSSHTicketDoor_KeyPathRefusals pins sshAuth's own run checks beside the door's: a key on a background task is
// refused with run_background_only, and a member's key on a service identity's run with run_owner_only.
func TestSSHTicketDoor_KeyPathRefusals(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*types.AgentRun)
		reason string
	}{
		{"background task", func(r *types.AgentRun) { r.Experience = types.ExperienceBackground }, reasonRunBackgroundOnly},
		{"service identity's run", func(r *types.AgentRun) { r.CreatedBy = types.ServicePrincipalPrefix + "bot" }, runOwnerOnlyReason},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newTicketDoorRig(t)
			run := rig.run
			run.ID = uuid.New()
			tc.mutate(&run)
			rig.st.putRun(run)
			priv, pub := mustSSHKeypair(t)
			rig.st.putKey(types.SSHPublicKey{Fingerprint: ssh.FingerprintSHA256(pub), Principal: rig.principal, PublicKey: string(ssh.MarshalAuthorizedKey(pub))})
			if client, err := sshDial(t, rig.h, run.ID.String(), priv); err == nil {
				_ = client.Close()
				t.Fatal("the key signed in")
			}
			if row := rig.waitRow(t, "ssh.authenticate", "failure", tc.reason); row["auth"] != nil {
				t.Errorf("key row = %v, want no auth", row)
			}
		})
	}
}

// execRuns runs cmd on a new session channel.
func execRuns(t *testing.T, client *ssh.Client, cmd string) error {
	t.Helper()
	sess, err := client.NewSession()
	if err != nil {
		return err
	}
	defer sess.Close()
	return sess.Run(cmd)
}

// TestSSHTicketDoor_ChannelPurpose: value rows 23-27. A ticket opens only its purpose's kinds; ssh-exec runs one
// command; ssh-forward reaches only its ports.
func TestSSHTicketDoor_ChannelPurpose(t *testing.T) {
	t.Run("ssh-exec", func(t *testing.T) {
		rig := newTicketDoorRig(t)
		client, err := rig.dial(t, rig.ticket(t, ticketPurposeSSHExec), rig.run.ID.String())
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		if err := execRuns(t, client, "true"); err != nil {
			t.Fatalf("the first command: %v", err)
		}
		if err := execRuns(t, client, "true"); err == nil {
			t.Fatal("a second command ran")
		}
		rig.waitRow(t, "ssh.channel.reject", "failure", "ticket_exec_spent")
		sess, err := client.NewSession()
		if err != nil {
			t.Fatal(err)
		}
		if err := sess.Shell(); err == nil {
			t.Error("an ssh-exec ticket opened a terminal")
		}
		_ = sess.Close()
		if row := rig.waitRow(t, "ssh.channel.reject", "failure", "ticket_purpose"); row["purpose"] != ticketPurposeSSHExec || row["_target"] != rig.run.ID.String() {
			t.Errorf("row = %v, want purpose ssh-exec on the run", row)
		}
		if _, err := client.Dial("tcp", "127.0.0.1:3000"); err == nil {
			t.Error("an ssh-exec ticket opened a forward")
		}
	})
	t.Run("ssh-forward", func(t *testing.T) {
		rig := newTicketDoorRig(t)
		client, err := rig.dial(t, rig.ticket(t, ticketPurposeSSHForward, 3000), rig.run.ID.String())
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		if _, err := client.Dial("tcp", "127.0.0.1:5000"); err == nil || !strings.Contains(err.Error(), "forwards only port(s) 3000") {
			t.Errorf("a forward to an unminted port = %v, want the ticket's sentence", err)
		}
		if row := rig.waitRow(t, "ssh.channel.reject", "failure", "ticket_port"); row["port"] != float64(5000) {
			t.Errorf("row = %v, want port 5000", row)
		}
		if err := execRuns(t, client, "true"); err == nil {
			t.Error("an ssh-forward ticket ran a command")
		}
		rig.waitRow(t, "ssh.channel.reject", "failure", "ticket_purpose")
	})
}

// TestSSHTicketDoor_SwitchAndDataOutFirst: value rows 28 and 29. WARDYN_SSH_CHANNELS refuses before the purpose,
// with its own row, and data_out applies to a ticket connection as to a key's.
func TestSSHTicketDoor_SwitchAndDataOutFirst(t *testing.T) {
	t.Run("switch", func(t *testing.T) {
		rig := newTicketDoorRig(t, func(c *Config) { c.SSHChannels = map[string]bool{"shell": true, "sftp": true, "forward": true} })
		client, err := rig.dial(t, rig.ticket(t, ticketPurposeSSHExec), rig.run.ID.String())
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		if err := execRuns(t, client, "true"); err == nil {
			t.Fatal("a command ran with exec switched off")
		}
		rig.waitRow(t, "ssh.channel.reject", "failure", "channel_off")
		for _, r := range rig.rows("ssh.channel.reject", "failure") {
			if r["reason"] == "ticket_purpose" || r["reason"] == "ticket_exec_spent" {
				t.Errorf("row = %v, want the switch's row only", r)
			}
		}
	})
	t.Run("data_out", func(t *testing.T) {
		rig := newTicketDoorRig(t)
		rig.st.mu.Lock()
		rig.st.policy = types.RunPolicySpec{DataOut: types.TransferBlocked}
		rig.st.mu.Unlock()
		client, err := rig.dial(t, rig.ticket(t, ticketPurposeSSHExec), rig.run.ID.String())
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		if err := execRuns(t, client, "true"); err == nil {
			t.Fatal("a command ran under a blocked data_out rule")
		}
		waitFor(t, "the data_out row", func() bool { return len(rig.rows("run.data_out.deny", "denied")) > 0 })
	})
}

// TestSSHTicketDoor_Recheck: value row 30 and the re-check. A revoked sign-in refuses the next channel with the
// ticket's sentence and closes an idle connection; deny_interactive set after the connection opened closes it too.
func TestSSHTicketDoor_Recheck(t *testing.T) {
	prev := setSSHKeepaliveEvery(100 * time.Millisecond)
	t.Cleanup(func() { setSSHKeepaliveEvery(prev) })
	t.Run("revoked", func(t *testing.T) {
		rig := newTicketDoorRig(t)
		client, err := rig.dial(t, rig.ticket(t, ticketPurposeSSHEditor), rig.run.ID.String())
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		if err := rig.rev.RevokeSub(context.Background(), rig.principal); err != nil {
			t.Fatal(err)
		}
		if _, err := client.NewSession(); err == nil {
			// The keepalive may have closed the connection first; either way no channel opens.
			t.Error("a channel opened after the sign-in was revoked")
		}
		done := make(chan error, 1)
		go func() { done <- client.Wait() }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("the idle connection stayed open")
		}
	})
	t.Run("deny_interactive later", func(t *testing.T) {
		rig := newTicketDoorRig(t)
		profile := types.GovernanceUnit{ID: uuid.New(), Name: "later", Limits: types.GovernanceLimits{}}
		rig.st.mu.Lock()
		rig.st.profiles = []types.GovernanceUnit{profile}
		rig.st.mu.Unlock()
		run := rig.run
		run.UnitID = &profile.ID
		rig.st.putRun(run)
		client, err := rig.dial(t, rig.ticket(t, ticketPurposeSSHEditor), rig.run.ID.String())
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		profile.Limits.DenyInteractive = true
		rig.st.mu.Lock()
		rig.st.profiles = []types.GovernanceUnit{profile}
		rig.st.mu.Unlock()
		done := make(chan error, 1)
		go func() { done <- client.Wait() }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("the connection stayed open under deny_interactive")
		}
	})
	t.Run("the channel's sentence", func(t *testing.T) {
		if got := sshRejectMessage(sshTicketEnded); got != "the sign-in that opened this SSH connection is no longer allowed on this run" {
			t.Errorf("sshRejectMessage(ended) = %q", got)
		}
		if sshRecheckUnread(&ssh.Permissions{Extensions: map[string]string{sshExtAuth: sshAuthTicket}}, sshTicketUnverified) {
			t.Error("an unverifiable ticket re-check rides the session budget; want it to close")
		}
	})
}

// TestSSHTicketDoor_RecheckBounds: sshCurrentAuth's ticket branch. A check that blocks past sshAuthTimeout, or
// fails, ends the connection; a ticket connection whose context lost its entry cannot be verified.
func TestSSHTicketDoor_RecheckBounds(t *testing.T) {
	srv := New(Config{})
	perms := &ssh.Permissions{Extensions: map[string]string{sshExtAuth: sshAuthTicket}}
	if got := srv.sshCurrentAuth(context.Background(), perms); got != sshTicketUnverified {
		t.Errorf("no ticket entry = %q, want %q", got, sshTicketUnverified)
	}
	blocks := &sshTicketConn{check: func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }}
	ctx := context.WithValue(context.Background(), sshTicketConnKey{}, blocks)
	start := time.Now()
	if got := srv.sshCurrentAuth(ctx, perms); got != sshTicketEnded || time.Since(start) > 2*sshAuthTimeout {
		t.Errorf("a blocked check = %q after %v, want %q within the bound", got, time.Since(start), sshTicketEnded)
	}
	holds := &sshTicketConn{check: func(context.Context) error { return nil }}
	if got := srv.sshCurrentAuth(context.WithValue(context.Background(), sshTicketConnKey{}, holds), perms); got != "" {
		t.Errorf("a check that holds = %q, want none", got)
	}
}

// TestSSHTicketDoor_Healthz: /healthz's ssh object names tickets and the public host key only while tickets are on,
// and the key is the one host_key_fingerprint names.
func TestSSHTicketDoor_Healthz(t *testing.T) {
	rig := newTicketDoorRig(t)
	out := rig.h.srv.sshGatewayHealthz()
	key, _, _, _, err := ssh.ParseAuthorizedKey([]byte(out["host_key"].(string)))
	if err != nil || out["tickets"] != true || ssh.FingerprintSHA256(key) != out["host_key_fingerprint"] {
		t.Fatalf("healthz ssh = %v (%v), want tickets and the host key host_key_fingerprint names", out, err)
	}
	off := newTicketDoorRig(t, func(c *Config) { c.SSHTickets = false }).h.srv.sshGatewayHealthz()
	if _, ok := off["tickets"]; ok {
		t.Errorf("healthz ssh with tickets off = %v, want no tickets", off)
	}
	if _, ok := off["host_key"]; ok {
		t.Errorf("healthz ssh with tickets off = %v, want no host_key", off)
	}
}
