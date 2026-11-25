// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/coder/websocket"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"golang.org/x/crypto/ssh"

	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// GET /runs/{id}/ssh: SSH over a WebSocket, opened by a single-use SSH ticket (attach_ticket_purpose.go) in the
// Wardyn-Ticket header, for a person with no registered key. The connection is served by handleSSHConn exactly as a
// TCP one is, with the run checks the key path makes (sshRunEntryRefusal), a re-check at keepalive cadence
// (sshCurrentAuth) and the channel kinds and ports the ticket's purpose names. The TCP listener's configuration
// stays public key only: the `none` method exists only on the per-connection configuration built here, after the
// ticket is redeemed.

// The re-check reasons of a ticket connection. Neither is transient (sshReasonTransient), so either closes it.
const (
	sshTicketEnded      = "SSH ticket authority ended"
	sshTicketUnverified = "SSH ticket could not be verified"
)

// sshExtAuth is "ticket" on a connection the ticket door opened; a key's connection never carries it.
const (
	sshExtAuth    = "auth"
	sshAuthTicket = "ticket"
)

// verdictPurposeMismatch is redemption's refusal of a ticket minted for no SSH purpose: answered as a bad ticket, its
// row names it.
const verdictPurposeMismatch entryVerdict = "ticket_purpose_mismatch"

// sshTicketConn is what a ticket connection's channels and re-check read: the purpose and its ports, the authority
// check (nil when there is nothing to repeat) and whether an ssh-exec ticket's one command has run. It lives only on
// the context the door hands handleSSHConn.
type sshTicketConn struct {
	purpose  string
	ports    []int
	check    func(context.Context) error
	execUsed atomic.Bool
}

type sshTicketConnKey struct{}

func sshTicketOf(ctx context.Context) (*sshTicketConn, bool) {
	tc, ok := ctx.Value(sshTicketConnKey{}).(*sshTicketConn)
	return tc, ok
}

// mountSSHTicketDoor mounts GET /runs/{id}/ssh. No auth middleware: the ticket is the credential, read by the door.
func (s *Server) mountSSHTicketDoor(r chi.Router) {
	r.Get("/runs/{id}/ssh", s.handleSSHTicketDoor)
}

// handleSSHTicketDoor admits the connection under the gateway's handshake bounds before any ticket work, redeems
// the ticket and checks the run, then upgrades and serves SSH. Every return before the hand-off to handleSSHConn
// gives the handshake and source slots back.
func (s *Server) handleSSHTicketDoor(w http.ResponseWriter, r *http.Request) {
	id, ok := parseIDParam(w, r, "id", "run")
	if !ok {
		return
	}
	if !s.cfg.SSHTickets {
		writeErrorReason(w, http.StatusConflict, reasonSSHTicketsOff, "SSH tickets are off on this deployment (WARDYN_SSH_TICKETS).")
		return
	}
	signer, err := ssh.NewSignerFromKey(s.cfg.SSHHostKey)
	if s.sshGatewayHealthz() == nil || err != nil {
		writeErrorReason(w, http.StatusConflict, reasonSSHGatewayOff, "The SSH gateway is off on this deployment (WARDYN_SSH_LISTEN).")
		return
	}
	lim := s.metrics.sshLimits.Load()
	if lim == nil {
		writeErrorReason(w, http.StatusServiceUnavailable, reasonSSHGatewayUnavailable, "The SSH gateway is not serving on this replica yet; try again.")
		return
	}
	src := peerKey(r.RemoteAddr)
	if !sshAdmit(lim, src) {
		writeErrorReason(w, http.StatusServiceUnavailable, reasonSSHGatewayBusy, "Too many SSH connections are being opened; try again.")
		return
	}
	release := sync.OnceFunc(func() {
		<-lim.handshakes
		lim.leaveSource(src)
	})
	handedOff := false
	defer func() {
		if !handedOff {
			release()
		}
	}()
	tc, perms, success, ok := s.admitSSHTicket(w, r, id)
	if !ok {
		return
	}
	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{CompressionMode: websocket.CompressionDisabled})
	if err != nil {
		return // Accept wrote its own refusal; the ticket is spent
	}
	connCtx, stop := context.WithCancel(r.Context())
	defer stop()
	if lim.serveCtx != nil {
		defer context.AfterFunc(lim.serveCtx, stop)()
	}
	ctx := context.WithValue(connCtx, sshTicketConnKey{}, tc)
	nc := websocket.NetConn(ctx, ws, websocket.MessageBinary)
	cfg := s.sshTicketServerConfig(r, signer, id, perms, success)
	handedOff = true
	done := make(chan struct{})
	s.sshGoTracked(func() {
		defer close(done)
		s.handleSSHConn(ctx, nc, cfg, lim, src)
	})
	<-done
}

// admitSSHTicket is the door's checks, under one sshAuthTimeout bound: the ticket, the run, the owner rule (with
// the door's own service-identity refusal), the run checks the key path makes, and the authority the connection
// re-checks. It answers every refusal itself; ok=false means it did. success is the ssh.authenticate success row's
// data, written once the client's username names the run.
func (s *Server) admitSSHTicket(w http.ResponseWriter, r *http.Request, id uuid.UUID) (tc *sshTicketConn, perms *ssh.Permissions, success []byte, ok bool) {
	lctx, cancel := context.WithTimeout(r.Context(), sshAuthTimeout)
	defer cancel()
	ta, v, err := s.redeemSSHTicket(lctx, r.Header.Get("Wardyn-Ticket"), id)
	if err != nil {
		slog.ErrorContext(r.Context(), "wardynd: ssh ticket lookup failed", "run_id", id, "err", err)
		writeErrorReason(w, http.StatusInternalServerError, reasonUIGatewayTicketLookupFailed, "attach ticket lookup failed")
		return nil, nil, nil, false
	}
	if v != verdictOK {
		if v == verdictInvalid {
			s.sshAuditTicketFailure(r, id, "unknown", "", "invalid, expired, or already-used SSH ticket")
		} else {
			s.sshAuditTicketFailure(r, id, ta.principal, ta.purpose, string(v))
		}
		refuseTicketResponse(w, v)
		return nil, nil, nil, false
	}
	refuse := func(status int, reason, msg, row string) (*sshTicketConn, *ssh.Permissions, []byte, bool) {
		s.sshAuditTicketFailure(r, id, ta.principal, ta.purpose, row)
		writeErrorReason(w, status, reason, msg)
		return nil, nil, nil, false
	}
	run, err := s.cfg.Store.GetRun(lctx, id)
	if errors.Is(err, store.ErrNotFound) {
		s.sshAuditTicketFailure(r, id, ta.principal, ta.purpose, "unknown run")
		refuseTicketResponse(w, verdictInvalid)
		return nil, nil, nil, false
	}
	if err != nil {
		slog.ErrorContext(r.Context(), "wardynd: ssh ticket run lookup failed", "run_id", id, "err", err)
		return refuse(http.StatusServiceUnavailable, reasonSSHTicketUnverifiable, "SSH access could not be checked right now.", "run lookup failed")
	}
	superAdmin := ta.superAdmin()
	override := run.CreatedBy != ta.principal
	if _, isService := serviceOfRun(run); !mayEnterRun(run, ta.principal, superAdmin) || (override && isService) {
		return refuse(http.StatusForbidden, runOwnerOnlyReason, "You do not own this run.", runOwnerOnlyReason)
	}
	switch reason, profile := s.sshRunEntryRefusal(lctx, run, superAdmin && s.adminDoorExempt(run)); {
	case reason == reasonRunBackgroundOnly:
		return refuse(http.StatusConflict, reasonRunBackgroundOnly, "This run is a background task; it has no SSH access.", reason)
	case profile != "":
		return refuse(http.StatusForbidden, string(authz.ReasonGovernanceUnit), fmt.Sprintf(
			"SSH is not allowed under the governance profile %q this run was launched under: it denies interactive sessions.", profile), reason)
	case reason != "":
		return refuse(http.StatusServiceUnavailable, reasonSSHTicketUnverifiable, "SSH access could not be checked right now.", reason)
	}
	ext := map[string]string{"principal": ta.principal, "run_id": id.String(), sshExtAuth: sshAuthTicket, "purpose": ta.purpose}
	sshMarkEntry(ext, override, uuid.Nil, nil)
	tc = &sshTicketConn{purpose: ta.purpose, ports: ta.ports, check: s.attachAuthority(r.WithContext(withTicketActor(r.Context(), ta)), id)}
	return tc, &ssh.Permissions{Extensions: ext}, sshTicketSuccessData(ta, override), true
}

// redeemSSHTicket burns the ticket, then refuses one that is unknown, expired, another run's or from before the
// authority columns (invalid, no actor), one minted for no SSH purpose (verdictPurposeMismatch: an attach-forward
// ticket is not an SSH ticket), and one whose authority no longer holds (ticketAuthority). An absent header reads
// no ticket. It never asks the support arm: a helper gets no SSH ticket.
func (s *Server) redeemSSHTicket(ctx context.Context, tok string, runID uuid.UUID) (ticketActor, entryVerdict, error) {
	if tok == "" {
		return ticketActor{}, verdictInvalid, nil
	}
	ta, ok, err := consumeAttachTicket(ctx, s.cfg.Store, tok, runID, s.cfg.Now())
	if err != nil {
		return ticketActor{}, verdictOK, err
	}
	if !ok || ta.authorizedAt.IsZero() {
		return ticketActor{}, verdictInvalid, nil
	}
	if !sshTicketPurpose(ta.purpose) {
		return ta, verdictPurposeMismatch, nil
	}
	_, v := s.ticketAuthority(ctx, ta, runID)
	return ta, v, nil
}

// sshTicketSuccessData is the ssh.authenticate success row's data, carried to the auth callback: the purpose, the
// credential the ticket was minted under, a forward's ports and the override.
func sshTicketSuccessData(ta ticketActor, override bool) []byte {
	data := map[string]any{"auth": sshAuthTicket, "purpose": ta.purpose}
	if ta.tokenID != uuid.Nil {
		data["token_id"] = ta.tokenID.String()
	}
	if ta.cliSessionID != uuid.Nil {
		data["cli_session_id"] = ta.cliSessionID.String()
	}
	if len(ta.ports) > 0 {
		data["ports"] = ta.ports
	}
	if override {
		data["override"] = true
	}
	return mustJSON(data)
}

// sshTicketServerConfig is one ticket connection's configuration: the `none` method only, whose callback admits the
// username that names the ticket's run and writes the row either way. No public key callback.
func (s *Server) sshTicketServerConfig(r *http.Request, signer ssh.Signer, runID uuid.UUID, perms *ssh.Permissions, success []byte) *ssh.ServerConfig {
	reqCtx, principal, purpose := r.Context(), perms.Extensions["principal"], perms.Extensions["purpose"]
	cfg := &ssh.ServerConfig{
		NoClientAuth: true,
		NoClientAuthCallback: func(cm ssh.ConnMetadata) (*ssh.Permissions, error) {
			if id, err := uuid.Parse(cm.User()); err != nil || id != runID {
				s.sshAuditTicketFailure(r, runID, principal, purpose, "invalid username (want the ticket's run id)")
				return nil, errors.New("ssh: username must be the ticket's run id")
			}
			actx, cancel := context.WithTimeout(reqCtx, sshAuthTimeout)
			defer cancel()
			ev := s.auditEvent(&runID, types.ActorHuman, principal, "ssh.authenticate", sshAuthTicket, "success", success)
			ev.SourceIP = r.RemoteAddr
			s.recordAudit(actx, ev)
			return perms, nil
		},
		MaxAuthTries: sshMaxAuthTries,
	}
	cfg.AddHostKey(signer)
	return cfg
}

// sshAuditTicketFailure writes one refused ticket entry's ssh.authenticate row, target "ticket", under the request's
// own context and address (never the WebSocket's peer). Every one is charged to the shared auth-failure limiter: a
// named refusal needs a ticket that is now spent, so it has no key bucket to spend.
func (s *Server) sshAuditTicketFailure(r *http.Request, runID uuid.UUID, actor, purpose, reason string) {
	if !s.authFailedAllowed() {
		return
	}
	data := map[string]any{"reason": reason, "auth": sshAuthTicket}
	if purpose != "" {
		data["purpose"] = purpose
	}
	ctx, cancel := context.WithTimeout(r.Context(), sshAuthTimeout)
	defer cancel()
	ev := s.auditEvent(&runID, types.ActorHuman, actor, "ssh.authenticate", sshAuthTicket, "failure", mustJSON(data))
	ev.SourceIP = r.RemoteAddr
	s.recordAudit(ctx, ev)
}

// sshCurrentAuth is an open connection's re-check: a ticket connection's authority check, bounded as sshCurrentKey
// bounds its reads, else sshCurrentKey. A ticket connection whose context carries no ticket entry, a check that
// fails, times out or cannot read, all close the connection (strict, as a helper's re-check is).
func (s *Server) sshCurrentAuth(ctx context.Context, perms *ssh.Permissions) string {
	if perms.Extensions[sshExtAuth] != sshAuthTicket {
		return s.sshCurrentKey(ctx, perms)
	}
	tc, ok := sshTicketOf(ctx)
	if !ok {
		return sshTicketUnverified
	}
	if tc.check == nil {
		return ""
	}
	cctx, cancel := context.WithTimeout(ctx, sshAuthTimeout)
	defer cancel()
	if err := tc.check(cctx); err != nil {
		return sshTicketEnded
	}
	return ""
}

// sshConnTarget is a connection's row target: its key's fingerprint, or "ticket" for a ticket connection.
func sshConnTarget(perms *ssh.Permissions) string {
	if perms.Extensions[sshExtAuth] == sshAuthTicket {
		return sshAuthTicket
	}
	return perms.Extensions["key_fingerprint"]
}

// sshTicketKindRefusal is sshKindRefusal's third check, on a ticket connection only: a kind the purpose does not
// serve, or an ssh-exec ticket's second command, gets its sentence and one ssh.channel.reject row; "" when it may run.
func (s *Server) sshTicketKindRefusal(ctx context.Context, runID uuid.UUID, principal string, k sshChannelKind, channelType string) string {
	tc, ok := sshTicketOf(ctx)
	if !ok {
		return ""
	}
	if !slices.Contains(sshTicketKinds[tc.purpose], k) {
		s.sshAuditTicketChannel(ctx, runID, principal, map[string]any{"channel_type": channelType, "reason": "ticket_purpose", "purpose": tc.purpose})
		word := sshKindWord(k)
		if k == sshKindSync {
			word = "wardyn sync"
		}
		return "wardyn: this SSH ticket was issued for " + ticketPurposeWords[tc.purpose] + " and does not open " + word
	}
	if tc.purpose == ticketPurposeSSHExec && k == sshKindExec && !tc.execUsed.CompareAndSwap(false, true) {
		s.sshAuditTicketChannel(ctx, runID, principal, map[string]any{"channel_type": "exec", "reason": "ticket_exec_spent"})
		return "wardyn: this SSH ticket's one command has already run"
	}
	return ""
}

// sshTicketPortRefused rejects an ssh-forward ticket's direct-tcpip channel to a port the ticket was not minted for.
// A malformed request is left to handleSSHDirectTCPIP's own refusal; loopback-only and the macOS Screen Sharing rule
// apply to every purpose there.
func (s *Server) sshTicketPortRefused(ctx context.Context, runID uuid.UUID, principal string, ch ssh.NewChannel) bool {
	tc, ok := sshTicketOf(ctx)
	if !ok || tc.purpose != ticketPurposeSSHForward {
		return false
	}
	var m sshDirectTCPIPMsg
	if ssh.Unmarshal(ch.ExtraData(), &m) != nil || slices.Contains(tc.ports, int(m.DestPort)) {
		return false
	}
	s.sshAuditTicketChannel(ctx, runID, principal, map[string]any{"channel_type": "direct-tcpip", "reason": "ticket_port", "port": m.DestPort})
	ports := make([]string, len(tc.ports))
	for i, p := range tc.ports {
		ports[i] = fmt.Sprint(p)
	}
	_ = ch.Reject(ssh.Prohibited, "wardyn: this SSH ticket forwards only port(s) "+strings.Join(ports, ", "))
	return true
}

func (s *Server) sshAuditTicketChannel(ctx context.Context, runID uuid.UUID, principal string, data map[string]any) {
	s.recordAudit(ctx, s.auditEvent(&runID, types.ActorHuman, principal, "ssh.channel.reject", runID.String(), "failure", mustJSON(data)))
}
