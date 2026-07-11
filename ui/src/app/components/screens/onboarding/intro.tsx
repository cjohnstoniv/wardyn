/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Shared first-run funnel primitives — the honest intro blurb + "how it works"
// strip (reused by the first-boot Welcome hero in onboarding-screen.tsx and the
// funnel shell's intro panel in setup-layout.tsx) plus the readiness derivation,
// so Welcome and the funnel can never drift apart (B1/B3/B6).
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
import type { SetupStatus } from "../../../lib/types";

// The single honest one-liner shown under the Welcome hero and in the funnel
// shell's intro panel.
export function IntroBlurb() {
  return (
    <>
      Wardyn runs your coding agents behind a barrier, with{" "}
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
  { Icon: KeyRound, title: "Keys stay brokered", sub: "Short-lived tokens, never your real keys", tone: "muted" },
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

// ---------------------------------------------------------------------------
// Readiness derivation from the REAL SetupStatus (B3/B6). barrierCount drives
// the honest "N of 3 barriers" badge; llmReady/composerReady drive the readiness
// chips and fast-path. Credentials are deliberately excluded (B8).
// ---------------------------------------------------------------------------
export function hasLlmPath(status: SetupStatus): boolean {
  if (status.providers.some((p) => p.logged_in)) return true;
  // A Wardyn-managed subscription (captured via container login) is real model
  // access even with no resident host login — the compose-mode path.
  if (status.harness?.some((h) => h.captured)) return true;
  if (status.secrets.present.some((n) => /anthropic|openai|api[-_]?key/i.test(n))) return true;
  // Honesty guard (mirrors the backend's llmProvenance): a `fake` composer
  // backend resolves trivially but calls NO model, so it is not real LLM access.
  // Counting it would render "LLM ✓ / Composer backend ready" for the default
  // `make setup` demo config, which has no model behind it.
  if (status.composer.backends.some((b) => b.key_resolved && b.wire !== "fake")) return true;
  return false;
}

export interface Readiness {
  /** The backend's own boot readiness (status.ready) — gates the fast-path (B3). */
  ready: boolean;
  barrierReady: boolean;
  barrierCount: number;
  llmReady: boolean;
  /** Human label for the connected LLM path, "" when none. */
  llmLabel: string;
  composerReady: boolean;
}

export function deriveReadiness(status: SetupStatus): Readiness {
  const barrierCount = status.runner?.confinement_classes?.length ?? 0;
  const claude = status.providers.find((p) => p.tool === "claude" && p.logged_in);
  let llmLabel = "";
  if (claude) llmLabel = "Claude connected";
  else if (status.harness?.some((h) => h.provider === "anthropic" && h.captured))
    llmLabel = "Claude connected (Wardyn-managed login)";
  else if (status.secrets.present.some((n) => /anthropic/i.test(n))) llmLabel = "Anthropic key added";
  else if (status.secrets.present.some((n) => /openai/i.test(n))) llmLabel = "OpenAI key added";
  else if (status.composer.backends.some((b) => b.key_resolved && b.wire !== "fake"))
    llmLabel = "Composer backend ready";
  const composerReady =
    status.composer.enabled &&
    status.composer.backends.some((b) => b.enabled && (!b.needs_key || b.key_resolved));
  return {
    ready: status.ready,
    barrierReady: barrierCount > 0,
    barrierCount,
    llmReady: hasLlmPath(status),
    llmLabel,
    composerReady,
  };
}

// ---------------------------------------------------------------------------
// lastCheckedLabel — the relative "Checked Ns ago" line for the host-status
// strip and the Review step's re-check control.
// ---------------------------------------------------------------------------
export function lastCheckedLabel(at: Date | null): string {
  if (!at) return "";
  const s = Math.round((Date.now() - at.getTime()) / 1000);
  if (s < 5) return "Checked just now";
  if (s < 60) return `Checked ${s}s ago`;
  return `Last checked ${at.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" })}`;
}
