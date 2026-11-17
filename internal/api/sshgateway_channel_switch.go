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
// wardyn-sync subsystem) is not a name an operator can list: it is always served.
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

// sshChannelOn reports whether the gateway serves k: sync always, every kind when no list is set, else
// the listed ones.
func (s *Server) sshChannelOn(k sshChannelKind) bool {
	return k == sshKindSync || s.cfg.SSHChannels == nil || s.cfg.SSHChannels[string(k)]
}

// sshChannelOffMsg is the one sentence a refused channel gets.
func sshChannelOffMsg(k sshChannelKind) string {
	word := string(k)
	switch k {
	case sshKindShell:
		word = "a terminal"
	case sshKindExec:
		word = "commands (exec)"
	case sshKindSFTP:
		word = "file transfer (sftp)"
	case sshKindForward:
		word = "port forwarding"
	}
	return "this deployment's SSH gateway does not serve " + word + " (WARDYN_SSH_CHANNELS)"
}

// sshAuditChannelOff writes the ssh.channel.reject row for a channel the switch turned off.
func (s *Server) sshAuditChannelOff(ctx context.Context, runID uuid.UUID, principal, channelType string) {
	s.recordAudit(ctx, s.auditEvent(&runID, types.ActorHuman, principal, "ssh.channel.reject",
		runID.String(), "failure", mustJSON(map[string]any{
			"channel_type": channelType, "reason": "channel_off",
		})))
}

// sshRefuseForward refuses a direct-tcpip channel when forward is off, else applies the data_out rule.
func (s *Server) sshRefuseForward(ctx context.Context, runID uuid.UUID, principal string, ch ssh.NewChannel, rule types.TransferRule) bool {
	if !s.sshChannelOn(sshKindForward) {
		s.sshAuditChannelOff(ctx, runID, principal, "direct-tcpip")
		_ = ch.Reject(ssh.Prohibited, sshChannelOffMsg(sshKindForward))
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

// mayDispatch first refuses a kind WARDYN_SSH_CHANNELS leaves out (replied false, the one sentence on the
// channel, one ssh.channel.reject row). It then refuses (and replies false to) a second dispatch, and — when
// shared is set — one a sync-only channel (admitted past the shared cap, see
// sshOpenSlot) may not make: that one is audited once, answered with the cap
// text and closed, which frees the sync slot it was holding.
func (g *sshSessionGate) mayDispatch(req *ssh.Request, shared bool, path string, kind sshChannelKind) bool {
	if !g.s.sshChannelOn(kind) {
		_ = req.Reply(false, nil)
		sendChannelError(g.channel, sshChannelOffMsg(kind))
		g.s.sshAuditChannelOff(g.ctx, g.runID, g.principal, string(kind))
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
