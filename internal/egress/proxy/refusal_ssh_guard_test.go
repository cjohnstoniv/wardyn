// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The SSH guard's tunnel refusal is its own sentence and no one's classified refusal:
// the row says why, and its Refusal stays nil, as it was before refusals were classified.
func TestRefusal_SSHGuardTunnelRowCarriesNoRefusal(t *testing.T) {
	p, buf := attributedTestProxy(t, types.RunPolicySpec{AllowedDomains: []string{"git.test"}}, "127.0.0.1:1", teamA)
	client, clientPeer := net.Pipe()
	upstream, upstreamPeer := net.Pipe()
	t.Cleanup(func() { _ = clientPeer.Close(); _ = upstreamPeer.Close() })
	w := newTunnelWatch(upstream)
	w.onSSH = func() error { return errSSHTunnel }
	go func() { _, _ = io.Copy(io.Discard, upstreamPeer) }()
	ended := make(chan struct{})
	go func() {
		first, err := tunnel(client, w)
		seen := decisionLog(egress.Request{Host: "git.test", Port: 22}, egress.Allow, "policy:allowed")
		p.tunnelEnded(&seen, "git.test", w, first, err)
		close(ended)
	}()
	_, _ = clientPeer.Write([]byte("SSH-2.0-client\r\n"))
	select {
	case <-ended:
	case <-time.After(5 * time.Second):
		t.Fatal("the tunnel was still open 5s after the client's SSH identification")
	}
	d := findDecision(t, buf, ruleSourceTunnelFailed)
	if d.Decision != egress.Deny || !strings.HasPrefix(d.Cause, "tunnel ssh: ") {
		t.Fatalf("row %s cause %q, want a deny with the tunnel ssh sentence", d.Decision, d.Cause)
	}
	if d.Refusal != nil {
		t.Fatalf("refusal = %+v on the SSH guard's row, want none", d.Refusal)
	}
}
