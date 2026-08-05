// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import "errors"

// errProxyImageUnset is returned when a Driver is asked to build the egress
// sidecar (and run the boot-time canary, which reuses the same image) but no
// proxy image was configured. Mirrors docker's errProxyImageUnset.
var errProxyImageUnset = errors.New("k8s: wardyn-proxy image not configured")

// errRuntimeClassUnavailable is the fail-closed sentinel for Confinement-Class
// gating: the policy (or WARDYN_CONFINEMENT_MAP) demanded a class whose
// RuntimeClass is not registered in the cluster, or whose .Handler fails the
// same floor guard docker applies. Wrapped (%w) so callers can errors.Is on it
// and refuse the run rather than silently downgrade (invariant 5). Mirrors
// docker's errRuntimeUnavailable.
var errRuntimeClassUnavailable = errors.New("required confinement RuntimeClass unavailable")

// errCanaryIndeterminate is returned by the constructor when the boot-time
// egress canary could not distinguish "NetworkPolicy enforced" from "not
// enforced" — a non-network failure (ImagePullBackOff, scheduling failure, a
// crash before Running) in either canary phase. The substrate refuses to boot
// rather than guess: see the package doc's two-phase canary contract.
var errCanaryIndeterminate = errors.New("k8s: egress canary indeterminate (non-network failure, not a NetworkPolicy verdict)")

// errNetworkPolicyUnenforced is returned by the constructor when Phase B (the
// deny-all NetworkPolicy) failed to block the canary's connect — the cluster's
// CNI does not enforce NetworkPolicy. WARDYN_K8S_ALLOW_UNENFORCED_NETPOL=1
// downgrades this to a loud warning instead of refusing boot; either way the
// substrate advertises NetworkPolicy=false (never overclaims).
var errNetworkPolicyUnenforced = errors.New("k8s: NetworkPolicy is not enforced on this cluster (deny-all canary connected anyway)")

// errMountsUnsupported is CreateSandbox's preflight rejection of any
// spec.Mounts entry — the single chokepoint that makes host-mount paths fail
// closed on k8s (no host filesystem is available to a pod the way a docker
// bind mount reaches the daemon host).
var errMountsUnsupported = errors.New("k8s: host bind mounts are not supported on this substrate; use a repo workspace or proxy-side credential injection instead")

// errSecondExec is returned when Exec is called twice against the same ref.
// Kubernetes ephemeral containers are ADD-ONLY (a pod's ephemeral-container
// list can only grow), so a second Exec cannot be honoured the way docker's
// "latest Exec wins" re-exec is: never silently no-op, never return the prior
// exec's id (see substrate.Substrate.Exec's doc).
var errSecondExec = errors.New("k8s: exec: this sandbox already has an agent exec; a substrate second Exec on the same ref is not supported (ephemeral containers are add-only)")

// errBYOIUnsupported is Exec's refusal of a wardyn-byoi/ image ref before any
// ephemeral container is created. BYOI's selftest-then-task double-exec
// (internal/api's byoiSelftest + startAgentOrIdle, "latest Exec wins" on
// docker) is impossible on k8s for the same add-only reason as errSecondExec;
// refusing on the FIRST Exec call (rather than letting the selftest succeed
// and only the second Exec fail) gives a clear, specific error instead of a
// generic once-per-ref one.
var errBYOIUnsupported = errors.New("k8s: BYOI (wardyn-byoi/ images) is docker-only and is refused on this substrate: ephemeral containers cannot honour the selftest-then-task double-exec")

// errTeardownUnresolved is returned when teardown found the ref'd pod but its
// wardyn.run-id label was missing or unparseable, so the sibling proxy pod,
// NetworkPolicies, and Secret (all selected by that label) cannot be located.
// Surfaced instead of reporting a false success — mirrors docker's identical
// fail-closed sentinel.
var errTeardownUnresolved = errors.New("k8s: teardown could not resolve run id from the sandbox's wardyn.run-id label; sibling proxy/netpols/secret may be orphaned")
