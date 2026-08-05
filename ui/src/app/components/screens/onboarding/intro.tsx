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
import { deriveIntegrations, defaultHolder, type IntegrationRow } from "../../../lib/api/integrations";

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
//
// llmReady/llmLabel/composerReady read the SAME rows /integrations itself
// derives (lib/api/integrations.ts) instead of a bespoke heuristic over raw
// SetupStatus fields — one source of truth, so a row that reads "Configured"
// on the Integrations page can never disagree with the funnel. AI rows never
// depend on SiteConfig (only SCM/mirror/proxy rows do), so `null` is the right
// siteConfig to pass into deriveIntegrations here.
//
// Honesty guard (unchanged in effect from the old five-way hasLlmPath): a
// `wire: "fake"` composer backend — the default `make setup` demo config —
// can never satisfy either check below. deriveAiRows only ever reads
// status.composer.backends for the Azure provider; every other AI row comes
// from a real secret, a captured harness login, or Bedrock config. A `fake`
// backend matches none of those, so it never becomes a row in the first
// place — counting it would render a green readiness for a config with no
// model behind it. Do not "fix" this by falling back to raw
// status.composer.backends.
// ---------------------------------------------------------------------------
const AGENT_TOOL_CAPABILITY = /Claude Code|Codex/;
const WARDYN_FEATURES_CAPABILITY = /^Wardyn features/;

function aiIntegrationRows(status: SetupStatus): IntegrationRow[] {
  return deriveIntegrations(status, null, status.secrets.present).ai;
}

// A row's credential is genuinely usable — excludes Bedrock/Azure's
// region/model-incomplete posture (IntegrationRow's "region_model_unset").
// Every other AI row deriveAiRows produces is only ever created once its
// credential is actually present, so this only ever excludes something for
// those two types.
function credentialResolved(row: IntegrationRow): boolean {
  return row.posture.kind !== "region_model_unset";
}

function agentCapableRows(rows: IntegrationRow[]): IntegrationRow[] {
  return rows.filter(
    (r) => credentialResolved(r) && r.chips.some((c) => !c.muted && AGENT_TOOL_CAPABILITY.test(c.label)),
  );
}

// Whether a coding agent (Claude Code / Codex CLI) has somewhere to call —
// ≥1 integration with an agent-tool capability ON and a resolved credential.
// Used directly by callers that only need the boolean (import-panel.tsx,
// record-pane.tsx's model-readiness warning) without the rest of Readiness.
export function hasLlmPath(status: SetupStatus): boolean {
  return agentCapableRows(aiIntegrationRows(status)).length > 0;
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

// The row that HOLDS the default for the agent-tool slot, if any type's matrix
// marks one (today only anthropic_api_key's Claude Code does — see
// defaultHolder's own W5 note in lib/api/integrations.ts); otherwise just the
// first agent-capable connected one, since with a single integration it's
// trivially the default. Exported so a caller that needs the ROW itself — not
// just its name (Readiness.llmLabel) — can reuse this exact pick instead of
// re-deriving it: the "agent in the box" Getting-Started step's binding picker
// (demos/harness-demo.ts) chooses the same row llmReady itself is built from.
export function defaultAgentRow(status: SetupStatus): IntegrationRow | undefined {
  const agentRows = agentCapableRows(aiIntegrationRows(status));
  return defaultHolder(agentRows, AGENT_TOOL_CAPABILITY) ?? agentRows[0];
}

export function deriveReadiness(status: SetupStatus): Readiness {
  const barrierCount = status.runner?.confinement_classes?.length ?? 0;
  const aiRows = aiIntegrationRows(status);
  const agentRows = agentCapableRows(aiRows);
  const defaultRow = defaultAgentRow(status);
  // ≥1 integration with the Wardyn-features capability ON and a resolved
  // credential — powers the AI Composer / Wardyn's own review features.
  const composerReady = aiRows.some(
    (r) => credentialResolved(r) && r.chips.some((c) => !c.muted && WARDYN_FEATURES_CAPABILITY.test(c.label)),
  );
  return {
    ready: status.ready,
    barrierReady: barrierCount > 0,
    barrierCount,
    llmReady: agentRows.length > 0,
    llmLabel: defaultRow?.name ?? "",
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
