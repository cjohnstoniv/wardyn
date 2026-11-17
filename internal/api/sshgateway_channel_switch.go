// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"fmt"

	"golang.org/x/crypto/ssh"
)

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
	if s.cfg.SSHProxyCommand != "" {
		out["proxy_command"] = s.cfg.SSHProxyCommand
	}
	return out
}

// mayDispatch refuses (and replies false to) a second dispatch, and — when
// shared is set — one a sync-only channel (admitted past the shared cap, see
// sshOpenSlot) may not make: that one is audited once, answered with the cap
// text and closed, which frees the sync slot it was holding.
func (g *sshSessionGate) mayDispatch(req *ssh.Request, shared bool, path string) bool {
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
