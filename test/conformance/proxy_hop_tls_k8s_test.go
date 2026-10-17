// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package conformance_test

// proxy_hop_tls_k8s_test.go is TestProxyHopTLSDocker's k8s counterpart (#692):
// the REAL wardyn-proxy image, built into the k8s substrate, dials a stand-in
// control plane (test/conformance/hopfake) running as an in-cluster pod behind
// a same-named Service. A proxy pinned to the right CA resolves its credential
// grant over TLS; the same sealed config pinned to a different CA never reaches
// the control plane. The wrong-CA half is only meaningful beside the right-CA
// half in the same test, which proves the fake is reachable and the proxy does
// resolve at startup.

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/hoptls"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runner/k8s"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func TestProxyHopTLSK8s(t *testing.T) {
	if os.Getenv("WARDYN_TEST_K8S") != "1" {
		t.Skip("WARDYN_TEST_K8S=1 not set; skipping the k8s control-plane TLS hop case")
	}
	proxyImage := os.Getenv("WARDYN_PROXY_IMAGE")
	if proxyImage == "" {
		t.Fatal("WARDYN_PROXY_IMAGE must name the REAL wardyn-proxy build (see conformance_k8s_test.go's doc comment)")
	}
	agentImage := os.Getenv("WARDYN_TEST_K8S_AGENT_IMAGE")
	if agentImage == "" {
		t.Fatal("WARDYN_TEST_K8S_AGENT_IMAGE must name an image built from deploy/kind/Dockerfile.conformance-agent")
	}
	ns := os.Getenv("WARDYN_K8S_NAMESPACE")
	if ns == "" {
		ns = "default"
	}
	restCfg, cs := k8sTestClient(t)
	sub, err := k8s.New(k8s.Config{Namespace: ns, ProxyImage: proxyImage})
	if err != nil {
		t.Fatalf("k8s.New: %v (this cluster's CNI must enforce NetworkPolicy — see ci.yml's Calico step)", err)
	}

	newCA := func() *hoptls.CA {
		blob, err := hoptls.NewCA(time.Now())
		if err != nil {
			t.Fatal(err)
		}
		ca, err := hoptls.ParseCA(blob)
		if err != nil {
			t.Fatal(err)
		}
		return ca
	}
	ca, other := newCA(), newCA()
	host := "wardyn-hopfake-" + uuid.NewString()[:8]
	grant, token := uuid.New(), "hop-tls-run-token-k8s"
	startHopFakeK8s(t, restCfg, cs, ns, agentImage, ca, host, grant, token)

	spec := func(caPEM string) runner.SandboxSpec {
		return runner.SandboxSpec{
			RunID:            uuid.New(),
			Image:            agentImage,
			ConfinementClass: types.CC1,
			Labels:           map[string]string{"wardyn.conformance": "true"},
			Resources:        runner.Resources{CPUMillis: 250, MemoryMiB: 128},
			ProxyConfig: runner.ProxyConfig{
				RunToken:          token,
				ControlPlaneURL:   "https://" + host + ":8443",
				ControlPlaneCAPEM: caPEM,
				Policy:            types.RunPolicySpec{AllowedDomains: []string{"api.example.com"}, MinConfinementClass: types.CC1},
				Injection:         []runner.InjectionGrant{{GrantID: grant, Rule: egress.InjectionRule{Host: "api.example.com", Header: "X-Api-Key"}}},
			},
		}
	}
	resolves := func() int { return strings.Count(hopfakePodLogs(cs, ns, host), "hopfake: resolve grant=") }

	t.Run("pinned CA resolves over TLS", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
		defer cancel()
		sb, err := sub.CreateSandbox(ctx, spec(string(ca.CertPEM)))
		if err != nil {
			t.Fatalf("CreateSandbox: %v", err)
		}
		t.Cleanup(func() { _ = sub.KillSandbox(context.Background(), sb.Ref) })
		want := "hopfake: resolve grant=" + grant.String() + " tls=true"
		logs := waitForHopfakeLog(t, cs, ns, host, want, 90*time.Second)
		t.Logf("control plane saw:\n%s", logs)
	})

	t.Run("wrong CA never reaches the control plane", func(t *testing.T) {
		before := resolves()
		if before == 0 {
			t.Fatal("no resolve recorded by the pinned-CA subtest: the fake or the proxy's startup resolve is not working, so this subtest would pass vacuously")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
		defer cancel()
		sb, err := sub.CreateSandbox(ctx, spec(string(other.CertPEM)))
		if err == nil {
			t.Cleanup(func() { _ = sub.KillSandbox(context.Background(), sb.Ref) })
		}
		time.Sleep(10 * time.Second) // past the proxy's startup resolve and its first restart
		if after := resolves(); after != before {
			t.Fatalf("a resolve reached the control plane through a proxy pinned to the wrong CA (%d -> %d)", before, after)
		}
	})
}
