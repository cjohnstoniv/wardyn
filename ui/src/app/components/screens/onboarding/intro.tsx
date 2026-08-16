/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Shared first-run funnel primitives — the honest intro blurb + "how it works"
// strip (reused by the first-boot Welcome hero and the funnel shell's intro panel).
// The readiness derivation moved to lib/readiness.ts — it outlives this screen.
import * as React from "react";
import {
  BrickWall,
  ChevronRight,
  Fingerprint,
  KeyRound,
  ScrollText,
  ShieldCheck,
} from "lucide-react";
import { cn } from "../../ui/utils";

// The single honest one-liner shown under the Welcome hero and in the funnel
// shell's intro panel. Platform-first: coding agents are the flagship use, not
// the definition — a run can be a script, a build, a tool, or a plain command.
export function IntroBlurb() {
  return (
    <>
      Wardyn runs any workload — coding agents, scripts, builds, tools — behind a barrier, with{" "}
      <strong className="font-semibold text-foreground">
        no resident credentials by default and no privileged host access
      </strong>
      . Every run gets its own identity; you gate the risky moments; everything is audited.
    </>
  );
}

// ---------------------------------------------------------------------------
// The single 5-node "how it works" strip (replaces the old 7-page tour). Tones
// are semantic (muted / primary / warning) — teal (primary) is reserved for the
// barrier node, warning for the "you gate the risky bits" node (honest: a grant
// is never a reassuring green).
// ---------------------------------------------------------------------------
type NodeTone = "muted" | "primary" | "warning";
const HOW_IT_WORKS: { Icon: React.ElementType; title: string; sub: string; tone: NodeTone }[] = [
  { Icon: Fingerprint, title: "Own identity", sub: "Every run, cryptographically scoped", tone: "muted" },
  { Icon: BrickWall, title: "Behind a barrier", sub: "Fence, Wall, or Vault — you choose", tone: "primary" },
  // Not "never your real keys, unqualified" — copy.ts's CAPABILITY comments
  // are explicit that brokerLine is "fully true only for github_token"; a
  // git_pat/ssh_key grant, and a host-CLI Claude subscription
  // (resident_mount), really do put the real credential in the sandbox. "By
  // default" + "labeled" keeps this true for every lane: brokering is the
  // common case, and the resident exceptions are the amber `resident` chip
  // (RESIDENCY_META.resident_mount) elsewhere in this same funnel, not a
  // silently different story.
  { Icon: KeyRound, title: "Keys stay brokered by default", sub: "Resident lanes are labeled, not hidden", tone: "muted" },
  { Icon: ShieldCheck, title: "You gate the risky bits", sub: "Egress and writes ask first", tone: "warning" },
  { Icon: ScrollText, title: "Everything recorded", sub: "Append-only audit; session replay where the runner supports it", tone: "muted" },
];
const NODE_TONE: Record<NodeTone, { ring: string; iconWrap: string }> = {
  muted: { ring: "border-border", iconWrap: "bg-muted text-foreground" },
  primary: { ring: "border-primary/40", iconWrap: "bg-primary/15 text-primary" },
  warning: { ring: "border-warning/40", iconWrap: "bg-warning-subtle text-warning" },
};

export function HowItWorksStrip() {
  return (
    <ol
      className="grid grid-cols-1 gap-2 sm:grid-cols-2 lg:flex lg:items-stretch"
      aria-label="How Wardyn protects each run"
    >
      {HOW_IT_WORKS.map((n, i) => {
        const tone = NODE_TONE[n.tone];
        return (
          <li key={n.title} className="flex items-center gap-2 lg:flex-1">
            <div className={cn("flex-1 rounded-xl border bg-card p-3", tone.ring)}>
              <div className={cn("mb-2 inline-flex size-8 items-center justify-center rounded-lg", tone.iconWrap)}>
                <n.Icon className="size-4" aria-hidden />
              </div>
              <div className="text-sm text-foreground">{n.title}</div>
              <div className="mt-0.5 text-xs text-muted-foreground">{n.sub}</div>
            </div>
            {i < HOW_IT_WORKS.length - 1 && (
              <ChevronRight className="hidden size-4 shrink-0 text-muted-foreground lg:block" aria-hidden />
            )}
          </li>
        );
      })}
    </ol>
  );
}
