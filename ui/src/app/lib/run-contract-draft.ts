/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The New Run form's draft of the 0.9 contract fields: where the run lives, its
// size and the person's per-component overrides. The draft keeps everything the
// person typed; what is SENT is decided here, from which sections are active:
// an override of a section that is not on the page (its workspace was removed,
// its provider has no repository left) stays in the draft and is not sent, so
// re-adding the section brings the edit back and a stale edit never rides along.
//
// A-L8 owns the controller that holds this draft; the panels read and write it
// through that controller, and whether a control is offered at all is
// override-narrowing.ts's answer, never a panel's.
import { hostOverrideKind, type OverrideKind, type OverrideOp } from "./override-narrowing";
import type {
  ADOOverrides,
  AgentOverrides,
  GitPATOverride,
  PlacementValue,
  PushRuleOverride,
  RequestedResources,
  RunOverrides,
} from "./types/new-run-contract";

/** The edits the person made, grouped by the component that owns them. */
export interface OverrideDraft {
  agent?: AgentOverrides;
  /** Absent is untouched; an explicit empty set must reach the server's refusal. */
  azureDevOps?: ADOOverrides;
  gitPAT: GitPATOverride[];
  pushRules: PushRuleOverride[];
}

/**
 * The Runner tab's draft: where the run lives, which image, how large. Every
 * field is the person's own choice: absent means untouched, and an untouched
 * field is never sent, so the server's own default stays in charge. Lifetime
 * (auto-stop) and Barrier sit on this tab too but are existing WizardState
 * fields (`lifecycle`, `autoStopMinutes`, `confinementClass`).
 */
export interface RunnerDraft {
  /** "Runs on". Absent until chosen: there is no default between two eligible placements (OD-8). */
  placement?: PlacementValue;
  runnerId?: string;
  /** The ref of an image the organisation allows (preview `allowed_images`), sent as `allowed_image`. Absent keeps the resolved image. */
  imageRef?: string;
  /** CPU in CPUs (tenths) and memory in whole MiB, as the fields show them. */
  cpus?: number;
  memoryMiB?: number;
}

/** The Access tab's draft: the person's per-component edits. */
export interface AccessDraft {
  overrides: OverrideDraft;
}

/**
 * The draft of the contract fields, grouped by the tab that owns them (Info →
 * Workspaces → Runner → Access → Policy; new-run-tabs.ts assigns every other
 * form field). Info, Workspaces (the drive attach lives there) and Policy add
 * nothing to the contract.
 */
export interface RunContractDraft {
  runner: RunnerDraft;
  access: AccessDraft;
}

export const emptyOverrideDraft = (): OverrideDraft => ({ gitPAT: [], pushRules: [] });
export const emptyRunContractDraft = (): RunContractDraft => ({ runner: {}, access: { overrides: emptyOverrideDraft() } });

/** The sections that are on the page now; the console derives them from the dry-run facts and the attached workspaces. */
export interface ActiveSections {
  agent: boolean;
  azureDevOps: boolean;
  /** The forge hosts of the git_pat sections on the page. */
  gitPATHosts: readonly string[];
  /** The `provider/org` keys of the Git sections on the page (PushRuleOverride keys). */
  pushKeys: readonly string[];
}

export const NO_ACTIVE_SECTIONS: ActiveSections = { agent: false, azureDevOps: false, gitPATHosts: [], pushKeys: [] };

/** The key a push-rule override is filed under: provider and organisation. */
export const pushRuleKey = (p: Pick<PushRuleOverride, "provider" | "org">): string => `${p.provider}/${p.org}`;

const hasAgentEdit = (a: AgentOverrides): boolean =>
  Object.values(a).some((v) => Array.isArray(v) && v.length > 0);

/** The overrides to send: only the active sections' edits, none when nothing is left. */
export function buildOverrides(draft: OverrideDraft, active: ActiveSections): RunOverrides | undefined {
  const out: RunOverrides = {};
  if (active.agent && draft.agent && hasAgentEdit(draft.agent)) out.agent = draft.agent;
  if (active.azureDevOps && draft.azureDevOps) out.azure_devops = draft.azureDevOps;
  const pat = draft.gitPAT.filter((g) => active.gitPATHosts.some((h) => h.toLowerCase() === g.host.toLowerCase()));
  if (pat.length) out.git_pat = pat;
  const push = draft.pushRules.filter((p) => active.pushKeys.includes(pushRuleKey(p)));
  if (push.length) out.push_rules = push;
  return Object.keys(out).length ? out : undefined;
}

/** The overrides the draft holds that are not being sent, so a panel can say so ("Not used by the selected workspaces."). */
export function inactiveOverrides(draft: OverrideDraft, active: ActiveSections): OverrideDraft {
  const sent = buildOverrides(draft, active) ?? {};
  return {
    agent: sent.agent ? undefined : draft.agent && hasAgentEdit(draft.agent) ? draft.agent : undefined,
    azureDevOps: sent.azure_devops ? undefined : draft.azureDevOps,
    gitPAT: draft.gitPAT.filter((g) => !sent.git_pat?.includes(g)),
    pushRules: draft.pushRules.filter((p) => !sent.push_rules?.includes(p)),
  };
}

/** One edit in the narrowing table's terms (the TS twin of client.RunOverrides.Items). */
export interface OverrideItem {
  kind: OverrideKind;
  op: OverrideOp;
  subject: string;
}

export function overrideItems(o: RunOverrides): OverrideItem[] {
  const out: OverrideItem[] = [];
  const a = o.agent;
  if (a) {
    for (const h of a.add_hosts ?? []) out.push({ kind: hostOverrideKind(h), op: "add", subject: h });
    for (const h of a.remove_hosts ?? []) out.push({ kind: "agent_host", op: "remove", subject: h });
    for (const s of a.add_secrets ?? []) out.push({ kind: "agent_secret", op: "add", subject: s.secret_name });
    for (const n of a.remove_secrets ?? []) out.push({ kind: "agent_secret", op: "remove", subject: n });
    for (const r of a.tool_rules ?? []) out.push({ kind: r.effect === "allow" ? "tool_rule_allow" : "tool_rule_restrict", op: "add", subject: r.tool });
  }
  if (o.azure_devops) out.push({ kind: "ado_capability", op: "narrow", subject: "azure_devops.capabilities" });
  for (const g of o.git_pat ?? []) {
    if (g.repos !== undefined || g.access === "read" || g.api === false) out.push({ kind: "git_pat_scope", op: "narrow", subject: g.host });
    if (g.access === "write" || g.api === true) out.push({ kind: "git_pat_scope", op: "add", subject: g.host });
  }
  for (const p of o.push_rules ?? []) {
    for (const path of p.deny_paths ?? []) out.push({ kind: "push_rule_deny", op: "add", subject: `${pushRuleKey(p)} ${path}` });
    for (const path of p.require_review_paths ?? []) out.push({ kind: "push_rule_review", op: "add", subject: `${pushRuleKey(p)} ${path}` });
  }
  return out;
}

/** CPUs in tenths and whole MiB to the wire's milli-CPU and MiB; undefined for an untouched or invalid field. */
export function resourcesWire(cpus?: number, memoryMiB?: number): RequestedResources | undefined {
  const out: RequestedResources = {};
  const tenths = cpus === undefined ? Number.NaN : cpus * 10;
  const rounded = Math.round(tenths);
  // Decimal increments can acquire binary noise, but finer choices must not be rounded into another request.
  if (Number.isFinite(tenths) && rounded > 0 && Math.abs(tenths - rounded) <= Number.EPSILON * Math.max(1, Math.abs(tenths))) {
    out.cpu_millis = rounded * 100;
  }
  if (memoryMiB !== undefined && Number.isInteger(memoryMiB) && memoryMiB > 0) out.memory_mib = memoryMiB;
  return out.cpu_millis || out.memory_mib ? out : undefined;
}

/** The contract fields of the request body, each present only when the person chose it. */
export interface RunContractWire {
  placement?: PlacementValue;
  runner_id?: string;
  allowed_image?: string;
  resources?: RequestedResources;
  overrides?: RunOverrides;
}

export function buildRunContractWire(draft: RunContractDraft | undefined, active: ActiveSections): RunContractWire {
  if (!draft) return {};
  const { runner } = draft;
  const wire: RunContractWire = {};
  if (runner.placement) wire.placement = runner.placement;
  // A runner is named only for a run on a runner; the server refuses runner_id otherwise.
  if (runner.placement === "local" && runner.runnerId) wire.runner_id = runner.runnerId;
  if (runner.imageRef) wire.allowed_image = runner.imageRef;
  const resources = resourcesWire(runner.cpus, runner.memoryMiB);
  if (resources) wire.resources = resources;
  const overrides = buildOverrides(draft.access.overrides, active);
  if (overrides) wire.overrides = overrides;
  return wire;
}
