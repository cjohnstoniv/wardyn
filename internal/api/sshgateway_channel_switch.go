// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"golang.org/x/crypto/ssh"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// sshChannelKind is one thing the gateway serves, as WARDYN_SSH_CHANNELS names it. sshKindSync (the
// wardyn-sync subsystem) is not a name an operator can list: it runs the same sftp-server, so it follows sftp.
type sshChannelKind string

const (
	sshKindShell   sshChannelKind = "shell"
	sshKindExec    sshChannelKind = "exec"
	sshKindSFTP    sshChannelKind = "sftp"
	sshKindForward sshChannelKind = "forward"
	sshKindSync    sshChannelKind = "sync"
)

// sshServedKinds is the names WARDYN_SSH_CHANNELS accepts, sorted.
var sshServedKinds = []sshChannelKind{sshKindExec, sshKindForward, sshKindSFTP, sshKindShell}

// sshChannelOn reports whether the gateway serves k: every kind when no list is set, else the listed ones.
// Sync answers as sftp.
func (s *Server) sshChannelOn(k sshChannelKind) bool {
	if k == sshKindSync {
		k = sshKindSFTP
	}
	return s.cfg.SSHChannels == nil || s.cfg.SSHChannels[string(k)]
}

// sshKindWord names a channel kind for the sentence a refused channel gets.
func sshKindWord(k sshChannelKind) string {
	switch k {
	case sshKindShell:
		return "a terminal"
	case sshKindExec:
		return "commands (exec)"
	case sshKindSFTP, sshKindSync:
		return "file transfer (sftp)"
	case sshKindForward:
		return "port forwarding"
	}
	return string(k)
}

// sshChannelOffMsg is the one sentence a channel the switch leaves out gets.
func sshChannelOffMsg(k sshChannelKind) string {
	return "wardyn: this deployment's SSH gateway does not serve " + sshKindWord(k) + " (WARDYN_SSH_CHANNELS)"
}

// sshKindRefusal is the sentence for a kind the switch leaves out or, on a support connection, the owner did not
// tick, with the one ssh.channel.reject row written for it; "" when the kind may run.
func (s *Server) sshKindRefusal(ctx context.Context, runID uuid.UUID, principal string, k sshChannelKind, channelType string) string {
	if !s.sshChannelOn(k) {
		s.sshAuditChannelOff(ctx, runID, types.ActorHuman, principal, channelType)
		return sshChannelOffMsg(k)
	}
	if sshSupportDoorRefused(ctx, k) {
		s.sshAuditSupportDoorOff(ctx, runID, principal, channelType)
		return sshSupportOffMsg(k)
	}
	return ""
}

// sshAuditChannelOff writes the ssh.channel.reject row for a channel the switch turned off, as the actor type at:
// the gateway's callers are people, the REST exec door's may be a service or the admin token.
func (s *Server) sshAuditChannelOff(ctx context.Context, runID uuid.UUID, at types.ActorType, principal, channelType string) {
	s.recordAudit(ctx, s.auditEvent(&runID, at, principal, "ssh.channel.reject",
		runID.String(), "failure", mustJSON(map[string]any{
			"channel_type": channelType, "reason": "channel_off",
		})))
}

// sshRefuseForward refuses a direct-tcpip channel when forward is off or, for a helper, not ticked, else applies
// the data_out rule.
func (s *Server) sshRefuseForward(ctx context.Context, runID uuid.UUID, principal string, ch ssh.NewChannel, rule types.TransferRule) bool {
	if msg := s.sshKindRefusal(ctx, runID, principal, sshKindForward, "direct-tcpip"); msg != "" {
		_ = ch.Reject(ssh.Prohibited, msg)
		return true
	}
	return s.sshRefuseDataOut(ctx, runID, principal, ch, rule)
}

// sshGatewayHealthz is /healthz's "ssh" field: nil (disabled) or the pane's
// three discovery facts (enabled, advertise_addr, host_key_fingerprint), plus
// proxy_command only when WARDYN_SSH_PROXY_COMMAND is set — see healthz.go's
// handleHealthz.
func (s *Server) sshGatewayHealthz() map[string]any {
	if s.cfg.SSHListenAddr == "" || len(s.cfg.SSHHostKey) == 0 || s.sshUnbound.Load() {
		return nil
	}
	signer, err := ssh.NewSignerFromKey(s.cfg.SSHHostKey)
	if err != nil {
		return nil
	}
	out := map[string]any{
		"enabled":              true,
		"advertise_addr":       s.cfg.SSHAdvertiseAddr,
		"host_key_fingerprint": ssh.FingerprintSHA256(signer.PublicKey()),
	}
	served := []string{}
	for _, k := range sshServedKinds {
		if s.sshChannelOn(k) {
			served = append(served, string(k))
		}
	}
	out["channels"] = served
	if s.cfg.SSHProxyCommand != "" {
		out["proxy_command"] = s.cfg.SSHProxyCommand
	}
	return out
}

// mayDispatch first refuses a kind WARDYN_SSH_CHANNELS leaves out, or that a helper's owner did not tick (the one
// sentence on the channel, then a false reply, or for sync an ack and then the sentence, as the sync cap does;
// one ssh.channel.reject row). It then refuses (and replies false to) a second dispatch, and — when
// shared is set — one a sync-only channel (admitted past the shared cap, see
// sshOpenSlot) may not make: that one is audited once, answered with the cap
// text and closed, which frees the sync slot it was holding.
func (g *sshSessionGate) mayDispatch(req *ssh.Request, shared bool, path string, kind sshChannelKind) bool {
	if msg := g.s.sshKindRefusal(g.ctx, g.runID, g.principal, kind, string(kind)); msg != "" {
		if kind == sshKindSync {
			// Acked, then ended with the sentence, as the sync cap is: the sync client stops on a false reply
			// before it reads the channel's stderr.
			_ = req.Reply(true, nil)
			sendChannelError(g.channel, msg)
			return false
		}
		// The sentence first, as sshDataOutRefused writes it: a reply after the close is dropped, so a client
		// reads the sentence and the exit status and not only a failed request.
		sendChannelError(g.channel, msg)
		_ = req.Reply(false, nil)
		return false
	}
	if g.started || g.s.sshDataOutRefused(g, path) {
		_ = req.Reply(false, nil)
		return false
	}
	if shared && g.slot.sync {
		if !g.capAudited {
			g.capAudited = true
			g.s.sshAuditChannelRejected(g.ctx, g.runID, g.principal, "session", sshCapReasonChannels, g.s.cfg.SSHMaxSessionsPerRun)
		}
		_ = req.Reply(false, nil)
		sendChannelError(g.channel, fmt.Sprintf("too many concurrent SSH channels for this run (max %d)", g.s.cfg.SSHMaxSessionsPerRun))
		return false
	}
	return true
}
