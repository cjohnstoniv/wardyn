// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"sort"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

const driverName = "k8s"

// Label vocabulary: THE SAME keys/values the docker driver uses
// (internal/runner/docker/naming.go's labelRun/labelComponent/labelManaged),
// re-declared here (build tags keep the two packages from importing one
// another) so an operator's audit/teardown selector ("wardyn.run-id=<uuid>")
// finds a run's resources identically on either substrate. All three are
// already legal k8s label keys (no "/" prefix; the bare name segment allows
// dots) and every value used (a UUID string, "agent"/"proxy"/"canary", "true")
// is a legal k8s label value.
const (
	labelRun       = "wardyn.run-id"
	labelComponent = "wardyn.component" // "agent" | "proxy" | "canary"
	labelManaged   = "wardyn.managed"   // "true"

	componentAgent  = "agent"
	componentProxy  = "proxy"
	componentCanary = "canary"
)

// In-pod container names (static; distinct from the pod-level names below,
// which embed the run id and become the sandbox ref).
const (
	// mainContainerName is the agent pod's pre-created placeholder container
	// (the idle main process CreateSandbox starts). Exec targets it via an
	// ephemeral container instead of running inside it, but Attach/ExecStream
	// fall back to it before any Exec has run.
	mainContainerName = "agent"
	// execContainerName is the ephemeral container Exec adds — fixed per the
	// A0 contract ("Exec (agent launch) = ephemeral container named
	// \"wardyn-agent\"").
	execContainerName = "wardyn-agent"
	// proxyContainerName is the proxy pod's sole container.
	proxyContainerName = "wardyn-proxy"
	// canaryContainerName is the boot-time egress canary's sole container.
	canaryContainerName = "canary"
)

// Pod/Secret/NetworkPolicy names are deterministic per run (mirrors docker's
// naming.go doc: a crashed control plane can reconstruct every name from the
// run UUID alone). A raw uuid.String() is lowercase hex+hyphens, which is
// already a legal (<=63 char) DNS-1123 label, so every name below stays under
// the k8s length ceiling with room to spare.
func agentPodName(runID uuid.UUID) string    { return "wardyn-agent-" + runID.String() }
func proxyPodName(runID uuid.UUID) string    { return "wardyn-proxy-" + runID.String() }
func secretName(runID uuid.UUID) string      { return "wardyn-proxy-cfg-" + runID.String() }
func agentNetPolName(runID uuid.UUID) string { return "wardyn-agent-netpol-" + runID.String() }
func proxyNetPolName(runID uuid.UUID) string { return "wardyn-proxy-netpol-" + runID.String() }

// wardynLabels stamps every Wardyn-owned object so audit and teardown
// selectors can find them by run and component. Mirrors docker's
// wardynLabels exactly (same keys, same shape).
func wardynLabels(runID uuid.UUID, component string, extra map[string]string) map[string]string {
	l := map[string]string{
		labelManaged:   "true",
		labelRun:       runID.String(),
		labelComponent: component,
	}
	for k, v := range extra {
		l[k] = v
	}
	return l
}

// restrictedSecurityContext is Wardyn's non-negotiable per-container hardening
// on k8s, reused for the agent's main + ephemeral (exec) containers, the proxy
// container, and the egress canary: runAsNonRoot, no privilege escalation,
// every Linux capability dropped, RuntimeDefault seccomp. THREE hard API
// facts (see the A0 contract) make container-level (not merely pod-level) the
// only correct place for this: (1) Pod Security Standards admission checks
// EPHEMERAL containers' own securityContext, not just the pod's; (2) an
// ephemeral container's spec must therefore carry this in full, not inherit
// it; (3) reusing one builder everywhere means the agent's ephemeral exec is
// never less hardened than its main container by accident.
func restrictedSecurityContext() *corev1.SecurityContext {
	return &corev1.SecurityContext{
		RunAsNonRoot:             boolPtr(true),
		AllowPrivilegeEscalation: boolPtr(false),
		Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
	}
}

func boolPtr(b bool) *bool { return &b }

// resourceRequirements maps runner.Resources onto a k8s ResourceRequirements
// with requests == limits (a hard cap, not a burst-friendly range — matches
// the docker driver's "every sandbox is capped" posture), applying the same
// conservative platform defaults docker uses for any zero field so a policy
// that sets nothing still gets a real cap. DiskMiB and PidsLimit have NO k8s
// Pod-API equivalent (there is no per-container "max pids" ResourceName, and
// ephemeral-storage enforcement is a separate, kubelet-eviction-manager-gated
// mechanism this lane does not wire up) — both are silently-nothing risks, so
// callers must warn rather than claim a cap that was dropped (see
// warnUnenforceableResources).
func resourceRequirements(res runner.Resources) corev1.ResourceRequirements {
	cpuMillis := res.CPUMillis
	if cpuMillis <= 0 {
		cpuMillis = runner.DefaultCPUMillis
	}
	memMiB := res.MemoryMiB
	if memMiB <= 0 {
		memMiB = runner.DefaultMemoryMiB
	}
	list := corev1.ResourceList{
		corev1.ResourceCPU:    *resource.NewMilliQuantity(cpuMillis, resource.DecimalSI),
		corev1.ResourceMemory: *resource.NewQuantity(memMiB*1024*1024, resource.BinarySI),
	}
	return corev1.ResourceRequirements{Requests: list, Limits: list}
}

// proxyResourcesMilliCPU/proxyResourcesMemoryMiB are the wardyn-proxy
// sidecar's fixed cgroup envelope — mirrors docker's proxyResources: the
// proxy only relays HTTP, so a tight, run-independent footprint leaves ample
// headroom while still bounding a compromised proxy.
const (
	proxyResourcesMilliCPU  int64 = 500
	proxyResourcesMemoryMiB int64 = 256
)

func proxyResources() corev1.ResourceRequirements {
	list := corev1.ResourceList{
		corev1.ResourceCPU:    *resource.NewMilliQuantity(proxyResourcesMilliCPU, resource.DecimalSI),
		corev1.ResourceMemory: *resource.NewQuantity(proxyResourcesMemoryMiB*1024*1024, resource.BinarySI),
	}
	return corev1.ResourceRequirements{Requests: list, Limits: list}
}

// envVars converts a non-secret env map (runner.SandboxSpec.Env) to k8s
// EnvVar form, mirroring docker's envSlice. Secrets never pass here
// (invariant 1) — the spec contract forbids it. Sorted by key for a
// deterministic pod spec (map iteration order is not).
func envVars(env map[string]string) []corev1.EnvVar {
	if len(env) == 0 {
		return nil
	}
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]corev1.EnvVar, 0, len(env))
	for _, k := range keys {
		out = append(out, corev1.EnvVar{Name: k, Value: env[k]})
	}
	return out
}
