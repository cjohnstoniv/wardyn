/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Shared first-run funnel primitives — the honest intro that appears BOTH on the
// first-boot Welcome hero (onboarding-screen.tsx) and as the dismissible
// IntroPanel above the Getting-started stepper (setup-screen.tsx). One copy of
// the "how it works" strip, the readiness derivation, and the re-check feedback
// line, so Welcome and the funnel can never drift apart (B1/B2/B5).
import * as React from "react";
import {
  BrickWall,
  ChevronRight,
  CircleX,
  Fingerprint,
  KeyRound,
  Loader2,
  ScrollText,
  ShieldCheck,
  X,
} from "lucide-react";
import type { SetupStatus } from "../../../lib/types";

// The single honest one-liner shown under the hero and in the IntroPanel.
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
const NODE_TONE: Record<NodeTone, string> = {
  muted: "text-muted-foreground",
  primary: "text-primary",
  warning: "text-warning",
};

export function HowItWorksStrip() {
  return (
    <div className="flex flex-wrap items-stretch gap-1.5">
      {HOW_IT_WORKS.map((n, i) => (
        <React.Fragment key={n.title}>
          <div className="flex min-w-[118px] flex-1 flex-col gap-1.5 rounded-[10px] border border-border bg-background/60 p-3">
            <span className={NODE_TONE[n.tone]}>
              <n.Icon className="size-4" />
            </span>
            <span className="text-[12.5px] font-semibold text-foreground">{n.title}</span>
            <span className="text-[11.5px] leading-snug text-muted-foreground">{n.sub}</span>
          </div>
          {i < HOW_IT_WORKS.length - 1 && (
            <ChevronRight aria-hidden className="size-3.5 shrink-0 self-center text-muted-foreground/50" />
          )}
        </React.Fragment>
      ))}
    </div>
  );
}

// ---------------------------------------------------------------------------
// IntroPanel — the dismissible intro that sits above the single stepper (B2).
// ---------------------------------------------------------------------------
export function IntroPanel({ onHide }: { onHide: () => void }) {
  return (
    <div className="mb-4 rounded-2xl border border-border bg-card p-5">
      <div className="flex items-start justify-between gap-3">
        <p className="max-w-[620px] text-sm leading-relaxed text-foreground/90">
          <IntroBlurb />
        </p>
        <button
          onClick={onHide}
          className="flex shrink-0 items-center gap-1.5 text-[12.5px] font-medium text-muted-foreground transition-colors hover:text-foreground"
        >
          <X className="size-3.5" /> Hide intro
        </button>
      </div>
      <div className="mt-4">
        <HowItWorksStrip />
      </div>
    </div>
  );
}

// ---------------------------------------------------------------------------
// Readiness derivation from the REAL SetupStatus (B3/B6). barrierCount drives
// the honest "N of 3 barriers" badge; llmReady/composerReady drive the readiness
// chips and fast-path. Credentials are deliberately excluded (B8).
// ---------------------------------------------------------------------------
export function hasLlmPath(status: SetupStatus): boolean {
  if (status.providers.some((p) => p.logged_in)) return true;
  if (status.secrets.present.some((n) => /anthropic|openai|api[-_]?key/i.test(n))) return true;
  // Honesty guard (mirrors the backend's llmAccessAvailable): a `fake` composer
  // backend resolves trivially but calls NO model, so it is not real LLM access.
  // Counting it would render "LLM ✓ / Composer backend ready" for the default
  // `make setup` demo config, which has no model behind it.
  if (status.composer.backends.some((b) => b.key_resolved && b.wire !== "fake")) return true;
  return false;
}

export function hasGitCredential(status: SetupStatus): boolean {
  return status.secrets.github_app || status.secrets.present.some((n) => /\bpat\b|-pat$|^pat-/i.test(n));
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
// RecheckFeedback (B5) — after Re-check, never a silent dead-end. Shows a live
// "checking…" line, or a concrete "still not detected" result + when it was
// last checked. Rendered per-tier from the actual re-probed status.
// ---------------------------------------------------------------------------
export function lastCheckedLabel(at: Date | null): string {
  if (!at) return "";
  const s = Math.round((Date.now() - at.getTime()) / 1000);
  if (s < 5) return "Checked just now";
  if (s < 60) return `Checked ${s}s ago`;
  return `Last checked ${at.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" })}`;
}

export function RecheckFeedback({
  rechecking,
  message,
}: {
  rechecking: boolean;
  /** The concrete "still not detected — …" line, shown once a re-check has run and the thing is still missing. */
  message?: string;
}) {
  // Always-mounted polite live region so a screen reader announces the async
  // re-check outcome instead of it appearing silently in the DOM.
  return (
    <div role="status" aria-live="polite">
      {rechecking ? (
        <p className="mt-2 flex items-start gap-1.5 text-[12.5px] leading-snug text-info">
          <Loader2 className="mt-0.5 size-3.5 shrink-0 animate-spin" />
          Re-checking the host…
        </p>
      ) : message ? (
        <p className="mt-2 flex items-start gap-1.5 text-[12.5px] leading-snug text-danger">
          <CircleX className="mt-0.5 size-3.5 shrink-0" />
          {message}
        </p>
      ) : null}
    </div>
  );
}
