// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package conformance_test

// conformance_k8s_test.go runs the conformance suite against the k8s driver,
// DIRECTLY against whatever kubeconfig context is current — no helm chart, no
// Postgres (mirrors conformance_docker_test.go's harness shape). Guarded by
// WARDYN_TEST_K8S=1; skipped cleanly when unset, so it never requires a live
// cluster in unit-CI.
//
// The //go:build k8s tag ensures this file only compiles under -tags k8s
// (matching the k8s driver's own build tag). ci.yml's conformance-k8s job
// stands up a kind cluster with Calico (NetworkPolicy enforcement — the
// substrate's boot-time canary REFUSES to construct without it — see
// internal/runner/k8s/canary.go) and runs `make test-conformance-k8s`.
//
// Agent image: busybox (idle + sh/cat/nc cover everything the suite needs).
// Proxy image: the REAL wardyn-proxy build, via WARDYN_PROXY_IMAGE — the
// construction-time canary launches it with -egress-canary instead of its
// normal entrypoint args, so a busybox stand-in would make the gate
// vacuously refuse (ImagePullBackOff/exec-format-error, never a
// NetworkPolicy verdict) rather than actually proving anything.

import (
	"context"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runner/k8s"
	"github.com/cjohnstoniv/wardyn/internal/runner/orchestrator"
	"github.com/cjohnstoniv/wardyn/test/conformance"
)

func TestConformanceK8s(t *testing.T) {
	if os.Getenv("WARDYN_TEST_K8S") != "1" {
		t.Skip("WARDYN_TEST_K8S=1 not set; skipping k8s conformance")
	}

	proxyImage := os.Getenv("WARDYN_PROXY_IMAGE")
	if proxyImage == "" {
		t.Fatal("WARDYN_PROXY_IMAGE must name the REAL wardyn-proxy build (the boot-time canary launches it with -egress-canary; a stand-in image makes the gate meaningless — see this file's doc comment)")
	}

	// Namespace: the substrate's own real config knob (WARDYN_K8S_NAMESPACE),
	// resolved the same way register.go would — empty => k8s.New's own
	// "default" fallback (Config.withDefaults). ci.yml sets it to the
	// PSS-restricted namespace it labels.
	sub, err := k8s.New(k8s.Config{
		Namespace:  os.Getenv("WARDYN_K8S_NAMESPACE"),
		ProxyImage: proxyImage,
		// ConfinementRuntimes deliberately nil: this suite proves CC1 (the
		// unconditional floor); CC2/CC3 need a RuntimeClass pin this
		// throwaway conformance cluster does not provision (see A1's
		// Config.ConfinementRuntimes doc — a RuntimeClass name carries no
		// guessable platform convention).
	})
	if err != nil {
		// Construction itself IS the positive canary-direction proof: k8s.New
		// refuses to boot unless the boot-time egress canary proves this
		// cluster's CNI enforces NetworkPolicy (see internal/runner/k8s's
		// package doc). The negative direction (a kindnet-only cluster
		// correctly REFUSING) is proven out-of-band against a second
		// throwaway cluster — see the lane's report for that run's output;
		// internal/runner/k8s/canary_test.go already unit-tests both verdict
		// paths against a fake clientset.
		t.Fatalf("k8s.New: %v (this cluster's CNI must enforce NetworkPolicy — see ci.yml's Calico step)", err)
	}
	// Exercise the assembled production path: the orchestrator over the k8s
	// substrate is what the control plane actually runs.
	r := orchestrator.New(sub)

	conformance.Run(t, r, conformance.Options{
		SandboxImage: "busybox:latest",
		Timeout:      3 * time.Minute,
		// busybox's sh can exit with an explicit code; the Wait conformance
		// case Execs this and asserts Wait returns the same code.
		ExitArgv: func(code int) []string {
			return []string{"sh", "-c", "exit " + strconv.Itoa(code)}
		},
		// DefaultRouteProbe: nil (deliberately). This substrate declares
		// StructuralEgress:false — it proves L1 (NetworkPolicy), never L0
		// (see internal/runner/k8s's package doc) — so conformance.go's own
		// testL0StructuralEgress self-skips: there is no default route to
		// probe FOR. testAgentCannotReachAPIServer below is the L1
		// replacement.
	})

	t.Run("AgentCannotReachAPIServer", func(t *testing.T) {
		testAgentCannotReachAPIServer(t, r)
	})
}

// testAgentCannotReachAPIServer is the k8s-local L1 replacement for the L0
// default-route probe: the agent pod's NetworkPolicy (internal/runner/k8s's
// CreateSandbox) allows egress ONLY to the proxy sidecar on
// runner.ProxyListenPort — no route to the Kubernetes API server. Every pod
// (regardless of ServiceAccount token automount — that is a separate
// mechanism) gets KUBERNETES_SERVICE_HOST/KUBERNETES_SERVICE_PORT env vars
// from the kubelet, so this needs no DNS (the agent's DNSPolicy is
// deliberately None). busybox's `nc -z` (zero-I/O connect probe, no data
// phase) must fail: exit 0 would mean the agent reached the apiserver
// directly, an L1 confinement breach.
func testAgentCannotReachAPIServer(t *testing.T, r runner.Runner) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	caps, err := r.Capabilities(ctx)
	if err != nil {
		t.Fatalf("Capabilities: %v", err)
	}
	if len(caps.ConfinementClasses) == 0 {
		t.Skip("no ConfinementClasses declared; cannot create a sandbox")
	}

	spec := runner.SandboxSpec{
		RunID:            uuid.New(),
		Image:            "busybox:latest",
		ConfinementClass: caps.ConfinementClasses[len(caps.ConfinementClasses)-1],
		Labels:           map[string]string{"wardyn.conformance": "true"},
	}
	sb, err := r.CreateSandbox(ctx, spec)
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	defer func() { _ = r.StopSandbox(context.Background(), sb.Ref) }()

	const probe = `nc -z -w2 "$KUBERNETES_SERVICE_HOST" "$KUBERNETES_SERVICE_PORT"`
	if _, err := r.Exec(ctx, sb.Ref, []string{"sh", "-c", probe}); err != nil {
		t.Fatalf("Exec(apiserver probe): %v", err)
	}
	code, err := r.Wait(ctx, sb.Ref)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if code == 0 {
		t.Error("agent pod reached the Kubernetes API server (KUBERNETES_SERVICE_HOST:PORT) directly — L1 confinement breach: the agent NetworkPolicy must allow ONLY the proxy sidecar")
	}
}
