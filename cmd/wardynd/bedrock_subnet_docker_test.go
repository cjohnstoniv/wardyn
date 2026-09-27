// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/netip"
	"os"
	"strings"
	"testing"

	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
)

type fakeNetworkInspector struct {
	res client.NetworkInspectResult
	err error
}

func (f fakeNetworkInspector) NetworkInspect(context.Context, string, client.NetworkInspectOptions) (client.NetworkInspectResult, error) {
	return f.res, f.err
}

// TestControlPlaneNetworkSubnets (#1198): the docker-tagged half of the
// boot-time Bedrock/proxy-subnet check — a network with an IPAM subnet
// reports it, a daemon/lookup error or a network with no configured subnet
// reports unknown (ok=false) rather than guessing.
func TestControlPlaneNetworkSubnets(t *testing.T) {
	subnet := netip.MustParsePrefix("172.30.0.0/16")

	for name, c := range map[string]struct {
		fake        fakeNetworkInspector
		wantOK      bool
		wantSubnets []netip.Prefix
	}{
		"IPAM subnet present": {
			fake: fakeNetworkInspector{res: client.NetworkInspectResult{Network: network.Inspect{
				Network: network.Network{IPAM: network.IPAM{Config: []network.IPAMConfig{{Subnet: subnet}}}},
			}}},
			wantOK:      true,
			wantSubnets: []netip.Prefix{subnet},
		},
		"daemon error reports unknown": {
			fake:   fakeNetworkInspector{err: errors.New("no such network")},
			wantOK: false,
		},
		"no IPAM config reports unknown": {
			fake:   fakeNetworkInspector{res: client.NetworkInspectResult{}},
			wantOK: false,
		},
	} {
		t.Run(name, func(t *testing.T) {
			prev := newNetworkInspector
			newNetworkInspector = func() (networkInspector, error) { return c.fake, nil }
			t.Cleanup(func() { newNetworkInspector = prev })

			subnets, ok := controlPlaneNetworkSubnets(context.Background(), "wardyn-internal")
			if ok != c.wantOK {
				t.Fatalf("ok = %v, want %v (subnets = %v)", ok, c.wantOK, subnets)
			}
			if ok && (len(subnets) != len(c.wantSubnets) || subnets[0] != c.wantSubnets[0]) {
				t.Errorf("subnets = %v, want %v", subnets, c.wantSubnets)
			}
		})
	}
}

// TestControlPlaneNetworkSubnets_ClientConstructFailure: a docker client that
// cannot even be constructed (no daemon reachable at boot) reports unknown,
// never guesses.
func TestControlPlaneNetworkSubnets_ClientConstructFailure(t *testing.T) {
	prev := newNetworkInspector
	newNetworkInspector = func() (networkInspector, error) { return nil, errors.New("no daemon") }
	t.Cleanup(func() { newNetworkInspector = prev })

	if _, ok := controlPlaneNetworkSubnets(context.Background(), "wardyn-internal"); ok {
		t.Error("expected ok=false when the docker client cannot be constructed")
	}
}

// TestValidateModelEndpoints_RefusesBedrockOnProxySubnet (#1198 F2): the
// wiring from boot flags all the way to the refusal, not just the pure
// decision (TestRefuseBedrockAddrsOnSubnets) or the network lookup
// (TestControlPlaneNetworkSubnets) in isolation. Faking newNetworkInspector
// here — rather than a live daemon — is what makes this hermetic AND able to
// pin the exact subnet the resolved Bedrock address must land in.
func TestValidateModelEndpoints_RefusesBedrockOnProxySubnet(t *testing.T) {
	prev := newNetworkInspector
	newNetworkInspector = func() (networkInspector, error) {
		return fakeNetworkInspector{res: client.NetworkInspectResult{Network: network.Inspect{
			Network: network.Network{IPAM: network.IPAM{Config: []network.IPAMConfig{
				{Subnet: netip.MustParsePrefix("172.30.0.0/16")},
			}}},
		}}}, nil
	}
	t.Cleanup(func() { newNetworkInspector = prev })

	empty, region, model := "", "us-east-1", ""
	ack := false
	bedrockBaseURL := "https://172.30.5.5" // an IP literal: no DNS involved
	f := &bootFlags{
		anthropicBaseURL:       &empty,
		openaiBaseURL:          &empty,
		anthropicGatewayHeader: &empty,
		anthropicGatewayFormat: &empty,
		openaiGatewayHeader:    &empty,
		openaiGatewayFormat:    &empty,
		bedrockBaseURL:         &bedrockBaseURL,
		bedrockRegion:          &region,
		bedrockModel:           &model,
		allowTestEndpoints:     &ack,
		awsSSOEndpointOverride: &empty,
	}

	_, _, _, _, err := validateModelEndpoints(context.Background(), f, "docker")
	if err == nil {
		t.Fatal("validateModelEndpoints: got nil, want a refusal — the resolved address is inside the faked control-plane network's subnet")
	}
	for _, want := range []string{"WARDYN_BEDROCK_BASE_URL", "172.30.5.5", "172.30.0.0/16"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q does not name %q", err, want)
		}
	}
}

// TestRefuseBedrockOnProxySubnet_Docker_UnknownNetworkWarns (#1198 F3): the
// docker-runnerTarget branch of refuseBedrockOnProxySubnet, made hermetic
// under -tags docker by faking newNetworkInspector to report "no such
// network" — exactly what an unreachable/misconfigured daemon looks like —
// rather than relying on whatever this host's real default daemon happens to
// have. Companion to TestRefuseBedrockOnProxySubnet_Skips's tagless cases.
func TestRefuseBedrockOnProxySubnet_Docker_UnknownNetworkWarns(t *testing.T) {
	prev := newNetworkInspector
	newNetworkInspector = func() (networkInspector, error) {
		return fakeNetworkInspector{err: errors.New("no such network: wardyn-internal")}, nil
	}
	t.Cleanup(func() { newNetworkInspector = prev })

	var buf bytes.Buffer
	prevLog := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prevLog) })

	err := refuseBedrockOnProxySubnet(context.Background(), "https://172.20.1.1", "docker")
	if err != nil {
		t.Fatalf("refuseBedrockOnProxySubnet: unexpected refusal: %v", err)
	}
	if !strings.Contains(buf.String(), "UNVERIFIED") {
		t.Errorf("expected an UNVERIFIED WARN when the control-plane network can't be inspected; log = %s", buf.String())
	}
	// 172.20.1.1 is inside docker's built-in pools, so the pool WARN must fire too.
	if !strings.Contains(buf.String(), "default-address-pools") {
		t.Errorf("expected the default-address-pools WARN for an address in docker's built-in pools; log = %s", buf.String())
	}
}

// TestControlPlaneNetworkSubnets_RealDaemon (#1198, Q6): an optional
// real-daemon check — what a fake cannot show is whether the real client
// actually talks to a real docker socket. Skipped unless WARDYN_TEST_DOCKER=1
// (same gate as internal/runner/docker's *_live_test.go files); "bridge" is
// docker's own always-present default network, so this needs no fixture.
func TestControlPlaneNetworkSubnets_RealDaemon(t *testing.T) {
	if os.Getenv("WARDYN_TEST_DOCKER") != "1" {
		t.Skip("set WARDYN_TEST_DOCKER=1 to run the real-Docker control-plane-subnet test")
	}
	subnets, ok := controlPlaneNetworkSubnets(context.Background(), "bridge")
	if !ok {
		t.Fatal("controlPlaneNetworkSubnets(\"bridge\"): ok = false against a real daemon")
	}
	if len(subnets) == 0 || !subnets[0].IsValid() {
		t.Errorf("subnets = %v, want at least one valid prefix", subnets)
	}
}
