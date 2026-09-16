// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bytes"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestMITMHostsKeepOnlyTheLastPortPerHost (B10-F9) PINS today's behaviour rather
// than changing it: mitmHosts/mitmPorts are keyed on the bare host, so two
// entries for the same host ("m.corp:443" and "m.corp:8443") collapse — the LAST
// one wins and the other port silently falls through as an opaque tunnel, never
// TLS-terminated and never offered the operator's token.
//
// It is LATENT, and this test is the guard on why: the only producer,
// planArtifactRedirect, dedupes by BARE host (its seenHost map), so a second
// entry for one host cannot be authored today. Re-keying the four maps on
// "host:port" is deferred until a second producer exists — at which point this
// test turns red and names exactly what changed.
func TestMITMHostsKeepOnlyTheLastPortPerHost(t *testing.T) {
	mk := func(hosts ...string) *Proxy {
		return newProxy(Options{
			RunID:     uuid.New(),
			Policy:    CompilePolicy(types.RunPolicySpec{AllowedDomains: []string{"m.corp:443", "m.corp:8443"}}),
			Sink:      &decisionSink{out: &bytes.Buffer{}, ch: make(chan egress.DecisionLog, 8)},
			MITMHosts: hosts,
			Resolver:  publicResolver{},
		})
	}

	t.Run("each port alone is eligible on its own port only", func(t *testing.T) {
		for _, c := range []struct {
			entry       string
			want, other int
		}{
			{"m.corp:443", 443, 8443},
			{"m.corp:8443", 8443, 443},
		} {
			p := mk(c.entry)
			if !p.isCorpMITMHost("m.corp") {
				t.Fatalf("%s: isCorpMITMHost = false", c.entry)
			}
			if !p.mitmPortAllowed("m.corp", c.want) {
				t.Errorf("%s: port %d not allowed", c.entry, c.want)
			}
			if p.mitmPortAllowed("m.corp", c.other) {
				t.Errorf("%s: port %d IS allowed; the entry named only %d", c.entry, c.other, c.want)
			}
		}
	})

	t.Run("two entries for one host collapse to the last", func(t *testing.T) {
		p := mk("m.corp:443", "m.corp:8443")
		if !p.mitmPortAllowed("m.corp", 8443) {
			t.Error("the LAST authored port must be the one that survives")
		}
		if p.mitmPortAllowed("m.corp", 443) {
			t.Error("mitmPorts now keys on host:port — B10-F9's re-keying landed; " +
				"delete this pin and assert both ports instead")
		}
	})

	// The end-to-end consequence of the collapse: the losing port is an ordinary
	// opaque tunnel, so the operator's token is never even offered on it.
	t.Run("the losing port falls through opaque", func(t *testing.T) {
		certPEM, keyPEM := genTestCA(t)
		ca, err := newCertAuthority(certPEM, keyPEM)
		if err != nil {
			t.Fatalf("newCertAuthority: %v", err)
		}
		p := newProxy(Options{
			RunID:     uuid.New(),
			Policy:    CompilePolicy(types.RunPolicySpec{AllowedDomains: []string{"m.corp:443", "m.corp:8443"}}),
			Sink:      &decisionSink{out: &bytes.Buffer{}, ch: make(chan egress.DecisionLog, 8)},
			CA:        ca,
			MITMHosts: []string{"m.corp:443", "m.corp:8443"},
			Resolver:  publicResolver{},
			Dial:      redirectDial(startEcho(t)),
		})
		proxySrv := httptest.NewServer(p)
		defer proxySrv.Close()

		conn, status := connectThrough(t, proxySrv.URL, "m.corp:443")
		defer conn.Close()
		if !strings.Contains(status, "200") {
			t.Fatalf("a policy-allowed host must still tunnel: %q", status)
		}
		_, _ = io.WriteString(conn, "ping")
		buf := make([]byte, 4)
		_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
		n, rerr := conn.Read(buf)
		if rerr != nil || string(buf[:n]) != "ping" {
			t.Fatalf("expected an opaque passthrough echo of %q, got %q err=%v", "ping", buf[:n], rerr)
		}
	})
}
