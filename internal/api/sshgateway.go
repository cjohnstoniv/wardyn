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
	// maxSSHSessionsPerRun bounds concurrent "session" channels (shell/exec/
	// subsystem) a single run may have open across every SSH connection
	// combined. A resource-exhaustion bound, not a product limit — raise it if
	// a real workflow needs more concurrent shells into one run.
	maxSSHSessionsPerRun = 4
)

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
			go func() {
				defer func() { <-sem }()
				// goSafe's contract, inlined: cmd/wardynd's goSafe lives in
				// package main and cannot be imported here, but an unrecovered
				// panic in this per-connection goroutine would still take down
				// the whole daemon, so it gets the same containment.
				defer func() {
					if r := recover(); r != nil {
						slog.Error("wardynd: PANIC in ssh connection handler (contained)", slog.Any("panic", r))
					}
				}()
				s.handleSSHConn(ctx, conn, cfg)
			}()
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
		MaxAuthTries:      sshMaxAuthTries,
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

// sshAuth is the gateway's ENTIRE auth+authz decision (ServerConfig's
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
// Every rejection is audited under ssh.auth — including an unknown key or an
// unparseable/unknown run id — so a scan against the gateway leaves a trail.
func (s *Server) sshAuth(conn ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
	ctx := s.cfg.BaseCtx
	fp := ssh.FingerprintSHA256(key)

	runID, err := uuid.Parse(conn.User())
	if err != nil {
		s.sshAuditAuthFailure(ctx, nil, fp, "invalid username (want a run id)")
		return nil, errors.New("ssh: username must be the run id")
	}
	rec, err := s.cfg.Store.GetSSHKeyByFingerprint(ctx, fp)
	if err != nil {
		s.sshAuditAuthFailure(ctx, &runID, fp, "unregistered key")
		return nil, errors.New("ssh: unknown key")
	}
	// Defense in depth: re-verify byte-equality against the STORED key
	// material rather than trusting the fingerprint index alone. A SHA256
	// collision is not a realistic threat here — this guards against an
	// implementation bug in the index, not a cryptographic one.
	stored, _, _, _, perr := ssh.ParseAuthorizedKey([]byte(rec.PublicKey))
	if perr != nil || stored == nil || !bytes.Equal(stored.Marshal(), key.Marshal()) {
		s.sshAuditAuthFailure(ctx, &runID, fp, "stored key mismatch")
		return nil, errors.New("ssh: unknown key")
	}
	run, err := s.cfg.Store.GetRun(ctx, runID)
	if err != nil {
		s.sshAuditAuthFailure(ctx, &runID, fp, "unknown run")
		return nil, errors.New("ssh: unknown run")
	}
	if run.CreatedBy != rec.Principal {
		s.sshAuditAuthFailure(ctx, &runID, fp, "not the run owner")
		return nil, errors.New("ssh: not authorized for this run")
	}

	s.recordAudit(ctx, s.auditEvent(&runID, types.ActorHuman, rec.Principal, "ssh.auth", fp, "success", nil))
	return &ssh.Permissions{Extensions: map[string]string{
		"principal": rec.Principal,
		"run_id":    runID.String(),
	}}, nil
}

func (s *Server) sshAuditAuthFailure(ctx context.Context, runID *uuid.UUID, fingerprint, reason string) {
	s.recordAudit(ctx, s.auditEvent(runID, types.ActorHuman, "unknown", "ssh.auth", fingerprint, "failure",
		mustJSON(map[string]any{"reason": reason})))
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
		case "session":
			if !s.sshAcquireSession(runID) {
				_ = newCh.Reject(ssh.ResourceShortage,
					fmt.Sprintf("too many concurrent SSH sessions for this run (max %d)", maxSSHSessionsPerRun))
				continue
			}
			go func(nc ssh.NewChannel) {
				defer s.sshReleaseSession(runID)
				s.handleSSHSessionChannel(connCtx, runID, principal, nc)
			}(newCh)
		case "direct-tcpip":
			go s.handleSSHDirectTCPIP(connCtx, runID, principal, newCh)
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

// sshAcquireSession reports whether runID may open one more concurrent
// "session" channel, incrementing its count on success (maxSSHSessionsPerRun).
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
// not liveness) and fails closed unless it is RUNNING with a live sandbox —
// the same precondition handleAttachWS enforces before every attach. Returns
// a non-empty, caller-facing message on failure; callers must return
// immediately and surface it however fits their channel's lifecycle stage
// (sendChannelError on an already-accepted channel, NewChannel.Reject
// otherwise).
func (s *Server) sshFreshRun(ctx context.Context, runID uuid.UUID) (types.AgentRun, string) {
	run, err := s.cfg.Store.GetRun(ctx, runID)
	if err != nil {
		return types.AgentRun{}, "run not found"
	}
	if run.State != types.RunRunning || run.SandboxRef == "" {
		return types.AgentRun{}, "run is not RUNNING; ssh unavailable (state=" + string(run.State) + ")"
	}
	return run, ""
}
