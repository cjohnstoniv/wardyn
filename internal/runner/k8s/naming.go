// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"sort"
	"strings"

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
// selectors can find them by run and component. extra is applied FIRST and
// the three reserved keys are stamped LAST (M3 finding): extra is
// caller-supplied (ultimately from policy/dispatch, e.g. an operator- or
// agent-provided label), and applying it last would let an entry silently
// override wardyn.component — un-selecting the agent from its own
// NetworkPolicy (whose selector is built from this same map). Every extra
// VALUE is also sanitized to legal k8s label syntax (L4 finding): a
// free-form value (e.g. a run's agent name) that fails k8s's
// `[A-Za-z0-9]([-A-Za-z0-9_.]*[A-Za-z0-9])?`, <=63-char syntax would 422 the
// WHOLE object create otherwise — an unsanitizable value is omitted
// entirely rather than risk that; the reserved keys (never caller-supplied)
// are always present and authoritative regardless.
func wardynLabels(runID uuid.UUID, component string, extra map[string]string) map[string]string {
	l := make(map[string]string, len(extra)+3)
	for k, v := range extra {
		if sv, ok := sanitizeLabelValue(v); ok {
			l[k] = sv
		}
	}
	l[labelManaged] = "true"
	l[labelRun] = runID.String()
	l[labelComponent] = component
	return l
}

// sanitizeLabelValue coerces s into a legal k8s label value
// ([A-Za-z0-9]([-A-Za-z0-9_.]*[A-Za-z0-9])?, <=63 chars) or reports it
// cannot be (ok=false, meaning: omit the label rather than send an illegal
// value to the apiserver). Any character outside the legal alphabet becomes
// '-'; leading/trailing '-'/'_'/'.' are trimmed (a value must start and end
// alphanumeric); the result is capped at 63 chars, re-trimmed in case
// truncation landed on a separator.
func sanitizeLabelValue(s string) (string, bool) {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	v := strings.Trim(b.String(), "-_.")
	if len(v) > 63 {
		v = strings.Trim(v[:63], "-_.")
	}
	return v, v != ""
}

// baseSecurityContext is the hardening every container on this substrate
// gets regardless of identity: no privilege escalation, every Linux
// capability dropped, RuntimeDefault seccomp, runAsNonRoot. THREE hard API
// facts (see the A0 contract) make container-level (not merely pod-level)
// the only correct place for this: (1) Pod Security Standards admission
// checks EPHEMERAL containers' own securityContext, not just the pod's; (2)
// an ephemeral container's spec must therefore carry this in full, not
// inherit it; (3) one shared builder means the agent's ephemeral exec is
// never less hardened than its main container by accident.
//
// RunAsUser is deliberately NOT set here — see agentSecurityContext and
// restrictedSecurityContext, which is is the one field that has to split by
// container identity (H2 finding): RunAsNonRoot:true with a nil RunAsUser
// only passes kubelet admission when the IMAGE's own USER is already
// numeric. The wardyn-proxy image (proxy + canary containers) is, so it
// gets this as-is; the agent images are not (see agentSecurityContext).
func baseSecurityContext() *corev1.SecurityContext {
	return &corev1.SecurityContext{
		RunAsNonRoot:             boolPtr(true),
		AllowPrivilegeEscalation: boolPtr(false),
		Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
	}
}

// restrictedSecurityContext is baseSecurityContext for containers whose
// image already runs as a KNOWN NUMERIC non-root user, so the kubelet can
// verify RunAsNonRoot without an explicit RunAsUser: the proxy and canary
// containers both run the wardyn-proxy image, built FROM
// gcr.io/distroless/static-debian12:nonroot (deploy/compose/Dockerfile.proxy)
// — numeric uid 65532, Google's documented distroless nonroot convention.
func restrictedSecurityContext() *corev1.SecurityContext {
	return baseSecurityContext()
}

// agentSecurityContext is baseSecurityContext PLUS RunAsUser:1000, for the
// agent's containers (the main placeholder AND the ephemeral exec
// container). Every wardyn agent image documents `USER agent` — a NAME, not
// a number (deploy/images/{claude-code,codex-cli,oracle,aws-sso,full}/Dockerfile
// all carry the "USER agent (uid 1000), home /home/agent" contract comment,
// and each creates that user via adduser/useradd -u 1000). RunAsNonRoot:true
// with a NIL RunAsUser fails Pod Security admission here: the kubelet can
// only verify non-root against a NUMERIC uid, and a name-form USER is
// opaque to it at admission time — H2 finding, product-breaking (every
// agent sandbox would 422 at pod create without this). RunAsUser:1000 is
// what makes RunAsNonRoot admission-checkable instead of a hard failure.
func agentSecurityContext() *corev1.SecurityContext {
	sc := baseSecurityContext()
	sc.RunAsUser = int64Ptr(1000)
	return sc
}

func boolPtr(b bool) *bool    { return &b }
func int64Ptr(i int64) *int64 { return &i }

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
