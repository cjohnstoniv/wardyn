// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/netip"
	"slices"
)

// localPeerTrusted is local mode's peer gate: a loopback peer, or, with
// LocalTrustForwarder, a peer inside LocalTrustedPeers.
func (s *Server) localPeerTrusted(remoteAddr string) bool {
	if isLoopbackRemoteAddr(remoteAddr) {
		return true
	}
	ap, err := netip.ParseAddrPort(remoteAddr)
	return err == nil && s.cfg.LocalTrustForwarder &&
		slices.ContainsFunc(s.cfg.LocalTrustedPeers, func(p netip.Prefix) bool { return p.Contains(ap.Addr().Unmap()) })
}

// localPeerRefusal is the local-mode peer gate's 403 sentence.
const localPeerRefusal = "local mode: request peer is not loopback (bind wardynd to 127.0.0.1, set WARDYN_LOCAL_TRUST_FORWARDER when behind a loopback-only publish " +
	"and WARDYN_LOCAL_TRUSTED_PEERS when its traffic arrives from an address wardynd does not resolve, or configure auth)"
