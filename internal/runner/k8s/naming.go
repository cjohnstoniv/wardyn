// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

const driverName = "k8s"

// Label vocabulary: the SAME keys/values the docker driver uses, re-declared here since build tags
// keep the two packages from importing one another — so an operator's audit/teardown selector finds
// a run identically on either substrate.
const (
	labelRun       = "wardyn.run-id"
	labelComponent = "wardyn.component" // "agent" | "proxy" | "canary"
	labelManaged   = "wardyn.managed"   // "true"

	componentAgent  = "agent"
	componentProxy  = "proxy"
	componentCanary = "canary"
)

// In-pod container names (static; distinct from the pod-level names below, which embed the run id
// and become the sandbox ref).
const (
	// mainContainerName is the agent pod's pre-created placeholder; Exec targets an ephemeral
	// container instead, but Attach/ExecStream fall back to this one before any Exec has run.
	mainContainerName = "agent"
	// execContainerName is the ephemeral container Exec adds, fixed per the A0 contract.
	execContainerName = "wardyn-agent"
	// proxyContainerName is the proxy pod's main container (beside its stage-proxy-config init container).
	proxyContainerName  = "wardyn-proxy"
	canaryContainerName = "canary" // the boot-time egress canary's sole container
)

// Pod/Secret/NetworkPolicy names are deterministic per run (a crashed control plane can reconstruct
// every name from the run UUID alone). A raw uuid.String() is already a legal (<=63 char) DNS-1123 label.
const agentPodNamePrefix = "wardyn-agent-"

func agentPodName(runID uuid.UUID) string { return agentPodNamePrefix + runID.String() }

// runIDFromAgentPodName recovers the run UUID from a deterministic agent pod name: a sandbox ref IS
// the agent pod name, so teardown can recover the run id from the ref alone once the pod and its
// labels are gone. A name that isn't one of ours errors, keeping a ghost ref idempotent.
func runIDFromAgentPodName(name string) (uuid.UUID, error) {
	if !strings.HasPrefix(name, agentPodNamePrefix) {
		return uuid.Nil, fmt.Errorf("%q is not a wardyn agent pod name", name)
	}
	return uuid.Parse(strings.TrimPrefix(name, agentPodNamePrefix))
}
func proxyPodName(runID uuid.UUID) string    { return "wardyn-proxy-" + runID.String() }
func secretName(runID uuid.UUID) string      { return "wardyn-proxy-cfg-" + runID.String() }
func agentNetPolName(runID uuid.UUID) string { return "wardyn-agent-netpol-" + runID.String() }
func proxyNetPolName(runID uuid.UUID) string { return "wardyn-proxy-netpol-" + runID.String() }

// wardynLabels stamps every Wardyn-owned object so audit and teardown selectors can find them by run
// and component. extra is applied FIRST and the three reserved keys LAST, so a caller-supplied entry
// can't silently override wardyn.component (un-selecting the agent from its own NetworkPolicy).
// Every extra VALUE is sanitized to legal k8s label syntax; an unsanitizable value is omitted
// entirely rather than risk a 422 on the whole object create.
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

// sanitizeLabelValue coerces s into a legal k8s label value ([A-Za-z0-9]([-A-Za-z0-9_.]*[A-Za-z0-9])?,
// <=63 chars) or reports it cannot be (ok=false: omit the label rather than send an illegal value to
// the apiserver). Any character outside the legal alphabet becomes '-'; leading/trailing '-'/'_'/'.'
// are trimmed; the result is capped at 63 chars, re-trimmed in case truncation landed on a separator.
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

// baseSecurityContext is the hardening every container on this substrate gets regardless of
// identity: no privilege escalation, every Linux capability dropped, RuntimeDefault seccomp,
// runAsNonRoot. Container-level (not pod-level) per the A0 contract, since Pod Security Standards
// admission checks an EPHEMERAL container's own securityContext — one shared builder means the
// agent's ephemeral exec is never less hardened than its main container.
//
// RunAsUser is deliberately NOT set here — see agentSecurityContext/restrictedSecurityContext, the
// field that splits by container identity: RunAsNonRoot:true with a nil RunAsUser only passes
// kubelet admission when the image's own USER is already numeric.
func baseSecurityContext() *corev1.SecurityContext {
	return &corev1.SecurityContext{
		RunAsNonRoot:             boolPtr(true),
		AllowPrivilegeEscalation: boolPtr(false),
		Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
	}
}

// restrictedSecurityContext is baseSecurityContext for containers whose image already runs as a
// KNOWN NUMERIC non-root user (proxy/canary run wardyn-proxy, built from
// gcr.io/distroless/static-debian12:nonroot — uid 65532), so the kubelet can verify RunAsNonRoot
// without an explicit RunAsUser.
func restrictedSecurityContext() *corev1.SecurityContext {
	return baseSecurityContext()
}

// agentSecurityContext is baseSecurityContext PLUS RunAsUser:1000, for the agent's containers. Every
// wardyn agent image documents `USER agent` — a NAME, not a number — so without RunAsUser:1000 every
// agent sandbox would 422 at pod create (RunAsNonRoot can't verify a named user).
func agentSecurityContext() *corev1.SecurityContext {
	sc := baseSecurityContext()
	sc.RunAsUser = int64Ptr(1000)
	return sc
}

func boolPtr(b bool) *bool    { return &b }
func int64Ptr(i int64) *int64 { return &i }

// ephemeralStorageRequestFloorMiB is the ephemeral-storage REQUEST the agent container carries
// whenever a limit is set. Small, fixed, and NEVER the limit: k8s copies a limit into the request
// when none is set, so an absent request would hand the scheduler the org's whole ceiling and leave
// the pod silently Pending. 256Mi fits any node that can pull the agent image; a limit smaller than the floor uses the limit instead.
const ephemeralStorageRequestFloorMiB int64 = 256

// resourceRequirements maps runner.Resources onto a k8s ResourceRequirements. CPU and memory limits are
// the hard cap (matching the docker driver's "every sandbox is capped" posture), with the same
// conservative platform defaults docker uses for any zero field. Requests equal the limits unless
// the deployment sets a request ratio (runner.EffectiveRequests), which makes the pod Burstable.
// The proxy sidecar never takes the ratio (proxyResources): it stays Guaranteed.
//
// DiskMiB is limits[ephemeral-storage] plus an explicit small request (ephemeralStorageRequestFloorMiB)
// — asymmetric on purpose, see that const. It covers the pod's writable layers + logs + local-ephemeral
// volumes, but not a drive PVC or the ephemeral container's own writable layer (see
// ephemeralScratchVolumes). Enforcement is eviction, not a quota. DiskMiB==0 leaves both keys absent.
//
// PidsLimit still has no k8s Pod-API equivalent, so it remains a silently-nothing risk and
// CreateSandbox warns rather than claiming a cap that was dropped.
func resourceRequirements(res runner.Resources) corev1.ResourceRequirements {
	sz := runner.EffectiveResources(res)
	list := func(cpu, mem int64) corev1.ResourceList { // two lists, not one aliased into both: a shared map would put the limit in the requests too
		return corev1.ResourceList{
			corev1.ResourceCPU:    *resource.NewMilliQuantity(cpu, resource.DecimalSI),
			corev1.ResourceMemory: *resource.NewQuantity(mem*1024*1024, resource.BinarySI),
		}
	}
	requests := list(sz.AgentCPURequestMillis, sz.AgentMemoryRequestMiB)
	limits := list(sz.AgentCPULimitMillis, sz.AgentMemoryLimitMiB)
	if res.DiskMiB > 0 {
		limits[corev1.ResourceEphemeralStorage] = *resource.NewQuantity(res.DiskMiB*1024*1024, resource.BinarySI)
		floor := min(res.DiskMiB, ephemeralStorageRequestFloorMiB)
		requests[corev1.ResourceEphemeralStorage] = *resource.NewQuantity(floor*1024*1024, resource.BinarySI)
	}
	return corev1.ResourceRequirements{Requests: requests, Limits: limits}
}

// The three scratch volumes and where they are mounted: /tmp, the workdir agent-run cds into, and
// /home/agent/.cache — the toolchain cache root runs_dispatch_mounts' env points into, so a build's
// cache writes land inside disk_mib instead of on the ephemeral container's unmetered writable layer
// (the gap issue #164 closes). THREE PATHS, NOT "where the agent writes": an authored target may sit
// anywhere under /home/agent, and GOPATH, ~/.cache/pip and /opt/rust are NOT covered.
//
// NOT /home/agent ITSELF, which would cover them all: a volume there would shadow the baked
// .bashrc, collide with the reserved drive target, and hide the read-only ~/.claude bind the
// subscription path mounts. The three leaf paths are what can be mounted safely today.
const (
	scratchTmpVolumeName   = "wardyn-tmp"
	scratchWorkVolumeName  = "wardyn-work"
	scratchCacheVolumeName = "wardyn-cache"
	scratchTmpPath         = runner.ScratchTmpPath
	scratchWorkPath        = runner.ScratchWorkPath
	// scratchCachePath is also the path the full image's /etc/profile.d/toolchains.sh
	// unconditionally re-exports GOCACHE/GOTMPDIR/GOMODCACHE under, updated alongside this volume so
	// a login shell doesn't stomp dispatch's env-only relocation back to the old out-of-budget paths.
	scratchCachePath = runner.ScratchCachePath
)

// ephemeralScratchVolumes is what brings the agent's /tmp, workdir and toolchain-cache writes inside
// disk_mib on this substrate; without it the ephemeral-storage limit binds only the idle main
// container's writable layer.
//
// The gap it NARROWS: the agent works in an EPHEMERAL container Exec adds, and the kubelet does not
// meter an ephemeral container's writable layer at all, so an ordinary `dd` there fills the node
// without eviction. An emptyDir is metered as the POD's ephemeral storage regardless of which
// container writes into it, and Exec copies the main container's VolumeMounts verbatim onto the
// ephemeral one, so mounting here reaches the agent by construction.
//
// WHOLE-VOLUME MOUNTS, NEVER SubPath: corev1.VolumeMount forbids subpath mounts on ephemeral
// containers, and Exec copies these mounts verbatim, so a SubPath here would fail
// UpdateEphemeralContainers for every autonomous run with disk_mib set.
//
// THREE SizeLimits and the container limit are not a quadruple budget: the kubelet evicts on
// whichever binds first, and emptyDir usage also counts toward the pod's ephemeral-storage total, so
// the three volumes together still can't exceed limits[ephemeral-storage]. Zero disk_mib adds
// nothing, matching resourceRequirements' "absent means unbounded" shape.
//
// NARROWED, NOT CLOSED: everything the agent writes outside these three paths stays on the ephemeral
// container's own unmetered writable layer. readOnlyRootFilesystem would close it and is NOT set,
// since the agent legitimately writes those paths.
//
// scratchCachePath shadows the full image's pre-created /home/agent/.cache/go-build with an empty
// directory, so the Go build cache starts cold; the kubelet creates it root-owned but 0777, writable
// without FSGroup.
func ephemeralScratchVolumes(diskMiB int64) ([]corev1.Volume, []corev1.VolumeMount) {
	if diskMiB <= 0 {
		return nil, nil
	}
	targets := []struct{ name, path string }{
		{scratchTmpVolumeName, scratchTmpPath},
		{scratchWorkVolumeName, scratchWorkPath},
		{scratchCacheVolumeName, scratchCachePath},
	}
	vols := make([]corev1.Volume, 0, len(targets))
	mounts := make([]corev1.VolumeMount, 0, len(targets))
	for _, t := range targets {
		vols = append(vols, corev1.Volume{ // a fresh Quantity per volume: one shared pointer would alias two spec fields onto the same object
			Name: t.name,
			VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{
				SizeLimit: resource.NewQuantity(diskMiB*1024*1024, resource.BinarySI),
			}},
		})
		mounts = append(mounts, corev1.VolumeMount{Name: t.name, MountPath: t.path})
	}
	return vols, mounts
}

// proxyResources is the wardyn-proxy sidecar's cgroup envelope — the same runner.ProxyLimits
// docker applies: a tight, run-independent footprint bounding a compromised proxy.
func proxyResources(azure bool) corev1.ResourceRequirements {
	cpuMillis, memMiB := runner.ProxyLimitsFor(azure)
	list := corev1.ResourceList{
		corev1.ResourceCPU:    *resource.NewMilliQuantity(cpuMillis, resource.DecimalSI),
		corev1.ResourceMemory: *resource.NewQuantity(memMiB*1024*1024, resource.BinarySI),
	}
	return corev1.ResourceRequirements{Requests: list, Limits: list}
}

// envVars converts a NON-SECRET env map (runner.SandboxSpec.Env) to k8s EnvVar form: an inline
// Value, part of the Pod spec and therefore readable by anyone with pods/get in this namespace.
// SECURITY: credential material must never reach this function — it rides SecretEnv/secretEnvVars
// below. Sorted by key for a deterministic pod spec.
func envVars(env map[string]string) []corev1.EnvVar {
	if len(env) == 0 {
		return nil
	}
	keys := slices.Sorted(maps.Keys(env))
	out := make([]corev1.EnvVar, 0, len(env))
	for _, k := range keys {
		out = append(out, corev1.EnvVar{Name: k, Value: env[k]})
	}
	return out
}

// secretEnvDataKey is the per-run Secret data key one SecretEnv variable's value is stored under.
// Prefixed to avoid colliding with proxyConfigSecretKey; legal Secret data keys are a superset of
// what internal/api's validEnvVarName admits, so an illegal name fails the Secret create closed.
func secretEnvDataKey(name string) string { return "env." + name }

// secretEnvVars converts a CREDENTIAL-BEARING env map (SecretEnv) into EnvVars that carry only a
// REFERENCE to the per-run Secret. SECURITY: a secretKeyRef resolves in the kubelet, so the value
// never enters the Pod spec that pods/get returns. Sorted by key like envVars.
func secretEnvVars(runID uuid.UUID, secretEnv map[string]string) []corev1.EnvVar {
	if len(secretEnv) == 0 {
		return nil
	}
	keys := slices.Sorted(maps.Keys(secretEnv))
	out := make([]corev1.EnvVar, 0, len(keys))
	for _, k := range keys {
		out = append(out, corev1.EnvVar{Name: k, ValueFrom: &corev1.EnvVarSource{
			SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: secretName(runID)},
				Key:                  secretEnvDataKey(k),
			},
		}})
	}
	return out
}
