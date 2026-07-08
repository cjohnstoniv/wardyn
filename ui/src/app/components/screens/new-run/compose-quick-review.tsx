/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Tier 0 — "Quick Review": the CAN / CAN'T split at the heart of the composer's
// proposed-setup review. It is a PURE read-projection of the CLAMPED inline_policy
// (never the model's self-assessment): the LEFT column lists what the run CAN do —
// its granted capabilities, rendered AMBER because a grant is a caution, never a
// reassuring green check (honesty rule D2) — and the RIGHT column lists what it
// CAN'T, the confinement guarantees. Every line is derived from a field the risk
// grader already reads, so it is honest by construction.
//
// allow_all_egress is ALWAYS phrased with CAPABILITY.allowAllEgress ("can reach
// almost any site (except a block-list)"), never "unrestricted"; brokered grants
// carry CAPABILITY.brokerLine; a git_pat grant uses CAPABILITY.gitPatLine (the PAT
// is handed to git INSIDE the sandbox, so the agent's process really can read it —
// no reassuring broker claim).
import * as React from "react";
import {
  Clock,
  FilePenLine,
  FolderLock,
  GitBranch,
  Globe,
  KeyRound,
  Lock,
  ShieldCheck,
  ShieldX,
  Terminal,
  Unlock,
  WifiOff,
  type LucideIcon,
} from "lucide-react";
import { cn } from "../../ui/utils";
import { hostLabel } from "../../../lib/compose-stages";
import { CAPABILITY, RESIDUAL_PREFIX } from "../../wardyn/copy";
import { CC_META } from "../../wardyn/cc-meta";
import type { GrantSpec, RiskItem, RunPolicySpec } from "../../../lib/types";
import { firstUseRaisesApproval } from "../../../lib/types";

export interface CapLine {
  icon: LucideIcon;
  text: string;
}

export function ComposeQuickReview({ inline_policy }: { inline_policy: RunPolicySpec }) {
  const cans = React.useMemo(() => canLines(inline_policy), [inline_policy]);
  const cants = React.useMemo(() => cantLines(inline_policy), [inline_policy]);
  return (
    <div className="grid gap-3 sm:grid-cols-2" data-testid="quick-review">
      <CapColumn variant="can" title="This run can" HeadIcon={Unlock} lines={cans} />
      <CapColumn variant="cant" title="It can't" HeadIcon={ShieldCheck} lines={cants} />
    </div>
  );
}

function CapColumn({
  variant,
  title,
  HeadIcon,
  lines,
}: {
  variant: "can" | "cant";
  title: string;
  HeadIcon: LucideIcon;
  lines: CapLine[];
}) {
  const can = variant === "can";
  // Amber for capabilities (a grant is a caution, never a green check — D2); teal
  // for the confinement guarantees.
  const accent = can ? "text-warning" : "text-primary";
  return (
    <div
      className={cn(
        "rounded-xl border p-4",
        can ? "border-warning/30 bg-warning-subtle/40" : "border-border bg-muted/30",
      )}
    >
      <div className="mb-2.5 flex items-center gap-2">
        <HeadIcon className={cn("size-3.5", accent)} aria-hidden="true" />
        <h3 className={cn("text-[12.5px] font-semibold", accent)}>{title}</h3>
      </div>
      <ul className="space-y-2.5" aria-label={title}>
        {lines.map((l, i) => (
          <li key={i} className="flex gap-2 text-[12.5px] leading-snug text-foreground">
            <l.icon className={cn("mt-0.5 size-3.5 shrink-0", accent)} aria-hidden="true" />
            <span>{l.text}</span>
          </li>
        ))}
      </ul>
    </div>
  );
}

// --- derivations (pure; exported for tests) ------------------------------------

// canLines: the granted capabilities (AMBER). Each is one lookup on the clamped
// inline_policy. A capability grant — a writable mount, a push token, a brokered
// credential, broad/allow-list egress — is a CAUTION: amber, never a green check.
export function canLines(p: RunPolicySpec): CapLine[] {
  const out: CapLine[] = [];
  const mounts = p.workspace_mounts ?? [];
  const grants = p.eligible_grants ?? [];
  const domains = p.allowed_domains ?? [];
  const allowAll = !!p.allow_all_egress;

  // Wire contract: an OMITTED read_only means READ-ONLY (types.ts / Go
  // ReadOnlyOrDefault) — only an explicit false grants write.
  if (mounts.some((m) => m.read_only === false)) {
    out.push({
      icon: FilePenLine,
      text: "Read and edit files in its workspace — changes are written to your disk.",
    });
  }

  for (const g of grants) {
    if (g.kind === "github_token") {
      if (isWriteCapable(g)) {
        out.push({
          icon: GitBranch,
          // Approval gates the TOKEN MINT (once), not every push — never claim
          // a per-push gate that doesn't exist.
          text: `Push branches and open PRs${
            g.requires_approval ? " — you approve its token before it's minted" : " without pausing to ask"
          }.`,
        });
      }
      // A read-only GitHub token is surfaced as a guarantee in the can't column.
    } else if (g.kind === "git_pat") {
      // Honest exception: the PAT is handed to git INSIDE the sandbox, so the
      // agent's process can read it — there is no broker protection to claim.
      out.push({ icon: KeyRound, text: CAPABILITY.gitPatLine });
    } else if (g.kind === "cloud_sts") {
      // The embedded IdP structurally refuses cloud_sts mints (needs SPIRE) —
      // never claim a capability the backend can't deliver.
      out.push({
        icon: KeyRound,
        text: "Requests a cloud credential — this control plane can't mint it (needs a SPIRE identity provider).",
      });
    } else {
      out.push({
        icon: KeyRound,
        text: `Use a short-lived ${grantNoun(g)}${g.requires_approval ? " — asks you first" : ""}.`,
      });
    }
  }

  if (allowAll) {
    out.push({ icon: Globe, text: CAPABILITY.allowAllEgress });
  } else if (domains.length > 0) {
    out.push({
      icon: Globe,
      text: `Reach ${
        domains.length === 1 ? "one approved site" : `${domains.length} approved sites`
      }: ${domains.map(hostLabel).join(", ")}.`,
    });
  }

  out.push({ icon: Terminal, text: "Run tests and shell commands inside its sandbox." });

  if (p.auto_stop_after_sec === -1) {
    out.push({ icon: Clock, text: "Stay up until you stop it — it has no auto-stop." });
  }

  return out;
}

// cantLines: the confinement guarantees (TEAL) — what the sandbox structurally
// prevents. Each is derived from the same clamped fields, with the honest
// fallbacks (broker line for brokered grants; block-list phrasing for allow_all).
export function cantLines(p: RunPolicySpec): CapLine[] {
  const out: CapLine[] = [];
  const mounts = p.workspace_mounts ?? [];
  const grants = p.eligible_grants ?? [];
  const domains = p.allowed_domains ?? [];
  const allowAll = !!p.allow_all_egress;

  // Egress guarantee. allow_all still denies the block-list + private/internal
  // hosts (SSRF guard) — the honest residual, never "unrestricted".
  if (allowAll) {
    out.push({
      icon: ShieldX,
      text: "Reach a blocked or private/internal address — the block-list and SSRF guard still apply.",
    });
  } else if (domains.length === 0 && !firstUseRaisesApproval(p.first_use_approval)) {
    // With first-use approval ON, an empty allow-list is NOT "no network" —
    // every host escalates to the operator, so the else branch's honest
    // "without asking you first" guarantee applies instead.
    out.push({ icon: WifiOff, text: "Reach the internet — it has no network access." });
  } else {
    out.push({
      icon: ShieldCheck,
      text: firstUseRaisesApproval(p.first_use_approval)
        ? "Reach an unlisted site without asking you first."
        : "Reach any site outside its allow-list — everything else is denied.",
    });
  }

  // Credential guarantee. cloud_sts is excluded: the broker structurally
  // refuses to mint it (needs SPIRE), so the broker line would be fiction.
  const brokered = grants.filter((g) => g.kind === "github_token" || g.kind === "api_key");
  if (grants.length === 0) {
    out.push({ icon: KeyRound, text: "Use any credentials — it has no keys or tokens." });
  } else {
    if (brokered.length > 0) {
      out.push({ icon: KeyRound, text: CAPABILITY.brokerLine });
    }
    if (grants.some((g) => g.kind === "github_token" && !isWriteCapable(g))) {
      out.push({
        icon: GitBranch,
        text: "Push or modify your repos — its GitHub token is read-only.",
      });
    }
  }

  // Files guarantee. Omitted read_only means READ-ONLY on the wire.
  if (mounts.length === 0) {
    out.push({ icon: FolderLock, text: "See your files — it runs in a throwaway sandbox." });
  } else if (mounts.every((m) => m.read_only !== false)) {
    out.push({ icon: FolderLock, text: "Change your files — they're mounted read-only." });
  } else {
    out.push({ icon: FolderLock, text: "Reach outside the folder you pointed it at." });
  }

  // Barrier guarantee (label only — the wire code CCx never leaks to the user).
  // "Sealed" is honest for Wall/Vault, but the Fence has holes (shared kernel),
  // so CC1 carries its residual line instead of an absolute guarantee (D11).
  const meta = CC_META[p.min_confinement_class];
  out.push({
    icon: ShieldCheck,
    text:
      p.min_confinement_class === "CC1"
        ? `Touch the rest of your machine — it's behind a ${CC_META.CC1.label}. ${RESIDUAL_PREFIX} ${CC_META.CC1.doesntProtect}`
        : `Touch the rest of your machine — it's sealed behind a ${meta?.label ?? "sandbox"}.`,
  });

  // Lifecycle guarantee.
  if (typeof p.auto_stop_after_sec === "number" && p.auto_stop_after_sec > 0) {
    out.push({
      icon: Clock,
      text: `Outlive its ${minutes(p.auto_stop_after_sec)}-minute auto-stop, or survive a kill.`,
    });
  } else {
    out.push({ icon: ShieldX, text: "Survive a kill — Wardyn can stop it at any moment." });
  }

  // Always-true invariant (no field — inherent to Clamp/Grade/audit).
  out.push({ icon: Lock, text: "Change these limits, or hide its activity from the audit log." });

  return out;
}

// guaranteeSentences: the shared plain-language translation of a clamped policy,
// each derived only from an inline_policy field with the honest fallbacks. STABLE
// CONTRACT — imported by policies.tsx (stored-policy detail) so a stored and a
// proposed policy read identically; keep the name/signature/return type.
export function guaranteeSentences(p: RunPolicySpec): string[] {
  const out: string[] = [];
  const mounts = p.workspace_mounts ?? [];
  const grants = p.eligible_grants ?? [];
  const domains = p.allowed_domains ?? [];
  const allowAll = !!p.allow_all_egress;

  // Files / mounts.
  if (mounts.length === 0) {
    out.push("Runs in a throwaway sandbox; sees none of your files.");
  } else {
    out.push("Can only touch the folder you pointed it at — not your whole computer.");
    // Omitted read_only means READ-ONLY on the wire — only explicit false writes.
    out.push(
      mounts.some((m) => m.read_only === false)
        ? "Can edit and save changes to that folder on your disk."
        : "Your files are look-but-don't-touch.",
    );
  }

  // Egress. allow_all_egress is ALWAYS the block-list phrasing, never "unrestricted".
  if (allowAll) {
    out.push("Can reach almost any site (except a block-list).");
  } else if (domains.length === 0 && !firstUseRaisesApproval(p.first_use_approval)) {
    out.push("No internet — it can reach nothing.");
  } else if (domains.length === 0) {
    // First-use approval with an empty allow-list: every site is unlisted and
    // escalates to the operator before it opens — not "no internet".
    out.push("Must ask you before reaching an unlisted site.");
  } else {
    out.push(
      `Can only reach ${domains.length === 1 ? "this site" : `these ${domains.length} sites`}: ` +
        `${domains.map(hostLabel).join(", ")}.`,
    );
    // first_use_approval is inert under allow_all, so only meaningful in allow-list mode.
    out.push(
      firstUseRaisesApproval(p.first_use_approval)
        ? "Must ask you before reaching an unlisted site."
        : "Unlisted sites are silently blocked.",
    );
  }

  // Credentials.
  if (grants.length === 0) {
    out.push("Cannot use any credentials — has no keys or tokens.");
  } else {
    for (const g of grants) {
      const writeCapable = isWriteCapable(g);
      if (g.requires_approval) {
        out.push(`Must ask you before using the ${grantNoun(g)}.`);
      } else if (writeCapable) {
        out.push("Auto-mints a write-capable credential with no approval.");
      }
      if (g.kind === "github_token") {
        out.push(writeCapable ? "Can push / modify your repos." : "The GitHub token is read-only.");
      }
      if (typeof g.ttl_seconds === "number" && g.ttl_seconds > 0) {
        out.push(`Any credential self-destructs in ≤ ${minutes(g.ttl_seconds)} min.`);
      }
    }
  }

  // Lifecycle.
  if (p.auto_stop_after_sec === -1) {
    out.push("Stays up until you stop it.");
  } else if (typeof p.auto_stop_after_sec === "number" && p.auto_stop_after_sec > 0) {
    out.push(`Shuts itself off after ${minutes(p.auto_stop_after_sec)} min idle.`);
  }

  // Confinement substrate.
  if (p.min_confinement_class === "CC3") out.push("Runs in a Vault — its own hardware-isolated VM.");
  else if (p.min_confinement_class === "CC1")
    out.push("Runs behind a Fence — the weakest tier; shares the host kernel.");
  else out.push("Runs behind a Wall — a gVisor sandbox (default).");

  // Inherent to Clamp/Grade/advisory — always true, no field.
  out.push("Cannot change any of these limits, and cannot see your prompts leaving.");

  return out;
}

// whyRisky: the "why" line — HIGH rationales if any, else the top few rationales.
// All are the grader's already-human-readable strings (never the model's).
export function whyRisky(risk_assessment: RiskItem[]): string[] {
  const high = risk_assessment.filter((r) => r.risk_level === "high").map((r) => r.rationale);
  const src = high.length > 0 ? high : risk_assessment.map((r) => r.rationale);
  return src.slice(0, 3);
}

function isWriteCapable(g: GrantSpec): boolean {
  if (g.kind !== "github_token") return false;
  const perms = (g.scope as { permissions?: Record<string, unknown> } | undefined)?.permissions;
  const contents = perms?.contents;
  return contents === "write" || contents === "admin";
}

function grantNoun(g: GrantSpec): string {
  if (g.kind === "github_token") return "GitHub token";
  if (g.kind === "api_key") return "API key";
  if (g.kind === "cloud_sts") return "cloud credential";
  return "credential";
}

function minutes(sec: number): number {
  return Math.max(1, Math.round(sec / 60));
}
