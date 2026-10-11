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
  Experience,
  GitPATOverride,
  IncludedTool,
  PlacementValue,
  PushRuleOverride,
  RequestedResources,
  RunOverrides,
  RunStartup,
  RunWorkload,
  StartFolder,
} from "./types/new-run-contract";
import { RUN_MODE_REFUSAL, RUN_MODE_REASON } from "./run-mode-refusals";

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
  /** The pool the run starts on. Absent is untouched: today's placement, and once pools are managed the person's, then the organisation's, default. */
  poolId?: string;
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
 * The run-mode draft (pkg/client/runs_mode.go): what the person is starting.
 * Experience belongs to Runner, the workload, tools and startup to Tools & Image,
 * the starting folder and the explicit "no repositories or drives" to
 * Repositories & Drives; the controller (A-L8) holds them as one group so a mode
 * switch revalidates the rest together. Nothing is inferred: a fresh draft has no
 * experience, and a draft without one sends none of these fields.
 */
export interface RunModeDraft {
  experience?: Experience;
  /** Background: an agent task or a command. Kept across a mode switch, sent only under Background. */
  workload?: RunWorkload;
  /** Included tools. Including is not starting: `startup` is separate. */
  tools: IncludedTool[];
  /** Interactive: what starts on its own. Absent is none. Kept across a mode switch, sent only under Interactive. */
  startup?: RunStartup;
  /** The one starting folder. Absent is the baseline: the first attached repository, else the image default. */
  startFolder?: StartFolder;
  /** The explicit choice to attach nothing. */
  noRepositoriesOrDrives?: boolean;
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
  /** Absent is untouched: nothing of the run mode is sent. */
  mode?: RunModeDraft;
}

export const emptyOverrideDraft = (): OverrideDraft => ({ gitPAT: [], pushRules: [] });
export const emptyRunModeDraft = (): RunModeDraft => ({ tools: [] });
export const emptyRunContractDraft = (): RunContractDraft => ({ runner: {}, access: { overrides: emptyOverrideDraft() }, mode: emptyRunModeDraft() });

/** The sections that are on the page now; the console derives them from the dry-run facts and the attached workspaces. */
export interface ActiveSections {
  agent: boolean;
  azureDevOps: boolean;
  /** The forge hosts of the git_pat sections on the page. */
  gitPATHosts: readonly string[];
  /** The `host/org` keys of the Git sections on the page (pushRuleKey). */
  pushKeys: readonly string[];
}

export const NO_ACTIVE_SECTIONS: ActiveSections = { agent: false, azureDevOps: false, gitPATHosts: [], pushKeys: [] };

/**
 * The key a push-rule override is filed under: the provider host and the
 * organisation, both canonical, or "" when they name no entry (Go:
 * types.PushRuleKey; push-rule-key.golden.json holds both to one table of cases).
 * The organisation is NFKC-normalised and ASCII-lowercased, and anything with
 * white space, a control or zero-width character, a trailing dot or a non-ASCII
 * character after normalisation names no entry.
 */
export function pushRuleKey(p: Pick<PushRuleOverride, "provider" | "org"> & { host?: string }): string {
  if (p.provider !== "github" && p.provider !== "azure_devops") return "";
  const org = p.org.normalize("NFKC");
  if (org === "" || org.length > 100 || org.endsWith(".") || /[^\x21-\x7e]/.test(org)) return "";
  const lower = org.toLowerCase();
  if (!/^[a-z0-9][a-z0-9._-]*$/.test(lower)) return "";
  const host = canonicalPushHost(p.provider, p.host ?? "");
  if (host === null) return "";
  // An {org}.visualstudio.com host names its own organisation; a different one contradicts it.
  if (host.org !== "" && host.org !== lower) return "";
  return `${host.host}/${lower}`;
}

/**
 * The host's canonical spelling (Go: canonicalPushHostOrg). A host outside ASCII
 * is refused before anything is lower-cased or normalised, ssh.github.com and
 * ssh.dev.azure.com fold into the https host, and an Azure DevOps remote's legacy
 * hosts fold into dev.azure.com (a subdomain host also names the organisation).
 */
function canonicalPushHost(provider: "github" | "azure_devops", host: string): { host: string; org: string } | null {
  if (host === "") return { host: provider === "github" ? "github.com" : "dev.azure.com", org: "" };
  if (/[^\x00-\x7f]/.test(host)) return null;
  let h = host.toLowerCase();
  if (h === "ssh.github.com") h = "github.com";
  else if (h === "ssh.dev.azure.com") h = "dev.azure.com";
  if (h.length > 253 || !/^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)*$/.test(h)) return null;
  let org = "";
  if (provider === "azure_devops") {
    if (h === "vs-ssh.visualstudio.com") h = "dev.azure.com";
    else if (h.endsWith(".visualstudio.com")) {
      org = h.slice(0, -".visualstudio.com".length);
      h = "dev.azure.com";
    }
  }
  return { host: h, org };
}

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
  const hosts = o.hosts;
  if (hosts) {
    for (const h of hosts.add_allowed ?? []) out.push({ kind: h.includes("*") ? "run_host_wildcard" : "run_host_allow", op: "add", subject: h });
    for (const h of hosts.add_denied ?? []) out.push({ kind: "run_host_deny", op: "add", subject: h });
  }
  const eg = o.egress;
  if (eg?.first_use_approval) out.push({ kind: "first_use_approval", op: "set", subject: eg.first_use_approval });
  if (eg?.allow_all_egress !== undefined) out.push({ kind: "allow_all_egress", op: "set", subject: "allow_all_egress" });
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

/** The run-mode carriers of the request body: all of them, or none while no experience is chosen. */
export interface RunModeWire {
  experience?: Experience;
  workload?: RunWorkload;
  tools?: IncludedTool[];
  startup?: RunStartup;
  start_folder?: StartFolder;
  no_repositories_or_drives?: true;
}

/**
 * The run-mode fields to send. Without an experience none are sent, because the
 * server never infers a mode and refuses a carrier without one; the launch gate
 * holds Launch until the person chooses. Under Background the harness the agent
 * task runs is the one included tool and there is no startup; under Interactive
 * there is no workload. What the draft holds for the other mode is kept, not sent.
 */
export function buildRunModeWire(mode: RunModeDraft | undefined): RunModeWire {
  if (!mode?.experience) return {};
  const out: RunModeWire = { experience: mode.experience };
  if (mode.experience === "background") {
    if (mode.workload) out.workload = mode.workload;
    // Every included tool is sent, never filtered: a tool a background run does not
    // use is the person's to remove (runModeBlocker holds Launch on it), and the
    // server refuses it by name, so nothing the draft holds is dropped unseen.
    if (mode.tools.length) out.tools = mode.tools;
  } else {
    if (mode.tools.length) out.tools = mode.tools;
    if (mode.startup) out.startup = mode.startup;
  }
  if (mode.startFolder) out.start_folder = mode.startFolder;
  if (mode.noRepositoriesOrDrives) out.no_repositories_or_drives = true;
  return out;
}

/** What the draft keeps for the mode it is not in, so a panel can say it is not used ("Not used by a background task."). */
export function inactiveRunMode(mode: RunModeDraft): Partial<Pick<RunModeDraft, "workload" | "startup" | "tools">> {
  if (mode.experience === "background") {
    const extra = unusedBackgroundTools(mode);
    return extra.length ? { startup: mode.startup, tools: extra } : { startup: mode.startup };
  }
  if (mode.experience === "interactive") return { workload: mode.workload };
  return {};
}

/** The included tools a background run does not use: all of them for a command, all but the task's harness for an agent task. */
export function unusedBackgroundTools(mode: RunModeDraft): IncludedTool[] {
  const agent = mode.workload?.kind === "agent_task" ? mode.workload.agent : undefined;
  return mode.tools.filter((t) => t.id !== agent);
}

/** Removing the startup harness clears the startup visibly, and never substitutes another. */
export function withoutTool(mode: RunModeDraft, id: string): { mode: RunModeDraft; clearedStartup: boolean } {
  const clearedStartup = mode.startup?.kind === "harness" && mode.startup.tool === id;
  return { mode: { ...mode, tools: mode.tools.filter((t) => t.id !== id), startup: clearedStartup ? undefined : mode.startup }, clearedStartup };
}

/** What blocks the run-mode choices from launching, as the server's own sentence and reason; null when none does. */
export function runModeBlocker(mode: RunModeDraft | undefined): { reason: string; text: string } | null {
  if (!mode?.experience) return { reason: RUN_MODE_REASON.REQUIRED, text: RUN_MODE_REFUSAL.RUN_MODE_REQUIRED() };
  if (mode.experience === "background" && !mode.workload) return { reason: RUN_MODE_REASON.REQUIRED, text: RUN_MODE_REFUSAL.RUN_WORKLOAD_REQUIRED() };
  if (mode.experience === "background" && unusedBackgroundTools(mode).length) {
    const instead = mode.workload?.kind === "command" ? "A command runs no harness." : "A background agent task includes only the harness it runs, which is workload.agent.";
    return { reason: RUN_MODE_REASON.CONFLICT, text: RUN_MODE_REFUSAL.RUN_MODE_CONFLICT("tools", instead) };
  }
  const s = mode.experience === "interactive" ? mode.startup : undefined;
  if (s?.kind === "harness" && !mode.tools.some((t) => t.id === s.tool && t.kind === "harness")) {
    return { reason: RUN_MODE_REASON.CONFLICT, text: RUN_MODE_REFUSAL.STARTUP_TOOL_UNKNOWN(s.tool ?? "") };
  }
  return null;
}

/** The documented mapping from an older client's input (Go: CreateRunRequest.EffectiveExperience). */
export function effectiveExperience(input: { experience?: Experience; interactive?: boolean; task?: string }): Experience {
  if (input.experience) return input.experience;
  return input.interactive || !input.task?.trim() ? "interactive" : "background";
}

/** The contract fields of the request body, each present only when the person chose it. */
export interface RunContractWire extends RunModeWire {
  placement?: PlacementValue;
  runner_id?: string;
  runner_pool_id?: string;
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
  if (runner.poolId) wire.runner_pool_id = runner.poolId;
  if (runner.imageRef) wire.allowed_image = runner.imageRef;
  const resources = resourcesWire(runner.cpus, runner.memoryMiB);
  if (resources) wire.resources = resources;
  const overrides = buildOverrides(draft.access.overrides, active);
  if (overrides) wire.overrides = overrides;
  return { ...wire, ...buildRunModeWire(draft.mode) };
}
