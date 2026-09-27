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
const agentPodNamePrefix = "wardyn-agent-"

func agentPodName(runID uuid.UUID) string { return agentPodNamePrefix + runID.String() }

// runIDFromAgentPodName recovers the run UUID from a deterministic agent pod
// name — the mirror of docker/naming.go's runIDFromAgentName, and for the same
// reason: a sandbox ref IS the agent pod name, so teardown can recover the run
// id from the ref alone when the pod itself is already gone and there is no
// label left to read. A name that is not one of ours (or carries no parseable
// uuid) errors, which is what keeps a ghost ref idempotent.
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

// wardynLabels stamps every Wardyn-owned object so audit and teardown
// selectors can find them by run and component. extra is applied FIRST and
// the three reserved keys are stamped LAST: extra is
// caller-supplied (ultimately from policy/dispatch, e.g. an operator- or
// agent-provided label), and applying it last would let an entry silently
// override wardyn.component — un-selecting the agent from its own
// NetworkPolicy (whose selector is built from this same map). Every extra
// VALUE is also sanitized to legal k8s label syntax: a
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
// container identity: RunAsNonRoot:true with a nil RunAsUser
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
// opaque to it at admission time — product-breaking (every
// agent sandbox would 422 at pod create without this). RunAsUser:1000 is
// what makes RunAsNonRoot admission-checkable instead of a hard failure.
func agentSecurityContext() *corev1.SecurityContext {
	sc := baseSecurityContext()
	sc.RunAsUser = int64Ptr(1000)
	return sc
}

func boolPtr(b bool) *bool    { return &b }
func int64Ptr(i int64) *int64 { return &i }

// ephemeralStorageRequestFloorMiB is the ephemeral-storage REQUEST the agent
// container carries whenever a limit is set. Small, fixed, and NEVER the limit:
// Kubernetes copies a limit into the request when no request is set for that key,
// so an ABSENT request would hand the scheduler the org's whole ceiling and leave
// the pod silently Pending on "Insufficient ephemeral-storage" — a scheduler
// event statusFromPod's waitingDetail never surfaces. 256Mi fits any node that
// can pull the agent image at all. A limit SMALLER than the floor uses the limit
// instead (a request may not exceed its own limit).
const ephemeralStorageRequestFloorMiB int64 = 256

// resourceRequirements maps runner.Resources onto a k8s ResourceRequirements.
//
// CPU and memory are requests == limits (a hard cap, not a burst-friendly range —
// matches the docker driver's "every sandbox is capped" posture), applying the
// same conservative platform defaults docker uses for any zero field so a policy
// that sets nothing still gets a real cap.
//
// DiskMiB is limits[ephemeral-storage] PLUS an explicit small request
// (ephemeralStorageRequestFloorMiB) — asymmetric on purpose, see that const. What
// the kubelet counts against it is the pod's container writable layers + logs +
// its local-ephemeral VOLUMES; a drive PVC is never counted against it. The
// writable layer of the EPHEMERAL container the agent runs in is counted by
// neither, which is why ephemeralScratchVolumes exists and what it does and does
// not reach is stated there. Enforcement is types.StorageEnforcementEviction, not
// a quota: the kubelet measures periodically and KILLS THE POD — it never refuses
// the write. DiskMiB == 0 leaves both keys absent (and adds no volumes).
//
// PidsLimit still has NO k8s Pod-API equivalent (there is no per-container "max
// pids" ResourceName), so that one remains a silently-nothing risk and
// CreateSandbox warns rather than claiming a cap that was dropped.
func resourceRequirements(res runner.Resources) corev1.ResourceRequirements {
	cpuMillis := res.CPUMillis
	if cpuMillis <= 0 {
		cpuMillis = runner.DefaultCPUMillis
	}
	memMiB := res.MemoryMiB
	if memMiB <= 0 {
		memMiB = runner.DefaultMemoryMiB
	}
	// Two lists, not one aliased into both: ephemeral-storage differs between
	// them, and a shared map would put the limit in the requests too.
	shared := func() corev1.ResourceList {
		return corev1.ResourceList{
			corev1.ResourceCPU:    *resource.NewMilliQuantity(cpuMillis, resource.DecimalSI),
			corev1.ResourceMemory: *resource.NewQuantity(memMiB*1024*1024, resource.BinarySI),
		}
	}
	requests, limits := shared(), shared()
	if res.DiskMiB > 0 {
		limits[corev1.ResourceEphemeralStorage] = *resource.NewQuantity(res.DiskMiB*1024*1024, resource.BinarySI)
		floor := min(res.DiskMiB, ephemeralStorageRequestFloorMiB)
		requests[corev1.ResourceEphemeralStorage] = *resource.NewQuantity(floor*1024*1024, resource.BinarySI)
	}
	return corev1.ResourceRequirements{Requests: requests, Limits: limits}
}

// The three scratch volumes and where they are mounted: /tmp (where
// runner.AgentIdleScript writes the per-run CA files, under /tmp/wardyn), the
// workdir agent-run cds into (the clone's DEFAULT destination), and
// /home/agent/.cache — the toolchain cache root runs_dispatch_mounts' env
// (GOCACHE, GOTMPDIR, GOMODCACHE, npm_config_cache) points into, so a build's
// Go and npm cache writes land inside disk_mib too instead of on the
// ephemeral container's unmetered writable layer (the gap issue #164 closes).
//
// THREE PATHS, NOT "where the agent writes". An authored target may legally
// sit at /work, /workspace or anywhere else under /home/agent
// (runner.ValidateAuthoredTarget's prefixes), and GOPATH itself (so the
// installed tool binaries under /home/agent/go/bin stay put — only
// GOMODCACHE, not GOPATH, moves), ~/.cache/pip and /opt/rust are NOT under
// these three volumes; see this function's doc for what that means for the
// budget.
//
// NOT /home/agent ITSELF, which would cover them all: a volume there would
// shadow the baked .bashrc every agent image ships (the attach hint), collide
// with the reserved drive target runner.DriveTarget under the same home, and
// hide the read-only ~/.claude bind the subscription path mounts. The three
// leaf paths are what can be mounted safely today; the home is not ours to
// replace.
const (
	scratchTmpVolumeName   = "wardyn-tmp"
	scratchWorkVolumeName  = "wardyn-work"
	scratchCacheVolumeName = "wardyn-cache"
	scratchTmpPath         = "/tmp"
	scratchWorkPath        = "/home/agent/work"
	// scratchCachePath is also the path the full image's
	// /etc/profile.d/toolchains.sh unconditionally re-exports GOCACHE/GOTMPDIR/
	// GOMODCACHE under (deploy/images/full/Dockerfile) — a login-shell task
	// runs `/bin/sh -lc`, which sources that profile AFTER dispatch's env and
	// would otherwise stomp an env-only relocation back to the old
	// out-of-budget paths. The profile was updated alongside this volume
	// (image rebuild required) rather than leaving the emptyDir at a path the
	// profile doesn't name, so both a plain and a login shell agree on where
	// the cache lives.
	scratchCachePath = "/home/agent/.cache"
)

// ephemeralScratchVolumes is what brings the agent's /tmp, workdir and
// toolchain-cache writes inside disk_mib on this substrate; without it the
// ephemeral-storage limit binds only the idle main container's writable
// layer.
//
// The gap it NARROWS (0.7.4's known gap (a)): the agent does its
// work in an EPHEMERAL container that Exec adds, and the kubelet does not meter
// an ephemeral container's writable layer at all — resourceRequirements' limit
// bounds a container nothing writes in, so an ordinary `dd` from the agent
// fills the node without the pod ever being evicted. An emptyDir is metered as the POD's local
// ephemeral storage no matter which container writes into it, and Exec copies
// the main container's VolumeMounts verbatim onto the ephemeral container, so
// mounting here reaches the agent by construction.
//
// WHOLE-VOLUME MOUNTS, NEVER SubPath. corev1.VolumeMount's own contract says
// subpath mounts are not allowed for ephemeral containers, and Exec copies these
// mounts VERBATIM: a SubPath here would make UpdateEphemeralContainers fail for
// every autonomous k8s run with disk_mib set, while a fake-clientset test went
// on passing. One volume per mount point is the only shape that survives the
// copy.
//
// THREE SizeLimits AND the container limit are not a quadruple budget: the
// kubelet evicts on whichever binds first, and it counts emptyDir usage toward
// the pod's ephemeral-storage total as well, so the three volumes together
// still cannot exceed resourceRequirements' limits[ephemeral-storage] =
// disk_mib. The per-volume SizeLimit is the tighter, earlier stop.
//
// Zero disk_mib adds nothing at all — the same "absent means unbounded" shape
// resourceRequirements uses, so a pod with no disk budget keeps the volume-less
// shape this substrate has always produced.
//
// NARROWED, NOT CLOSED. Everything the agent writes outside these three paths
// stays on the ephemeral container's own unmetered writable layer: /home/agent/go
// (the module cache moved out via GOMODCACHE, but the installed tool binaries
// under its bin/ have not — moving GOPATH would lose those), ~/.cache/pip,
// /opt/rust, the dotfiles (~/.wardyn, ~/.ssh, ~/.claude), and any authored
// workspace_repos or ephemeral-source target outside /home/agent/work.
// readOnlyRootFilesystem would close it and is NOT set, because the agent
// legitimately writes those paths.
//
// scratchCachePath shadows the full image's pre-created
// /home/agent/.cache/go-build (deploy/images/full/Dockerfile) with an empty
// directory, so the Go build cache starts cold. The mount itself is writable
// without FSGroup: the kubelet creates an emptyDir root-owned but 0777, the
// same mode the /tmp and /home/agent/work volumes have been written through by
// the uid-1000 agent since 0.7.5. test/conformance's ephemeralFillTargets (the
// "Cache" target) writes it against a real cluster, which a fake clientset
// cannot model.
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
		// A fresh Quantity per volume: one shared pointer would alias two
		// spec fields onto the same object.
		vols = append(vols, corev1.Volume{
			Name: t.name,
			VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{
				SizeLimit: resource.NewQuantity(diskMiB*1024*1024, resource.BinarySI),
			}},
		})
		mounts = append(mounts, corev1.VolumeMount{Name: t.name, MountPath: t.path})
	}
	return vols, mounts
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

// envVars converts a NON-SECRET env map (runner.SandboxSpec.Env) to k8s
// EnvVar form, mirroring docker's envSlice: an inline Value, which is part of
// the Pod spec and therefore readable by anyone with pods/get in this
// namespace. Credential material must never reach this function — it rides
// runner.SandboxSpec.SecretEnv and secretEnvVars below. Sorted by key for a
// deterministic pod spec (map iteration order is not).
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

// secretEnvDataKey is the per-run Secret data key one SecretEnv variable's
// value is stored under. Prefixed so it can never collide with
// proxyConfigSecretKey (the proxy config JSON sharing that Secret) whatever a
// future dispatch lane decides to name a variable. Legal Secret data keys are
// [-._a-zA-Z0-9]+, a superset of the [A-Z_][A-Z0-9_]* env-var names
// internal/api's validEnvVarName admits, so a name that reaches here is
// already a legal key — and one that somehow is not fails the Secret create
// closed rather than silently dropping a credential.
func secretEnvDataKey(name string) string { return "env." + name }

// secretEnvVars converts a CREDENTIAL-BEARING env map
// (runner.SandboxSpec.SecretEnv, which states why the spec splits the two) into
// EnvVars that carry only a REFERENCE to the per-run Secret. That is the whole
// difference from envVars above: a secretKeyRef resolves in the kubelet, so the
// value never enters the Pod spec that pods/get returns. The container still
// sees an ordinary environment variable under its own name, so nothing in the
// sandbox changes. Sorted by key for a deterministic pod spec, like envVars.
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
