/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Canonical, HONEST confinement metadata — the single source of truth for both
// the ConfinementChip (primitives.tsx) and the New Run confinement step
// (step-confinement.tsx), so the wording can't drift apart.
//
// CC1/CC2/CC3 is the internal WIRE value (mirrors internal/types/types.go's
// ConfinementClass). Users see the friendly display label — Fence / Wall / Vault
// — a single "how separated is the agent from your machine" ladder; the wire
// code shows only in the tooltip for operators who know it.
//
// The class selects ONLY the isolation SUBSTRATE — credential brokering, egress
// filtering, and HITL approvals are governed by policy, independent of the class
// (see CONFINEMENT_CONSTANT_NOTE).
import type { ConfinementClass } from "../../lib/types";

export interface CCMeta {
  /** Friendly display label shown to users. The CC wire code shows only in the
   *  New Run confinement step's tooltip; the Getting-started matrix deliberately
   *  prints it beside the label as a column sub-header (design decision). */
  label: string;
  /** Short "what it is / strength / availability" tagline. */
  tagline: string;
  /** Plain "what this barrier protects you from". */
  protects: string;
  /** Honest "what it does NOT protect from" — the residual risk. */
  doesntProtect: string;
  /** The plain Fence/Wall/Vault metaphor — holes / over-under / all-sides. */
  metaphor: string;
  /** The precise substrate mechanism — used as the honest tooltip body. */
  mechanism: string;
}

// Canonical weakest -> strongest ladder — the single place the Fence < Wall <
// Vault order is spelled out. The barrier picker, the New Run confinement step,
// and default-confinement's strongest-available scan all consume this.
export const CC_ORDER: ConfinementClass[] = ["CC1", "CC2", "CC3"];

// Ordered weakest -> strongest. The Fence/Wall/Vault metaphor: a fence has holes
// (shares your kernel), a wall closes the holes (gVisor seals the kernel path)
// but you could go over/under, a vault covers all sides (its own hardware VM).
export const CC_META: Record<ConfinementClass, CCMeta> = {
  CC1: {
    label: "Fence",
    tagline: "Weakest · runs anywhere",
    protects:
      "The agent seeing or touching the rest of your machine — it gets its own files, processes, and network.",
    doesntProtect:
      "A kernel exploit or container escape — it shares your machine's kernel (the “holes” in the fence).",
    metaphor:
      "A fence keeps the agent in, but it has holes. It shares your machine's kernel, so a kernel-level escape can slip through the gaps.",
    mechanism:
      "Shared-kernel container (runc) hardened with user namespaces, seccomp, AppArmor, and capability drop.",
  },
  CC2: {
    label: "Wall",
    tagline: "Default · runs anywhere Docker does",
    protects:
      "Everything Fence does, plus most kernel exploits — a software kernel (gVisor) handles the agent's system calls so it never touches your real kernel (the holes are closed).",
    doesntProtect:
      "A flaw in the sandbox software itself (rare); it's still not a fully separate machine.",
    metaphor:
      "A wall closes the holes — gVisor handles every system call, so nothing the agent does touches your kernel. But it's still software, not a separate machine, so a flaw in the wall is the way over or under.",
    mechanism:
      "gVisor userspace kernel intercepts syscalls — the default; runs anywhere Docker runs.",
  },
  CC3: {
    label: "Vault",
    tagline: "Strongest · needs KVM hardware",
    protects:
      "Everything Wall does, plus a full break-in staying trapped — its own hardware-walled VM with its own kernel; even total takeover inside stays inside.",
    doesntProtect:
      "A flaw in the virtualization layer itself — a very rare hypervisor- or CPU-level escape.",
    metaphor:
      "A vault covers every side — its own hardware-walled VM with its own kernel. Even a full break-in stays sealed inside.",
    mechanism:
      "Kata microVM — hardware-virtualized, with its own guest kernel (requires /dev/kvm).",
  },
};

// Applies to EVERY tier — the barrier only sets isolation strength; the other
// protections (egress, credentials, approvals, audit) are policy-driven and
// constant across classes. Shown once, near the tier selector / proposal.
export const CONFINEMENT_CONSTANT_NOTE =
  "Whatever the barrier, every run still gets Wardyn's egress filtering, short-lived brokered credentials, human approvals, and full audit — those are set by policy, not the barrier. The barrier only decides how strongly the sandbox is walled off from your machine.";

// Back-compat: the honest mechanism sentence keyed by class. The ConfinementChip
// tooltip, the step-confinement hints, and setup-screen's fix copy read this, so
// the substrate wording stays in exactly one place.
export const CC_HINTS: Record<ConfinementClass, string> = {
  CC1: CC_META.CC1.mechanism,
  CC2: CC_META.CC2.mechanism,
  CC3: CC_META.CC3.mechanism,
};

// ── Tier comparison matrix (E1) ─────────────────────────────────────────────
// The pricing-table view (tiers as columns, protections as rows). The ONLY new
// tier metadata — no parallel store: each row just GRADES the tiers; the actual
// wording (protects / doesntProtect) still lives in CC_META above. Three states,
// rendered with the same CircleCheck/CircleAlert/CircleX + success/warning/danger
// trio the audit-outcome badge uses (primitives.tsx):
//   "yes"    — fully protected at this tier
//   "caveat" — protected EXCEPT for a named residual risk; the cell tooltip reuses
//              RESIDUAL_PREFIX + CC_META[cc].doesntProtect verbatim (zero new copy)
//   "no"     — not protected at this tier
export type CCMark = "yes" | "caveat" | "no";

export interface CCMatrixRow {
  /** Plain "what it protects" phrasing — never names a wire code or mechanism. */
  label: string;
  /** Per-tier grade for this protection. */
  cells: Record<ConfinementClass, CCMark>;
}

// Weakest→strongest hardening ladder. CC2 is "caveat" (not "no") on the full-
// break-in row: a software kernel contains a full compromise ABSENT a sandbox-
// software flaw, so it's a qualified yes — the tooltip names the residual risk
// (a sandbox flaw for CC2 vs a hypervisor flaw for CC3, straight from doesntProtect).
export const CC_MATRIX_ROWS: CCMatrixRow[] = [
  {
    label: "Isolated from your files, processes, and network",
    cells: { CC1: "yes", CC2: "yes", CC3: "yes" },
  },
  {
    label: "A kernel exploit can't reach your host kernel",
    cells: { CC1: "no", CC2: "caveat", CC3: "yes" },
  },
  {
    label: "A full break-in stays sealed inside",
    cells: { CC1: "no", CC2: "caveat", CC3: "caveat" },
  },
];

// Separate "where it runs" row-group — plain TEXT cells, not a protection grade,
// so it's a distinct structure (not a CCMark row). "Needs KVM hardware" is the
// one approved place a substrate constraint shows as visible copy.
export const CC_MATRIX_WHERE: { label: string; cells: Record<ConfinementClass, string> } = {
  label: "Where it runs",
  cells: { CC1: "Any host", CC2: "Any Docker host", CC3: "Needs KVM hardware" },
};
