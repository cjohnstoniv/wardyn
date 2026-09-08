// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net"
	"strconv"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestAuthorBedrockBearerInjection_MITMEntryIsPortScoped is the pin for F037.
//
// A MITM-eligibility entry is ANY-PORT when it carries no ":port" suffix:
// proxy.parseMITMHostPort returns port 0 for a bare host, and handleConnect
// then matches with `cport == 0 || cport == port`. planArtifactRedirect has
// authored net.JoinHostPort(host, port) since W13-S1-5 for exactly that reason
// (and internal/egress/proxy/mitm_test.go's
// TestMITMCorpHost_PortMismatchFallsThroughOpaque proves a port-scoped entry
// falls through opaque on any other port). The Bedrock bearer lane, a LIVE
// caller, still authored a bare host — so an agent that could reach the
// Bedrock host at all could CONNECT to it on a port nobody configured, have
// that tunnel TLS-terminated with the Wardyn leaf, and have the OPERATOR's
// Bearer injected onto whatever answered there.
//
// The pin holds the property (the entry is port-scoped, at the port the run
// actually reaches), not a literal, so a future endpoint knob cannot re-widen
// it by accident.
func TestAuthorBedrockBearerInjection_MITMEntryIsPortScoped(t *testing.T) {
	for _, tc := range []struct {
		name     string
		baseURL  string
		wantHost string
		wantPort int
	}{
		{
			name:     "regional public endpoint defaults to 443",
			baseURL:  "",
			wantHost: bedrockRuntimeHost("us-east-1"),
			wantPort: 443,
		},
		{
			name:     "PrivateLink override with no port is 443",
			baseURL:  bedrockOverrideBaseURL,
			wantHost: bedrockOverrideHost,
			wantPort: 443,
		},
		{
			name:     "PrivateLink override on an explicit port carries THAT port",
			baseURL:  "https://" + bedrockOverrideHost + ":8443",
			wantHost: bedrockOverrideHost,
			wantPort: 8443,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := fullyConfiguredBedrockServer()
			s.cfg.BedrockBaseURL = tc.baseURL
			s.cfg.Secrets.(*memSecrets).m[bedrockAPIKeySecret] = []byte("bedrock-bearer-token-xyz")
			s.cfg.Store = vetoGrantStore{}
			ba := s.resolveBedrockAuth(context.Background(), "claude-code", false, true /* modelRun */, nil)
			if !ba.ready || !ba.bearer {
				t.Fatalf("ready=%v bearer=%v, want both true (bearer secret present)", ba.ready, ba.bearer)
			}
			_, mitmHosts, ok := s.authorBedrockBearerInjection(context.Background(),
				types.AgentRun{ID: uuid.New()}, llmTransport{bedrock: ba}, nil)
			if !ok {
				t.Fatal("authorBedrockBearerInjection failed; want ok")
			}
			if len(mitmHosts) != 1 {
				t.Fatalf("MITM entries = %v, want exactly 1", mitmHosts)
			}
			host, portStr, err := net.SplitHostPort(mitmHosts[0])
			if err != nil {
				t.Fatalf("MITM entry %q carries NO port: a bare entry is any-port in the proxy "+
					"(parseMITMHostPort -> 0, handleConnect's `cport == 0 || cport == port`), so a CONNECT to "+
					"the Bedrock host on a port nobody configured would be TLS-terminated with the Wardyn leaf "+
					"and the OPERATOR's Bearer injected onto whatever answered there: %v", mitmHosts[0], err)
			}
			port, perr := strconv.Atoi(portStr)
			if perr != nil || port <= 0 || port > 65535 {
				t.Fatalf("MITM entry %q has an unusable port %q — the proxy treats that as ABSENT, i.e. any-port",
					mitmHosts[0], portStr)
			}
			if host != tc.wantHost {
				t.Errorf("MITM host = %q, want %q", host, tc.wantHost)
			}
			if port != tc.wantPort {
				t.Errorf("MITM port = %d, want %d (the port this run's data plane is actually reached on)", port, tc.wantPort)
			}
			// The injection SCOPE must stay a BARE host — buildInjector requires it.
			// Only the MITM-eligibility set carries the port.
			injections, _, _ := s.authorBedrockBearerInjection(context.Background(),
				types.AgentRun{ID: uuid.New()}, llmTransport{bedrock: ba}, nil)
			if len(injections) != 1 {
				t.Fatalf("injections = %d, want 1", len(injections))
			}
			if got := injections[0].Rule.Host; got != tc.wantHost {
				t.Errorf("injection scope host = %q, want the BARE host %q (buildInjector matches on bare host)", got, tc.wantHost)
			}
		})
	}
}
