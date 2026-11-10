/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// #162's frozen strings — approved in the console-0.8 mock round (surface 1)
// and copied here byte-for-byte per that review.
//
// Not in wardyn/copy.ts: that file is near its 1000-line cap with no
// allowlist entry (scripts/check-file-size.sh) — the same reason
// model-access-copy.ts got its own module.
//
// The banner bodies derive from the wording internal/api/setup_checks.go's
// k8sEgressContainmentCheck already uses for the same three verdicts
// (enforced/acknowledged/unenforced) and the same indeterminate case — restated
// for an anonymous, pre-auth audience rather than the operator-only setup
// checklist.
export const POSTURE_UNENFORCED_BANNER = {
  TITLE: "Runs on this cluster are not network-confined.",
  BODY: "The boot-time canary proved this cluster's CNI does not enforce NetworkPolicy, and Wardyn was started with that risk accepted. Every sandbox it creates has unrestricted egress.",
  ACTION: "What to fix",
};

export const POSTURE_ACKNOWLEDGED_BANNER = {
  TITLE: "Network confinement is acknowledged, not proven.",
  BODY: "An admin confirmed this cluster applies its own default-deny network policy. Wardyn's boot-time canary could not test it, so nothing here proves a sandbox's egress is blocked.",
  ACTION: "How to prove it",
};

// Ruling 2 (issue #162, mock-approval comment): unknown/skew WARNS, it does
// not stay silent. This is the k8s-daemon-that-could-not-report case — see
// confinement-posture.tsx's resolveConfinementPosture for the runner+
// network_policy table that reaches it.
export const POSTURE_UNKNOWN_BANNER = {
  TITLE: "Wardyn can't confirm network confinement on this cluster.",
  BODY: "This install reports a Kubernetes runner but not its network-policy result, so egress containment can't be confirmed. Check the daemon's boot log for the egress-canary result.",
  ACTION: "Where to look",
};

// After a failed re-check of the canary (K-1, PS-302 and PS-306), under the
// existing danger and unknown titles. {time} is the check that produced the
// verdict, "2006-01-02 15:04 UTC". The not-proven body names no cause: three
// checks that could not tell and an acknowledged result in agent-sandbox mode
// both read not_proven.
export const POSTURE_REFUSED_BANNER = {
  TITLE: POSTURE_UNENFORCED_BANNER.TITLE,
  BODY: (time: string) =>
    `Not enforcing. The check at ${time} showed that this cluster no longer blocks egress with a deny-all NetworkPolicy, so new sandboxes are refused. Runs already going keep going.`,
  ACTION: POSTURE_UNENFORCED_BANNER.ACTION,
};

export const POSTURE_NOT_PROVEN_BANNER = {
  TITLE: POSTURE_UNKNOWN_BANNER.TITLE,
  BODY: "Not proven. The canary has not proved that this cluster blocks egress with a deny-all NetworkPolicy. New sandboxes in affected namespaces are refused until a check passes.",
  ACTION: POSTURE_UNENFORCED_BANNER.ACTION,
};

// The same bands when /healthz's verdict is a connected Kubernetes runner's
// (network_policy_source "runner"), not this control plane's own cluster's.
// No action: the setup page shows this control plane's own verdict, not the
// runner's.
export const POSTURE_RUNNER_UNENFORCED_BANNER = {
  TITLE: "Runs on a connected Kubernetes runner are not network-confined.",
  BODY: "That runner's canary proved its cluster's CNI does not enforce NetworkPolicy, and it was started with that risk accepted. New sandboxes in affected namespaces can start without proven network confinement.",
};

export const POSTURE_RUNNER_ACKNOWLEDGED_BANNER = {
  TITLE: POSTURE_ACKNOWLEDGED_BANNER.TITLE,
  BODY: "An admin confirmed a connected Kubernetes runner's cluster applies its own default-deny network policy. Its canary could not test it, so nothing there proves a sandbox's egress is blocked.",
};

export const POSTURE_RUNNER_REFUSED_BANNER = {
  TITLE: POSTURE_RUNNER_UNENFORCED_BANNER.TITLE,
  BODY: (time: string) =>
    `Not enforcing. The check at ${time} showed that a connected Kubernetes runner's cluster no longer blocks egress with a deny-all NetworkPolicy, so new sandboxes in affected namespaces are refused. Runs already going keep going.`,
};

export const POSTURE_RUNNER_NOT_PROVEN_BANNER = {
  TITLE: "Wardyn can't confirm network confinement on a connected Kubernetes runner.",
  BODY: "Not proven. The canary has not proved that a connected Kubernetes runner's cluster blocks egress with a deny-all NetworkPolicy. New sandboxes in affected namespaces are refused until a check passes.",
};

// Appended to ConfinementChip's `title` only — never to its visible label or
// accessible name, which stay Fence/Wall/Vault (D4, primitives.tsx#ConfinementChip).
export const POSTURE_CHIP_SUFFIX_UNENFORCED = " · network confinement is not enforced on this cluster";
export const POSTURE_CHIP_SUFFIX_ACKNOWLEDGED = " · network confinement is acknowledged, not proven";
export const POSTURE_CHIP_SUFFIX_NOT_PROVEN = " · network confinement is not proven on this cluster";
export const POSTURE_CHIP_SUFFIX_RUNNER_UNENFORCED = " · network confinement is not enforced on a connected Kubernetes runner";
export const POSTURE_CHIP_SUFFIX_RUNNER_NOT_PROVEN = " · network confinement is not proven on a connected Kubernetes runner";
