// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// The SSH gateway (C2): `ssh <run-id>@<advertise-host>` lands in the same
// tmux session and masked live recorder the web terminal (attach.go) uses.
// Lives here, next to attach.go, rather than a new package, because the SSH
// caller must go through the SAME runIsUnrecordable gate and liveMaskWriter
// the web terminal does — splitting them across packages would let a second
// caller miss that gate.
//
// Channel-level protocol handling (session requests, exec/sftp/direct-tcpip
// bridging) is sshgateway_channels.go; this file is connection lifecycle:
// listener bring-up, the DoS bounds, and public-key auth+authz.
package api

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"time"

	"github.com/google/uuid"
	"golang.org/x/crypto/ssh"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

const (
	// sshHandshakeTimeout bounds ONLY the pre-auth handshake (net.Conn deadline,
	// cleared once ssh.NewServerConn succeeds) — NewServerConn otherwise blocks
	// with no default timeout, and a slowloris against wardynd's pre-auth
	// listener is a containment incident, not a nuisance.
	sshHandshakeTimeout = 15 * time.Second
	// sshMaxAuthTries bounds per-connection auth attempts. Set explicitly
	// (rather than relying on ssh.ServerConfig's own default-when-zero of 6)
	// so the bound is self-documenting here, not implicit in a zero value.
	sshMaxAuthTries = 6
	// maxSSHConnections bounds total concurrent SSH connections this process
	// will handshake at once. A connection accepted over the cap is closed
	// immediately — before any handshake byte is read or written — so the
	// bound bites before a goroutine or a NewServerConn call is spent on it.
	maxSSHConnections = 64
	// maxSSHSessionsPerRun bounds concurrent SSH channels — "session" (shell/
	// exec/sftp) AND "direct-tcpip" (-L forwards) both draw from the SAME
	// per-run counter — a single run may have open across every SSH
	// connection combined. A resource-exhaustion bound, not a product limit —
	// raise it if a real workflow needs more concurrent shells/forwards into
	// one run.
	maxSSHSessionsPerRun = 4
	// sshMaxEnvVars bounds how many "env" requests a single session channel
	// accepts before dispatch (shell/exec/subsystem) — an unbounded client
	// could otherwise grow the env slice forever pre-dispatch.
	sshMaxEnvVars = 32
	// sshAuthTimeout bounds sshAuth's store lookups AND sshVerifiedAuth's
	// synchronous audit write — both run as ssh.ServerConfig callbacks
	// (PublicKeyCallback / VerifiedPublicKeyCallback respectively). The
	// pre-auth handshake deadline (sshHandshakeTimeout) cannot interrupt a
	// blocked call INSIDE either callback — NewServerConn is what owns the
	// deadline, not the callback — so a slow/blocked backend call in either
	// one would otherwise let an unauthenticated client park a connection
	// slot indefinitely.
	sshAuthTimeout = 5 * time.Second
)

// sshGo runs fn in a new goroutine with panic recovery — the ONE place that
// containment lives, used at every per-connection and per-channel spawn site
// in this file and sshgateway_channels.go. cmd/wardynd's goSafe (the same
// contract) lives in package main and can't be imported here, but an
// unrecovered panic in ANY of these goroutines would still crash the whole
// daemon — and with it the kill switch every other run depends on, not just
// this one SSH session — so every spawn site needs this, not only the
// connection-level one.
func sshGo(fn func()) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				slog.Error("wardynd: PANIC in ssh goroutine (contained)", slog.Any("panic", r))
			}
		}()
		fn()
	}()
}

// ServeSSHGateway blocks running the SSH gateway's accept loop until ctx is
// cancelled or the listener fails unrecoverably. A no-op (nil error) when the
// gateway is disabled — WARDYN_SSH_LISTEN empty = off = no listener, no new
// surface — so cmd/wardynd may call this unconditionally once it has decided
// whether to launch it in a goroutine at all.
func (s *Server) ServeSSHGateway(ctx context.Context) error {
	if s.cfg.SSHListenAddr == "" || len(s.cfg.SSHHostKey) == 0 {
		return nil
	}
	signer, err := ssh.NewSignerFromKey(s.cfg.SSHHostKey)
	if err != nil {
		return fmt.Errorf("ssh gateway: host signer: %w", err)
	}
	cfg := s.sshServerConfig(signer)

	ln, err := net.Listen("tcp", s.cfg.SSHListenAddr)
	if err != nil {
		return fmt.Errorf("ssh gateway: listen %s: %w", s.cfg.SSHListenAddr, err)
	}
	defer ln.Close()
	slog.Info("wardynd: ssh gateway listening",
		slog.String("listen", s.cfg.SSHListenAddr),
		slog.String("advertise", s.cfg.SSHAdvertiseAddr),
		slog.String("host_key_fingerprint", ssh.FingerprintSHA256(signer.PublicKey())),
	)

	// Close the listener when ctx is cancelled so Accept() unblocks with an
	// error and the loop below returns cleanly — the ctx-close idiom, no
	// second http.Server-style Shutdown needed for a bare net.Listener.
	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()

	sem := make(chan struct{}, maxSSHConnections)
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil // clean shutdown (the goroutine above closed ln)
			}
			return fmt.Errorf("ssh gateway: accept: %w", err)
		}
		select {
		case sem <- struct{}{}:
			sshGo(func() {
				defer func() { <-sem }()
				s.handleSSHConn(ctx, conn, cfg)
			})
		default:
			// Over the concurrent-connection cap: reject before any handshake
			// byte is read/written — the DoS bound has to bite here, not after
			// a goroutine and a NewServerConn call are already spent on it.
			_ = conn.Close()
		}
	}
}

// sshServerConfig builds the gateway's auth posture: registered public keys
// only (no PasswordCallback / KeyboardInteractiveCallback is set, so neither
// auth method is ever offered to a client).
func (s *Server) sshServerConfig(signer ssh.Signer) *ssh.ServerConfig {
	cfg := &ssh.ServerConfig{
		PublicKeyCallback: s.sshAuth,
		// VerifiedPublicKeyCallback runs ONLY after golang.org/x/crypto/ssh has
		// verified a real signature over the offered key (see sshVerifiedAuth's
		// doc) — this is where ssh.auth success is now audited, not sshAuth.
		VerifiedPublicKeyCallback: s.sshVerifiedAuth,
		MaxAuthTries:              sshMaxAuthTries,
	}
	cfg.AddHostKey(signer)
	return cfg
}

// sshGatewayHealthz is /healthz's "ssh" field: nil (disabled) or the pane's
// three discovery facts (enabled, advertise_addr, host_key_fingerprint) — see
// server.go's handleHealthz.
func (s *Server) sshGatewayHealthz() map[string]any {
	if s.cfg.SSHListenAddr == "" || len(s.cfg.SSHHostKey) == 0 {
		return nil
	}
	signer, err := ssh.NewSignerFromKey(s.cfg.SSHHostKey)
	if err != nil {
		return nil
	}
	return map[string]any{
		"enabled":              true,
		"advertise_addr":       s.cfg.SSHAdvertiseAddr,
		"host_key_fingerprint": ssh.FingerprintSHA256(signer.PublicKey()),
	}
}

// sshAuth is the gateway's auth+authz DECISION (ServerConfig's
// PublicKeyCallback): registered public keys only, OWNER-ONLY authorization
// (run.CreatedBy == the key's principal). Username = the target run's UUID
// (conn.User()) — SSH has no cookie, so the run id IS the addressing the
// client supplies, the same way `ssh host` names a machine.
//
// ponytail: owner-only is a single principal-equality check today; an
// admin/operator override to reach ANOTHER human's run needs a role column
// this table doesn't have yet (see THREAT-MODEL.md's SSH gateway residual) —
// until then an admin uses the web terminal (GET /runs/{id}/attach) for
// someone else's run, exactly like a viewer must.
//
// Every REJECTION is audited under ssh.auth right here — including an
// unknown key or an unparseable/unknown run id — so a scan against the
// gateway leaves a trail. SUCCESS is deliberately NOT audited here (W25.4-1):
// golang.org/x/crypto/ssh calls PublicKeyCallback on the UNSIGNED "query"
// every pubkey auth attempt opens with (RFC 4252 §7), and even for a direct
// signed attempt this callback still runs BEFORE the signature is verified —
// so a caller who merely KNOWS a victim's registered public key (never the
// matching private key) could reach this function, get approved, and —
// before this fix — walk away with a forged ssh.auth success row attributed
// to that victim, having proven nothing. The *ssh.Permissions returned on
// approval here are therefore PROVISIONAL; sshVerifiedAuth
// (VerifiedPublicKeyCallback) records the success audit, and the ssh package
// guarantees it runs ONLY after a real signature over this exact key verifies.
//
// sshAuthTimeout-bounded: this runs INSIDE ssh.NewServerConn's handshake,
// which has no deadline of its own over callback-internal work — the
// pre-auth net.Conn deadline (sshHandshakeTimeout, handleSSHConn) only fires
// on the NEXT socket I/O, so it does nothing while this function is blocked
// on a store call or an audit write. Without its own bound, a slow/stuck
// backend call here lets an UNAUTHENTICATED client park a connection slot
// (one of maxSSHConnections) indefinitely.
func (s *Server) sshAuth(conn ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
	ctx, cancel := context.WithTimeout(s.cfg.BaseCtx, sshAuthTimeout)
	defer cancel()
	fp := ssh.FingerprintSHA256(key)

	runID, err := uuid.Parse(conn.User())
	if err != nil {
		s.sshAuditAuthFailure(ctx, conn, nil, "unknown", fp, "invalid username (want a run id)")
		return nil, errors.New("ssh: username must be the run id")
	}
	rec, err := s.cfg.Store.GetSSHKeyByFingerprint(ctx, fp)
	if err != nil {
		s.sshAuditAuthFailure(ctx, conn, &runID, "unknown", fp, "unregistered key")
		return nil, errors.New("ssh: unknown key")
	}
	// Defense in depth: re-verify byte-equality against the STORED key
	// material rather than trusting the fingerprint index alone. A SHA256
	// collision is not a realistic threat here — this guards against an
	// implementation bug in the index, not a cryptographic one.
	stored, _, _, _, perr := ssh.ParseAuthorizedKey([]byte(rec.PublicKey))
	if perr != nil || stored == nil || !bytes.Equal(stored.Marshal(), key.Marshal()) {
		s.sshAuditAuthFailure(ctx, conn, &runID, "unknown", fp, "stored key mismatch")
		return nil, errors.New("ssh: unknown key")
	}
	run, err := s.cfg.Store.GetRun(ctx, runID)
	if err != nil {
		s.sshAuditAuthFailure(ctx, conn, &runID, "unknown", fp, "unknown run")
		return nil, errors.New("ssh: unknown run")
	}
	if run.CreatedBy != rec.Principal {
		// The key itself is genuine (owned by rec.Principal) — just not
		// authorized for THIS run — so, unlike the other failures above, a
		// real principal is known here and worth recording instead of
		// "unknown".
		s.sshAuditAuthFailure(ctx, conn, &runID, rec.Principal, fp, "not the run owner")
		return nil, errors.New("ssh: not authorized for this run")
	}

	// Provisional approval ONLY — no success audit here, see the function doc:
	// the client has not yet proven it holds the private key for this offer.
	// sshVerifiedAuth records ssh.auth success, and only after
	// ssh.ServerConfig has verified a real signature over this key.
	return &ssh.Permissions{Extensions: map[string]string{
		"principal": rec.Principal,
		"run_id":    runID.String(),
	}}, nil
}

func (s *Server) sshAuditAuthFailure(ctx context.Context, conn ssh.ConnMetadata, runID *uuid.UUID, actor, fingerprint, reason string) {
	ev := s.auditEvent(runID, types.ActorHuman, actor, "ssh.auth", fingerprint, "failure",
		mustJSON(map[string]any{"reason": reason}))
	ev.SourceIP = conn.RemoteAddr().String()
	s.recordAudit(ctx, ev)
}

// sshVerifiedAuth is ServerConfig's VerifiedPublicKeyCallback (W25.4-1's fix):
// golang.org/x/crypto/ssh calls it ONLY after verifying the client's
// signature over this exact key — i.e. only once the client has actually
// proven it holds the matching private key, which sshAuth's own invocation
// (PublicKeyCallback) cannot guarantee (see its doc). perms is the SAME
// *ssh.Permissions object sshAuth returned for this key, ownership
// transferred to this callback per the ssh package's contract — principal and
// run_id are already resolved in its Extensions, so no store lookups are
// needed here, only the audit write sshAuth used to do prematurely.
//
// sshAuthTimeout-bounded for the same reason sshAuth is: this also runs
// inside ssh.NewServerConn's handshake, uninterruptible by the pre-auth
// net.Conn deadline (sshHandshakeTimeout).
func (s *Server) sshVerifiedAuth(conn ssh.ConnMetadata, key ssh.PublicKey, perms *ssh.Permissions, _ string) (*ssh.Permissions, error) {
	ctx, cancel := context.WithTimeout(s.cfg.BaseCtx, sshAuthTimeout)
	defer cancel()
	runID, err := uuid.Parse(perms.Extensions["run_id"])
	if err != nil {
		// Unreachable in practice: sshAuth only ever returns Permissions with
		// a well-formed run_id already in Extensions.
		return nil, errors.New("ssh: internal: missing run id in verified permissions")
	}
	ev := s.auditEvent(&runID, types.ActorHuman, perms.Extensions["principal"], "ssh.auth", ssh.FingerprintSHA256(key), "success", nil)
	ev.SourceIP = conn.RemoteAddr().String()
	s.recordAudit(ctx, ev)
	return perms, nil
}

// handleSSHConn completes the handshake (bounded by sshHandshakeTimeout, then
// cleared for the life of the session) and dispatches every channel the
// client opens. One goroutine per connection; each channel gets its own off
// this one (sshgateway_channels.go). connCtx is cancelled the moment this
// function returns (the connection died, one way or another), so no channel
// handler outlives its connection.
func (s *Server) handleSSHConn(ctx context.Context, nc net.Conn, cfg *ssh.ServerConfig) {
	defer nc.Close()
	_ = nc.SetDeadline(time.Now().Add(sshHandshakeTimeout))

	sconn, chans, globalReqs, err := ssh.NewServerConn(nc, cfg)
	if err != nil {
		// sshAuth already audited a specific reason when PublicKeyCallback was
		// reached; a pure transport failure (bad version string, KEX failure)
		// never identified a run/principal to attribute to, so it is not
		// separately audited here.
		return
	}
	defer sconn.Close()
	// Clear the handshake deadline: an established session runs for the life
	// of the connection, not the pre-auth bound above.
	_ = nc.SetDeadline(time.Time{})

	// No global requests are served. This is also how "-R" (remote/reverse
	// port forwarding, the "tcpip-forward" global request) is refused:
	// DiscardRequests replies false to every request that wants a reply,
	// which is exactly a `ssh -R` client's "request denied by peer" path.
	go ssh.DiscardRequests(globalReqs)

	runID, err := uuid.Parse(sconn.Permissions.Extensions["run_id"])
	if err != nil {
		return // unreachable in practice: sshAuth only succeeds after parsing this
	}
	principal := sconn.Permissions.Extensions["principal"]

	connCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	for newCh := range chans {
		switch newCh.ChannelType() {
		// "session" (shell/exec/sftp) and "direct-tcpip" (-L forwards) share
		// ONE per-run cap (sshAcquireSession/maxSSHSessionsPerRun) — a single
		// owner opening unbounded forwards is exactly the same resource-
		// exhaustion shape as unbounded shells, so both draw from the same
		// counter rather than needing a second one.
		case "session":
			if !s.sshAcquireSession(runID) {
				_ = newCh.Reject(ssh.ResourceShortage,
					fmt.Sprintf("too many concurrent SSH channels for this run (max %d)", maxSSHSessionsPerRun))
				continue
			}
			sshGo(func() {
				defer s.sshReleaseSession(runID)
				s.handleSSHSessionChannel(connCtx, runID, principal, newCh)
			})
		case "direct-tcpip":
			if !s.sshAcquireSession(runID) {
				_ = newCh.Reject(ssh.ResourceShortage,
					fmt.Sprintf("too many concurrent SSH channels for this run (max %d)", maxSSHSessionsPerRun))
				continue
			}
			sshGo(func() {
				defer s.sshReleaseSession(runID)
				s.handleSSHDirectTCPIP(connCtx, runID, principal, newCh)
			})
		default:
			// Structurally refuses everything else too — notably
			// "auth-agent@openssh.com" and "x11", the channel TYPES agent/X11
			// forwarding rides on. Combined with never acking the "-req"
			// request that would have authorized opening one
			// (sshgateway_channels.go's session request loop), forwarding is
			// refused by omission on both layers — no protocol
			// reimplementation needed to "reject" what is simply never granted.
			_ = newCh.Reject(ssh.UnknownChannelType, "unsupported channel type "+newCh.ChannelType())
		}
	}
}

// sshAcquireSession reports whether runID may open one more concurrent SSH
// channel — "session" (shell/exec/sftp) OR "direct-tcpip" (-L forward), same
// counter — incrementing its count on success (maxSSHSessionsPerRun).
// Callers that get true MUST call sshReleaseSession exactly once when that
// channel's handling ends.
func (s *Server) sshAcquireSession(runID uuid.UUID) bool {
	s.sshSessionsMu.Lock()
	defer s.sshSessionsMu.Unlock()
	if s.sshSessions == nil {
		s.sshSessions = map[uuid.UUID]int{}
	}
	if s.sshSessions[runID] >= maxSSHSessionsPerRun {
		return false
	}
	s.sshSessions[runID]++
	return true
}

func (s *Server) sshReleaseSession(runID uuid.UUID) {
	s.sshSessionsMu.Lock()
	defer s.sshSessionsMu.Unlock()
	s.sshSessions[runID]--
	if s.sshSessions[runID] <= 0 {
		delete(s.sshSessions, runID)
	}
}

// sshFreshRun re-fetches run fresh (state may have changed since the SSH
// connection authenticated — auth checks OWNERSHIP, which is immutable, but
// not liveness) and fails closed unless a Runner is wired AND the run is
// RUNNING with a live sandbox — the same precondition handleAttachWS
// enforces before every attach. The nil-Runner guard lives HERE, not in each
// of the four callers (bridgeSSHShell/bridgeSSHExec/bridgeSSHSFTP/
// handleSSHDirectTCPIP all call this first): under the supported `-runner
// none` (headless) mode, s.cfg.Runner is a nil INTERFACE value, and calling
// ANY method on it — Attach, ExecStream — panics immediately (no concrete
// type to dispatch to); one guard here closes that for every bridge instead
// of three call sites that could individually forget it.
//
// Returns a non-empty, caller-facing message on failure; callers must return
// immediately and surface it however fits their channel's lifecycle stage
// (sendChannelError on an already-accepted channel, NewChannel.Reject
// otherwise).
func (s *Server) sshFreshRun(ctx context.Context, runID uuid.UUID) (types.AgentRun, string) {
	if s.cfg.Runner == nil {
		return types.AgentRun{}, "no runner configured; ssh unavailable"
	}
	run, err := s.cfg.Store.GetRun(ctx, runID)
	if err != nil {
		return types.AgentRun{}, "run not found"
	}
	if run.State != types.RunRunning || run.SandboxRef == "" {
		return types.AgentRun{}, "run is not RUNNING; ssh unavailable (state=" + string(run.State) + ")"
	}
	return run, ""
}
