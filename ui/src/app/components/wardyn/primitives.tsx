/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import { cn } from "../ui/utils";
import { CC_META } from "./cc-meta";
import {
  Bot,
  User,
  Cpu,
  Fence,
  BrickWall,
  Vault,
  CircleCheck,
  CircleX,
  CircleAlert,
} from "lucide-react";
import type {
  ActorType,
  Agent,
  ApprovalKind,
  ApprovalState,
  ConfinementClass,
  Outcome,
  RiskLevel,
  RunState,
} from "../../lib/types";

/* ---------- generic semantic chip ---------- */
type Tone = "neutral" | "success" | "warning" | "danger" | "info" | "cyan" | "primary";

const toneClass: Record<Tone, string> = {
  neutral: "bg-muted text-muted-foreground border-border",
  success: "bg-success-subtle text-success border-success/25",
  warning: "bg-warning-subtle text-warning border-warning/25",
  danger: "bg-danger-subtle text-danger border-danger/25",
  info: "bg-info-subtle text-info border-info/25",
  cyan: "bg-cyan-subtle text-cyan border-cyan/25",
  primary: "bg-primary/12 text-primary border-primary/25",
};

export function Chip({
  tone = "neutral",
  className,
  children,
  dot,
  pulse,
  mono,
  title,
  srLabel,
}: {
  tone?: Tone;
  className?: string;
  children: React.ReactNode;
  dot?: boolean;
  pulse?: boolean;
  mono?: boolean;
  title?: string;
  // Accessible NAME for the chip (aria-label). Prefer this over an sr-only text
  // twin: it announces the reason to AT without adding a duplicate text node
  // (which double-matches getByText) and without leaking the value into the
  // DOM text content — so an internal-only string (a confinement wire code)
  // stays out of accessible content entirely (D4). Use only for AT-safe copy.
  srLabel?: string;
}) {
  return (
    <span
      title={title}
      aria-label={srLabel}
      className={cn(
        "inline-flex items-center gap-1.5 rounded-md border px-2 py-0.5 text-xs font-medium leading-5 whitespace-nowrap w-fit",
        toneClass[tone],
        mono && "font-mono",
        className,
      )}
    >
      {dot &&
        (pulse ? (
          <span className="relative flex size-1.5">
            <span className="absolute inline-flex size-full animate-ping rounded-full bg-current opacity-60" />
            <span className="relative inline-flex size-1.5 rounded-full bg-current" />
          </span>
        ) : (
          <span className="size-1.5 rounded-full bg-current opacity-80" />
        ))}
      {children}
    </span>
  );
}

/* ---------- section eyebrow label ---------- */
export function SectionLabel({
  children,
  className,
}: {
  children: React.ReactNode;
  className?: string;
}) {
  return <div className={cn("label-eyebrow", className)}>{children}</div>;
}

/* ---------- fail-soft enum lookup ----------
 * Wire enums (run state, confinement class, actor type, outcome, approval
 * kind/state) are owned by the backend. If the backend adds or renames a value
 * the UI does not know about, an unguarded `meta[value]` returns undefined and
 * dereferencing it throws inside render — and with no error boundary that
 * blanks the whole screen. metaFor() degrades to a neutral fallback that shows
 * the raw wire value instead, so a new backend state can never crash the UI.
 */
// NoInfer pins T to the table's value type, so an inline fallback literal is
// contextually typed (e.g. tone: "neutral" stays Tone, not widened to string).
function metaFor<T>(table: Record<string, T>, key: string, fallback: NoInfer<T>): T {
  return (key != null && table[key]) || fallback;
}

/* ---------- run state ---------- */
const runStateMeta: Record<string, { tone: Tone; label: string; pulse?: boolean }> = {
  PENDING: { tone: "neutral", label: "Pending" },
  STARTING: { tone: "info", label: "Starting", pulse: true },
  RUNNING: { tone: "success", label: "Running", pulse: true },
  WAITING_FOR_CONFIRMATION: { tone: "warning", label: "Awaiting confirmation", pulse: true },
  COMPLETED: { tone: "success", label: "Completed" },
  STOPPED: { tone: "neutral", label: "Stopped" },
  ARCHIVED: { tone: "neutral", label: "Archived" },
  FAILED: { tone: "danger", label: "Failed" },
  KILLED: { tone: "danger", label: "Killed" },
};

export function RunStateBadge({ state }: { state: RunState }) {
  const m = metaFor(runStateMeta, state as string, { tone: "neutral", label: String(state) });
  return (
    <Chip tone={m.tone} dot pulse={m.pulse}>
      {m.label}
    </Chip>
  );
}

/* ---------- confinement class ----------
 * Tone/Icon are presentational and live here; the hint text is the honest,
 * substrate-only wording shared with step-confinement.tsx via cc-meta.ts —
 * see that module for why (credential brokering / egress filtering / HITL
 * approvals are policy-driven, not tied to the confinement class).
 */
// Fence / Wall / Vault — a single "how separated is the agent from your machine"
// ladder. Each tier gets its own METALS token (bronze/silver/gold, defined in
// theme.css) instead of a teal intensity ramp — teal is the action/agent
// accent, never a barrier tier, so tier color and action color never collide.
// The icon + the
// ordinal position carry the ladder; the wire code (CC1/2/3) + the honest
// substrate mechanism live in the tooltip, never on the chip face.
const ccMeta: Record<
  ConfinementClass,
  { Icon: React.ElementType; cls: string; fillClass: string; ordinal: 1 | 2 | 3 }
> = {
  CC1: { Icon: Fence, cls: "bg-fence-bg text-fence-fg border-fence-border", fillClass: "bg-fence-fg", ordinal: 1 },
  CC2: { Icon: BrickWall, cls: "bg-wall-bg text-wall-fg border-wall-border", fillClass: "bg-wall-fg", ordinal: 2 },
  CC3: { Icon: Vault, cls: "bg-vault-bg text-vault-fg border-vault-border", fillClass: "bg-vault-fg", ordinal: 3 },
};
const ccMetaFallback = {
  Icon: Fence,
  cls: "bg-muted text-muted-foreground border-border",
  fillClass: "bg-muted-foreground",
  ordinal: 1 as const,
};
export function ConfinementChip({ value }: { value: ConfinementClass }) {
  const meta = CC_META[value as ConfinementClass];
  const label = meta?.label ?? String(value);
  const mechanism = meta?.mechanism ?? String(value);
  const m = metaFor(ccMeta, value as string, ccMetaFallback);
  const title = `${label} — ${mechanism} · internal class ${value}`;
  return (
    <Chip
      tone="neutral"
      title={title}
      className={cn(m.cls, "gap-1 px-1.5")}
    >
      <m.Icon className="size-3" />
      {label}
      {/* No sr-only twin here: `title` carries the mechanism + internal wire
          class (CC1/2/3, gVisor/runc/Kata) for a sighted power-user's hover, but
          that string must NEVER reach accessible content — the confinement class
          is internal (D4). The visible barrier label ("Fence"/"Wall"/"Vault") is
          the accessible name; screen-reader users hear it, not the wire code. */}
    </Chip>
  );
}

// Shared tier->ordinal (+ fill color) lookup so BarrierStrengthStrip renders
// the exact same ladder position and metal tint as ConfinementChip, from one
// source instead of a second copy of the tier table.
export function confinementTierMeta(value: ConfinementClass): { ordinal: 1 | 2 | 3; fillClass: string } {
  const m = metaFor(ccMeta, value as string, ccMetaFallback);
  return { ordinal: m.ordinal, fillClass: m.fillClass };
}

/* ---------- actor type (audit hero) ---------- */
const actorMeta: Record<ActorType, { tone: Tone; Icon: React.ElementType; label: string }> = {
  human: { tone: "info", Icon: User, label: "human" },
  agent: { tone: "primary", Icon: Bot, label: "agent" },
  system: { tone: "neutral", Icon: Cpu, label: "system" },
};
export function ActorTypeChip({ type }: { type: ActorType }) {
  const m = metaFor(actorMeta, type as string, { tone: "neutral", Icon: Cpu, label: String(type) });
  return (
    <Chip tone={m.tone} className="font-semibold uppercase tracking-wide">
      <m.Icon className="size-3" />
      {m.label}
    </Chip>
  );
}

/* ---------- outcome ---------- */
const outcomeMeta: Record<Outcome, { Icon: React.ElementType; label: string; color: string }> = {
  success: { Icon: CircleCheck, label: "success", color: "text-success" },
  denied: { Icon: CircleAlert, label: "denied", color: "text-warning" },
  failure: { Icon: CircleX, label: "failure", color: "text-danger" },
};
export function OutcomeBadge({ outcome }: { outcome: Outcome }) {
  const m = metaFor(outcomeMeta, outcome as string, { Icon: CircleAlert, label: String(outcome), color: "text-muted-foreground" });
  return (
    <span className={cn("inline-flex items-center gap-1.5 text-xs font-medium", m.color)}>
      <m.Icon className="size-3.5" />
      {m.label}
    </span>
  );
}

/* ---------- approval kind / state ---------- */
const kindMeta: Record<ApprovalKind, { tone: Tone; label: string }> = {
  credential: { tone: "info", label: "credential" },
  egress_domain: { tone: "cyan", label: "egress-domain" },
  tool_call: { tone: "neutral", label: "tool-call" },
};
export function ApprovalKindChip({ kind }: { kind: ApprovalKind }) {
  const m = metaFor(kindMeta, kind as string, { tone: "neutral", label: String(kind) });
  return <Chip tone={m.tone} mono>{m.label}</Chip>;
}

const apprStateMeta: Record<string, Tone> = {
  PENDING: "warning",
  APPROVED: "success",
  DENIED: "danger",
  EXPIRED: "neutral",
};
export function ApprovalStateBadge({ state }: { state: ApprovalState }) {
  const s = String(state);
  return (
    <Chip tone={metaFor<Tone>(apprStateMeta, s, "neutral")} dot>
      {s ? s.charAt(0) + s.slice(1).toLowerCase() : "Unknown"}
    </Chip>
  );
}

/* ---------- composer risk level ---------- */
// Wardyn's DETERMINISTIC risk grade for a config choice (low/medium/high). The
// tone escalates with risk so high-risk items are visually unmistakable. Fail-
// soft: an unrecognized level degrades to a neutral chip showing the raw value.
const riskMeta: Record<string, { tone: Tone; label: string }> = {
  low: { tone: "success", label: "Low" },
  medium: { tone: "warning", label: "Medium" },
  high: { tone: "danger", label: "High" },
};
export function RiskBadge({ level }: { level: RiskLevel }) {
  const m = metaFor(riskMeta, level as string, { tone: "neutral", label: String(level) });
  return (
    <span data-risk={String(level)} className="inline-flex">
      <Chip tone={m.tone} dot>
        {m.label}
      </Chip>
    </span>
  );
}

/* ---------- egress decision ---------- */
export function EgressDecisionChip({ decision }: { decision: "allow" | "deny" | "pending" }) {
  const tone: Tone = decision === "allow" ? "success" : decision === "deny" ? "danger" : "warning";
  return <Chip tone={tone} dot mono>{decision}</Chip>;
}

/* ---------- agent monogram avatar ---------- */
// Circular colored initials (CC / CX / CU) — one consistent agent representation
// used on run cards, tables, and detail/recording headers alike.
type AgentMeta = { label: string; initials: string; badge: string };
const agentMeta: Record<string, AgentMeta> = {
  claude_code: { label: "Claude Code", initials: "CC", badge: "bg-agent-claude text-white" },
  codex: { label: "Codex", initials: "CX", badge: "bg-success text-white" },
  cursor: { label: "Cursor", initials: "CU", badge: "bg-info text-white" },
};

// Tolerate both dotted ("claude-code", "codex-cli") and underscore
// ("claude_code", "codex") agent ids coming off the wire.
function agentMetaFor(agent: string): AgentMeta {
  const key = agent.toLowerCase().replace(/-/g, "_");
  if (agentMeta[key]) return agentMeta[key];
  if (key.startsWith("claude")) return agentMeta.claude_code;
  if (key.startsWith("codex")) return agentMeta.codex;
  if (key.startsWith("cursor")) return agentMeta.cursor;
  // Unknown agent — render the first two chars on a neutral chip.
  return { label: agent, initials: agent.slice(0, 2).toUpperCase(), badge: "bg-muted text-muted-foreground" };
}

export function AgentBadge({ agent, withLabel = true }: { agent: Agent; withLabel?: boolean }) {
  const m = agentMetaFor(agent);
  return (
    <span className="inline-flex items-center gap-2 text-sm">
      <span className={cn("inline-flex size-6 shrink-0 items-center justify-center rounded-full text-[10px] font-semibold", m.badge)}>
        {m.initials}
      </span>
      {withLabel && <span className="text-foreground">{m.label}</span>}
    </span>
  );
}
