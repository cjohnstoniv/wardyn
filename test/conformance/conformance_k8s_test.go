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
// Agent image: WARDYN_TEST_K8S_AGENT_IMAGE — must be built from
// deploy/kind/Dockerfile.conformance-agent (busybox:1.36 + a real wardyn-rec
// binary; `make build-conformance-agent-image`), NOT bare busybox. k8s's
// SessionRecording is unconditionally true (exec.go's recordCmd has no
// docker-style opt-out) — a bare busybox agent image fails EVERY Exec closed
// (no wardyn-rec on PATH), so this suite would never reach a real verdict
// with one; review round 2 (H1) caught a first version of this file that
// silently took that failure as "conformance passed" via a since-reverted
// fallback in recordCmd itself. busybox:1.36 (not :latest): a `:latest` tag
// defaults to imagePullPolicy Always, which defeats `kind load` — the
// kubelet re-pulls over the network on every run regardless (M6).
// Proxy image: the REAL wardyn-proxy build, via WARDYN_PROXY_IMAGE — the
// construction-time canary launches it with -egress-canary instead of its
// normal entrypoint args, so a stand-in would make the gate vacuously
// refuse (ImagePullBackOff/exec-format-error, never a NetworkPolicy verdict)
// rather than actually proving anything.

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
	agentImage := os.Getenv("WARDYN_TEST_K8S_AGENT_IMAGE")
	if agentImage == "" {
		t.Fatal("WARDYN_TEST_K8S_AGENT_IMAGE must name an image built from deploy/kind/Dockerfile.conformance-agent (`make build-conformance-agent-image`) — a recorder-less image (e.g. bare busybox) fails every Exec closed under this substrate's unconditional SessionRecording, which would make the gate meaningless; see this file's doc comment")
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
		SandboxImage: agentImage,
		Timeout:      3 * time.Minute,
		// The conformance-agent image's sh can exit with an explicit code;
		// the Wait conformance case Execs this and asserts Wait returns the
		// same code. It runs through recordCmd's wardyn-rec wrapper like
		// every other Exec on this substrate — see the file doc comment.
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
		testAgentCannotReachAPIServer(t, r, agentImage)
	})
}

// apiServerProbeScript is a CODED probe (review round 2, H2): the original
// version asserted only "exit code != 0 means confined", which reads
// nc-missing (a broken image), KUBERNETES_SERVICE_HOST/PORT unset (a broken
// env), and a broken exec ALL as "confined" — none of them prove anything.
// Distinct exit codes separate "the probe could not run" from "the probe
// ran and found a breach":
//
//	90  nc is not on PATH                          — environment, not a verdict
//	91  KUBERNETES_SERVICE_HOST/PORT unset          — environment, not a verdict
//	92  the proxy sidecar (the ONE allowed peer) is unreachable — environment,
//	    not a verdict, but also a POSITIVE CONTROL: if the one peer the agent
//	    NetworkPolicy is supposed to allow can't be reached either, the probe
//	    proves nothing about confinement either way
//	93  the apiserver WAS reached                   — an actual L1 breach
//	0   the apiserver was NOT reached, everything else ran fine — confined
const apiServerProbeScript = `command -v nc >/dev/null 2>&1 || exit 90
[ -n "$KUBERNETES_SERVICE_HOST" ] && [ -n "$KUBERNETES_SERVICE_PORT" ] || exit 91
nc -z -w2 wardyn-proxy 3128 || exit 92
nc -z -w2 "$KUBERNETES_SERVICE_HOST" "$KUBERNETES_SERVICE_PORT" && exit 93
exit 0`

// testAgentCannotReachAPIServer is the k8s-local L1 replacement for the L0
// default-route probe: the agent pod's NetworkPolicy (internal/runner/k8s's
// CreateSandbox) allows egress ONLY to the proxy sidecar on
// runner.ProxyListenPort — no route to the Kubernetes API server. Every pod
// (regardless of ServiceAccount token automount — that is a separate
// mechanism) gets KUBERNETES_SERVICE_HOST/KUBERNETES_SERVICE_PORT env vars
// from the kubelet, so this needs no DNS (the agent's DNSPolicy is
// deliberately None). See apiServerProbeScript's doc for the exit-code
// contract this asserts against.
func testAgentCannotReachAPIServer(t *testing.T, r runner.Runner, agentImage string) {
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
		Image:            agentImage,
		ConfinementClass: caps.ConfinementClasses[len(caps.ConfinementClasses)-1],
		Labels:           map[string]string{"wardyn.conformance": "true"},
	}
	sb, err := r.CreateSandbox(ctx, spec)
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	defer func() { _ = r.StopSandbox(context.Background(), sb.Ref) }()

	if _, err := r.Exec(ctx, sb.Ref, []string{"sh", "-c", apiServerProbeScript}); err != nil {
		t.Fatalf("Exec(apiserver probe): %v", err)
	}
	code, err := r.Wait(ctx, sb.Ref)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	switch code {
	case 0:
		// confined: the proxy sidecar (the one allowed peer) was reachable,
		// the apiserver was not.
	case 90, 91, 92:
		t.Fatalf("apiserver probe could not run (exit %d) — an environment problem, not a confinement verdict either way: nc missing (90), KUBERNETES_SERVICE_HOST/PORT unset (91), or the proxy sidecar on 3128 unreachable (92 — the positive control: if the one allowed peer can't be reached, the probe proves nothing)", code)
	case 93:
		t.Error("agent pod reached the Kubernetes API server (KUBERNETES_SERVICE_HOST:PORT) directly — L1 confinement breach: the agent NetworkPolicy must allow ONLY the proxy sidecar")
	default:
		t.Fatalf("apiserver probe exited %d, a code the script never emits — investigate before trusting this result either way", code)
	}
}
