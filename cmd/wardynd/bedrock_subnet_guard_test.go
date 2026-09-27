// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net"
	"net/netip"
	"strings"
	"testing"
)

// TestRefuseBedrockHostOnSubnets (#1198): the pure decision behind
// refuseBedrockOnProxySubnet — given a resolved subnet list, an address
// inside one is refused naming the variable/address/subnet, an address
// outside every subnet is fine, and an IP-literal host needs no resolver at
// all.
func TestRefuseBedrockHostOnSubnets(t *testing.T) {
	cpNet := netip.MustParsePrefix("172.30.0.0/16")

	for name, c := range map[string]struct {
		host       string
		resolved   []net.IP
		resolveErr error
		wantErr    bool
		wantMsg    []string // substrings the error must contain
	}{
		"IP literal inside the subnet is refused": {
			host:    "172.30.5.5",
			wantErr: true,
			wantMsg: []string{"172.30.5.5", `"wardyn-internal"`, "172.30.0.0/16", "never lifts its own subnet"},
		},
		"IP literal outside the subnet is fine": {
			host: "10.0.0.5",
		},
		"resolved hostname inside the subnet is refused": {
			host:     "bedrock-vpce.example.com",
			resolved: []net.IP{net.ParseIP("172.30.9.9")},
			wantErr:  true,
			wantMsg:  []string{"172.30.9.9"},
		},
		"resolved hostname outside the subnet is fine": {
			host:     "bedrock-vpce.example.com",
			resolved: []net.IP{net.ParseIP("100.64.1.1")},
		},
		"unresolvable hostname WARNs and proceeds": {
			host:       "does-not-resolve.example.com",
			resolveErr: errors.New("no such host"),
		},
	} {
		t.Run(name, func(t *testing.T) {
			prevResolve := resolveBedrockHost
			resolveBedrockHost = func(host string) ([]net.IP, error) { return c.resolved, c.resolveErr }
			t.Cleanup(func() { resolveBedrockHost = prevResolve })

			var buf bytes.Buffer
			prev := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
			t.Cleanup(func() { slog.SetDefault(prev) })

			err := refuseBedrockHostOnSubnets("https://"+c.host, c.host, "wardyn-internal", []netip.Prefix{cpNet})

			if (err != nil) != c.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, c.wantErr)
			}
			for _, want := range c.wantMsg {
				if err == nil || !strings.Contains(err.Error(), want) {
					t.Errorf("error %v does not contain %q", err, want)
				}
			}
			if c.resolveErr != nil && !strings.Contains(buf.String(), "could not resolve") {
				t.Errorf("expected a WARN naming the unresolvable host; log = %s", buf.String())
			}
		})
	}
}

// TestRefuseBedrockOnProxySubnet_Skips (#1198): the substrate-dispatch wrapper
// never even looks at a substrate when there is nothing configured to check,
// and it WARNs (never refuses) on the substrates whose subnet it cannot know
// from cmd/wardynd's own boot alone.
func TestRefuseBedrockOnProxySubnet_Skips(t *testing.T) {
	for name, c := range map[string]struct {
		bedrockBaseURL string
		runnerTarget   string
		wantWarn       string
	}{
		"empty base URL: no-op, no warning": {
			bedrockBaseURL: "",
			runnerTarget:   "docker",
		},
		"none runner: nothing dispatches a proxy, no warning": {
			bedrockBaseURL: "https://vpce-bedrock.example.com",
			runnerTarget:   "none",
		},
		"k8s runner: documented gap, WARNs": {
			bedrockBaseURL: "https://vpce-bedrock.example.com",
			runnerTarget:   "k8s",
			wantWarn:       "pod's CIDR is not known",
		},
		"docker runner with no discoverable network: WARNs unverified": {
			bedrockBaseURL: "https://vpce-bedrock.example.com",
			runnerTarget:   "docker",
			wantWarn:       "UNVERIFIED",
		},
	} {
		t.Run(name, func(t *testing.T) {
			var buf bytes.Buffer
			prev := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
			t.Cleanup(func() { slog.SetDefault(prev) })

			// controlPlaneNetworkSubnets is the tagless (bedrock_subnet_nodocker.go)
			// stub in this build: it always reports "unknown", exercising exactly
			// the WARN-and-proceed path a real daemon that can't reach docker would
			// also take.
			err := refuseBedrockOnProxySubnet(context.Background(), c.bedrockBaseURL, c.runnerTarget)
			if err != nil {
				t.Fatalf("refuseBedrockOnProxySubnet: unexpected refusal: %v", err)
			}
			if c.wantWarn != "" && !strings.Contains(buf.String(), c.wantWarn) {
				t.Errorf("expected log to contain %q; log = %s", c.wantWarn, buf.String())
			}
			if c.wantWarn == "" && buf.Len() != 0 {
				t.Errorf("expected no log output; got %s", buf.String())
			}
		})
	}
}

// TestBedrockBaseURLHost covers the host-extraction helper directly, since
// refuseBedrockOnProxySubnet silently no-ops on a parse failure (unreachable
// via a validated WARDYN_BEDROCK_BASE_URL, but worth pinning on its own).
func TestBedrockBaseURLHost(t *testing.T) {
	if h, err := bedrockBaseURLHost("https://vpce-bedrock.example.com/prefix"); err != nil || h != "vpce-bedrock.example.com" {
		t.Errorf("got (%q, %v)", h, err)
	}
	if _, err := bedrockBaseURLHost("https:///no-host"); err == nil {
		t.Error("expected an error for an empty host")
	}
}
