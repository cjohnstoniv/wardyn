/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The walls form: one field per knob a person may set, shared by Your walls and the wall presets card. It holds
// the form model (toForm and toSpec, a lossless round trip of a PersonalWallsSpec) and the controlled form itself;
// the callers own loading, saving and their own buttons.
import * as React from "react";
import { Plus, Trash2, X } from "lucide-react";
import { type PersonalWallsSpec, type WithheldGrant } from "../../../lib/api/walls";
import { ADO_CAPABILITIES, ADO_CAPABILITY_GROUPS } from "../../../lib/ado-capabilities";
import { adoCapName, adoGroupName } from "../../../lib/ado-access-copy";
import { APPROVE_EACH_REASON, CC_ORDER, canApproveEach, toolRulesProblem, type ConfinementClass, type FirstUseMode } from "../../../lib/types";
import { idleDisplay, isRead, type FormSection } from "../../../lib/wall-preset-words";
import { Button } from "../../ui/button";
import { Checkbox } from "../../ui/checkbox";
import { Input } from "../../ui/input";
import { RadioGroup, RadioGroupItem } from "../../ui/radio-group";
import { cn } from "../../ui/utils";
import { GITHUB_CAPABILITIES, GITHUB_CAPABILITY_GROUPS, GITHUB_PUSH_NAME, YOUR_WALLS as T } from "../../wardyn/copy/personal-walls";
import { RECORDING_LEVEL } from "../../wardyn/copy/recording-level";
import { Switch } from "../../wardyn/form-primitives";
import { ConfinementChip, Chip } from "../../wardyn/primitives";
import { Segmented } from "../../wardyn/segmented";

export type Direction = "paste_in" | "copy_out" | "upload" | "download";
export const DIRECTIONS: { id: Direction; label: string }[] = [
  { id: "paste_in", label: T.PASTE_IN },
  { id: "copy_out", label: T.COPY_OUT },
  { id: "upload", label: T.UPLOAD },
  { id: "download", label: T.DOWNLOAD },
];
const UNLISTED: { value: FirstUseMode | ""; label: string }[] = [
  { value: "", label: T.UNLISTED_ORG },
  { value: "always_deny", label: T.UNLISTED_BLOCK },
  { value: "deny_with_review", label: T.UNLISTED_ASK },
  { value: "wait_for_review", label: T.UNLISTED_HOLD },
];

export type IdleUnit = "seconds" | "minutes";
export type WallToolEffect = "hold" | "approve_each" | "deny";

/**
 * A tool rule or a path as a row. `fresh` is a row added in this edit and not yet typed into: Save drops it when it is
 * still blank. A row that held text, from the server or typed here, and was then cleared is never dropped: it blocks Save.
 */
export interface ToolRow {
  id: number;
  tool: string;
  effect: WallToolEffect;
  fresh: boolean;
}
export interface PathRow {
  id: number;
  path: string;
  fresh: boolean;
}

let nextRowId = 0;
const newToolRow = (tool: string, effect: WallToolEffect, fresh: boolean): ToolRow => ({ id: ++nextRowId, tool, effect, fresh });
const newPathRow = (path: string, fresh: boolean): PathRow => ({ id: ++nextRowId, path, fresh });

// The form, one field per knob the card shows. `allowAllOff` keeps a walls document's own
// allow_all_egress: false (written with the CLI) across a save, as the card has no control for it.
export interface Form {
  only: boolean;
  allowed: string[];
  allowedText: string;
  denied: string[];
  deniedText: string;
  firstUse: FirstUseMode | undefined;
  floor: ConfinementClass | undefined;
  blocked: Record<Direction, boolean>;
  withheld: WithheldGrant[];
  allowAllOff: boolean;
  /** push_rules.deny_all: refuse every push. */
  denyAll: boolean;
  /** github_capabilities: a present list (Only these capabilities) and what is checked. */
  githubOnly: boolean;
  github: string[];
  /** The github_capabilities list as loaded, so a save that does not change the set keeps its stored order. */
  githubLoaded: string[];
  /** azure_devops_capabilities, the same. */
  adoOnly: boolean;
  ado: string[];
  adoLoaded: string[];
  tools: ToolRow[];
  /** push_rules.deny_paths. */
  paths: PathRow[];
  recording: "terminal" | "screen_actions" | undefined;
  /** auto_stop_after_sec as a whole number (text, so a bad entry can be said so) and its unit. */
  idleOn: boolean;
  idleText: string;
  idleUnit: IdleUnit;
}

export function toForm(s: PersonalWallsSpec): Form {
  const d = s.desktop;
  const idle = s.auto_stop_after_sec === undefined ? { n: 30, unit: "minutes" as IdleUnit } : idleDisplay(s.auto_stop_after_sec);
  return {
    only: s.allowed_domains !== undefined,
    allowed: s.allowed_domains ?? [],
    allowedText: "",
    denied: s.denied_domains ?? [],
    deniedText: "",
    firstUse: s.first_use_approval,
    floor: s.min_confinement_class,
    blocked: { paste_in: d?.paste_in === "blocked", copy_out: d?.copy_out === "blocked", upload: d?.upload === "blocked", download: d?.download === "blocked" },
    withheld: s.withheld_grants ?? [],
    allowAllOff: s.allow_all_egress === false,
    denyAll: s.push_rules?.deny_all === true,
    githubOnly: s.github_capabilities !== undefined,
    github: s.github_capabilities ?? [],
    githubLoaded: s.github_capabilities ?? [],
    adoOnly: s.azure_devops_capabilities !== undefined,
    ado: s.azure_devops_capabilities ?? [],
    adoLoaded: s.azure_devops_capabilities ?? [],
    tools: (s.tool_rules ?? []).map((r) => newToolRow(r.tool, r.effect === "deny" || r.effect === "approve_each" ? r.effect : "hold", false)),
    paths: (s.push_rules?.deny_paths ?? []).map((p) => newPathRow(p, false)),
    recording: s.recording === "terminal" || s.recording === "screen_actions" ? s.recording : undefined,
    idleOn: s.auto_stop_after_sec !== undefined,
    idleText: String(idle.n),
    idleUnit: idle.unit,
  };
}

/** The idle stop in seconds, or null when the text is not a whole number of 1 or more. */
export function idleSeconds(f: Pick<Form, "idleText" | "idleUnit">): number | null {
  const text = f.idleText.trim();
  if (!/^\d+$/.test(text)) return null;
  const n = Number(text);
  const sec = n * (f.idleUnit === "minutes" ? 60 : 1);
  return n >= 1 && Number.isSafeInteger(sec) ? sec : null;
}

const blankRow = (text: string, fresh: boolean) => text.trim() === "" && !fresh;
const filled = (rows: ToolRow[]) => rows.filter((r) => r.tool.trim() !== "");

/** What keeps Save off, by section: the form checks what it can before the server is asked. */
export function formProblems(f: Form): { github?: string; ado?: string; tools?: string; idle?: string; blankTools: Set<number>; blankPaths: Set<number>; duplicateTools: Set<number> } {
  const blankTools = new Set(f.tools.filter((r) => blankRow(r.tool, r.fresh)).map((r) => r.id));
  const blankPaths = new Set(f.paths.filter((r) => blankRow(r.path, r.fresh)).map((r) => r.id));
  const seen = new Map<string, number>();
  const duplicateTools = new Set<number>();
  for (const r of filled(f.tools)) {
    const first = seen.get(r.tool);
    if (first === undefined) seen.set(r.tool, r.id);
    else duplicateTools.add(first).add(r.id);
  }
  return {
    github: f.githubOnly && f.github.length === 0 ? T.CAPS_EMPTY : undefined,
    ado: f.adoOnly && f.ado.length === 0 ? T.CAPS_EMPTY : undefined,
    tools: toolRulesProblem(filled(f.tools).map((r) => ({ tool: r.tool, effect: r.effect }))) ?? undefined,
    idle: f.idleOn && idleSeconds(f) === null ? T.IDLE_PROBLEM : undefined,
    blankTools,
    blankPaths,
    duplicateTools,
  };
}

/** True while a check above fails: the caller keeps Save off. */
export function formBlocked(f: Form): boolean {
  const p = formProblems(f);
  return !!(p.github || p.ado || p.tools || p.idle) || p.blankTools.size > 0 || p.blankPaths.size > 0;
}

// A typed site is a bare host: lower-case, with any scheme, everything from the first "/", "?" or "#", and a port dropped.
// A bracketed IPv6 literal keeps its brackets and loses only the port after them; an unbracketed one has no port to drop.
const siteOf = (text: string): string => {
  const host = text.trim().toLowerCase().replace(/^[a-z][a-z0-9+.-]*:\/\//, "").replace(/[/?#].*$/, "");
  if (host.startsWith("[")) return host.replace(/^(\[[^\]]*\]).*$/, "$1");
  return host.split(":").length === 2 ? host.replace(/:\d*$/, "") : host;
};

// A site typed and not yet entered still counts: Save folds it in.
export const withText = (list: string[], text: string): string[] => {
  const site = siteOf(text);
  return site && !list.includes(site) ? [...list, site] : list;
};

// The stored list as it was stored while the picked set is the same set; a changed selection takes the catalogue's order.
const capabilityList = (order: readonly string[], picked: string[], loaded: string[]) => {
  const same = new Set(picked).size === new Set(loaded).size && picked.every((c) => loaded.includes(c));
  return same ? [...loaded] : order.filter((c) => picked.includes(c));
};

export function toSpec(f: Form): PersonalWallsSpec {
  const spec: PersonalWallsSpec = {};
  if (f.only) spec.allowed_domains = withText(f.allowed, f.allowedText);
  const denied = withText(f.denied, f.deniedText);
  if (denied.length > 0) spec.denied_domains = denied;
  if (f.allowAllOff && !f.only) spec.allow_all_egress = false;
  if (f.firstUse) spec.first_use_approval = f.firstUse;
  if (f.floor) spec.min_confinement_class = f.floor;
  const blocked = DIRECTIONS.filter((d) => f.blocked[d.id]);
  if (blocked.length > 0) spec.desktop = Object.fromEntries(blocked.map((d) => [d.id, "blocked" as const]));
  if (f.withheld.length > 0) spec.withheld_grants = f.withheld;
  if (f.githubOnly) spec.github_capabilities = capabilityList(GITHUB_CAPABILITIES, f.github, f.githubLoaded);
  if (f.adoOnly) spec.azure_devops_capabilities = capabilityList(ADO_CAPABILITIES.map((c) => c.cap), f.ado, f.adoLoaded);
  // A row added in this edit and left blank is dropped; one that held text and was cleared blocks Save (formBlocked).
  const tools = f.tools.filter((r) => !(r.fresh && r.tool.trim() === ""));
  if (tools.length > 0) spec.tool_rules = tools.map((r) => ({ tool: r.tool, effect: r.effect }));
  const paths = f.paths.filter((r) => !(r.fresh && r.path.trim() === "")).map((r) => r.path);
  if (paths.length > 0 || f.denyAll) spec.push_rules = { ...(paths.length > 0 && { deny_paths: paths }), ...(f.denyAll && { deny_all: true }) };
  if (f.recording) spec.recording = f.recording;
  const idle = f.idleOn ? idleSeconds(f) : null;
  if (idle !== null) spec.auto_stop_after_sec = idle;
  return spec;
}

/** The sites of one list: a chip for each, and the field a new one is typed into (Enter or a comma enters it). */
function SiteList({ label, sites, text, onSites, onText, errorId }: { label: string; sites: string[]; text: string; onSites: (s: string[]) => void; onText: (t: string) => void; errorId?: string }) {
  const enter = () => {
    onSites(withText(sites, text));
    onText("");
  };
  return (
    <div className="flex flex-col gap-1.5">
    <div className="flex flex-wrap items-center gap-1.5">
      {sites.map((site) => (
        <Chip key={site} mono className="gap-1">
          {site}
          <button type="button" aria-label={T.REMOVE_SITE(site)} className="text-muted-foreground hover:text-foreground" onClick={() => onSites(sites.filter((s) => s !== site))}>
            <X className="size-3" aria-hidden="true" />
          </button>
        </Chip>
      ))}
      <Input
        aria-label={label}
        aria-describedby={errorId}
        aria-invalid={errorId ? true : undefined}
        value={text}
        placeholder={T.ADD_SITE}
        className="h-8 w-44 font-mono text-xs"
        onChange={(e) => (e.target.value.endsWith(",") ? (onSites(withText(sites, e.target.value.slice(0, -1))), onText("")) : onText(e.target.value))}
        onKeyDown={(e) => {
          if (e.key !== "Enter") return;
          e.preventDefault();
          enter();
        }}
      />
    </div>
    <p className="text-xs text-muted-foreground">{T.SITE_HINT}</p>
    </div>
  );
}

export const KIND_OPTIONS = Object.keys(T.KINDS);
export const LABEL = "text-sm font-medium text-foreground";

// An error is tied to the input or group it describes by its id (aria-describedby there); the ids come from useId,
// so two forms on one page never share one.
const SectionError = ({ id, text }: { id: string; text: string }) => (
  <p id={id} role="alert" className="text-xs text-danger">
    {text}
  </p>
);

/** The aria-describedby value for the error ids that are showing, or nothing. */
const describedBy = (...ids: (string | false | undefined)[]) => ids.filter(Boolean).join(" ") || undefined;

interface CapGroup {
  name: string;
  caps: { cap: string; name: string; wire?: string }[];
}

const GITHUB_GROUPS: CapGroup[] = GITHUB_CAPABILITY_GROUPS.map((g) => ({ name: g.name, caps: g.caps.map((c) => ({ cap: c.cap, name: c.label, wire: c.cap })) }));
const ADO_GROUPS: CapGroup[] = ADO_CAPABILITY_GROUPS.map((g) => ({
  name: adoGroupName(g.id),
  caps: ADO_CAPABILITIES.filter((c) => c.group === g.id).map((c) => ({ cap: c.cap, name: adoCapName(c.cap) })),
}));

const GROUP_HEAD = "bg-muted px-3 py-1.5 text-xs font-semibold text-foreground";

/**
 * One forge's capability list: No limit, or Only these capabilities with a grouped checklist. A wall never grants, so the
 * list only names what the run may keep; nothing is checked until the person checks it.
 */
function CapabilityList({ id, title, forge, pushName, groups, only, picked, onChange, serverError, problem }: {
  id: string;
  title: string;
  forge: string;
  /** The push capability's short name, for the line under a list that leaves it out. */
  pushName: string;
  groups: CapGroup[];
  only: boolean;
  picked: string[];
  onChange: (only: boolean, picked: string[]) => void;
  serverError?: string;
  problem?: string;
}) {
  // Two forms can be on the page at once (Your walls and a preset dialog): the ids are this list's own.
  const uid = React.useId();
  const all = groups.flatMap((g) => g.caps.map((c) => c.cap));
  const toggle = (cap: string, on: boolean) => onChange(true, on ? [...picked, cap] : picked.filter((c) => c !== cap));
  return (
    <div className="flex flex-col gap-2" data-testid={id}>
      <div className="flex flex-wrap items-baseline gap-x-3">
        <span className={LABEL}>{title}</span>
        {only && <span className="text-xs text-muted-foreground">{T.CAPS_COUNT(all.filter((c) => picked.includes(c)).length, all.length)}</span>}
      </div>
      {serverError && <SectionError id={`${uid}-server`} text={serverError} />}
      <RadioGroup
        className="gap-2"
        aria-label={title}
        aria-describedby={describedBy(serverError && `${uid}-server`, only && problem && `${uid}-problem`)}
        aria-invalid={serverError || (only && problem) ? true : undefined}
        value={only ? "only" : "none"}
        onValueChange={(v) => onChange(v === "only", picked)}
      >
        <label className="flex items-center gap-2 text-sm">
          <RadioGroupItem value="none" />
          {T.LIMIT_NONE}
        </label>
        <label className="flex items-center gap-2 text-sm">
          <RadioGroupItem value="only" />
          {T.CAPS_ONLY}
        </label>
      </RadioGroup>
      {only && (
        <div className="flex flex-col gap-2 pl-6">
          <p className="text-xs text-muted-foreground">{T.CAPS_HELP}</p>
          <div>
            <Button type="button" variant="ghost" size="sm" className="px-1" onClick={() => onChange(true, all.filter(isRead))}>
              {T.CAPS_READS_ONLY}
            </Button>
          </div>
          {groups.map((g) => (
            <fieldset key={g.name} className="min-w-0 overflow-hidden rounded-md border border-border">
              <legend className="sr-only">{g.name}</legend>
              <div aria-hidden="true" className={GROUP_HEAD}>
                {g.name}
              </div>
              <ul>
                {g.caps.map((c) => (
                  <li key={c.cap} className="flex items-start gap-2.5 border-t border-border px-3 py-2">
                    <Checkbox id={`${uid}-${c.cap}`} className="mt-0.5" aria-label={c.name} checked={picked.includes(c.cap)} onCheckedChange={(on) => toggle(c.cap, on === true)} />
                    <label htmlFor={`${uid}-${c.cap}`} className="min-w-0 text-sm leading-snug">
                      <span className={picked.includes(c.cap) ? "text-foreground" : "text-muted-foreground"}>{c.name}</span>
                      {c.wire && <span className="block font-mono text-xs text-muted-foreground">{c.wire}</span>}
                    </label>
                  </li>
                ))}
              </ul>
            </fieldset>
          ))}
          {problem && <SectionError id={`${uid}-problem`} text={problem} />}
          {!picked.includes("code_write") && (
            <>
              <p className="text-xs text-warning">{T.CAPS_NO_PUSH(pushName, forge)}</p>
              <p className="text-xs text-muted-foreground">{T.CAPS_NO_PUSH_BROKER_OFF}</p>
            </>
          )}
        </div>
      )}
    </div>
  );
}
const RULE_ROW = "grid grid-cols-[minmax(0,1fr)_9rem_2rem] items-center gap-2 max-[399px]:grid-cols-[minmax(0,1fr)_2rem]";
const EFFECT_TONE: Record<WallToolEffect, string> = { hold: "text-warning", approve_each: "text-warning", deny: "text-danger" };

/** Tool rules: a tool and what to do with it: hold, approve each call, or deny. No default row: `*` is a rule like any other. */
function ToolRules({ form, edit, serverError, problem }: { form: Form; edit: (patch: Partial<Form>) => void; serverError?: string; problem: ReturnType<typeof formProblems> }) {
  const uid = React.useId();
  const patch = (id: number, p: Partial<ToolRow>) => edit({ tools: form.tools.map((r) => (r.id === id ? { ...r, ...p } : r)) });
  return (
    <div className="flex flex-col gap-2" role="group" aria-label={T.TOOLS} aria-describedby={describedBy(serverError && `${uid}-server`, problem.tools && `${uid}-problem`)} data-testid="walls-tool-rules">
      <span className={LABEL}>{T.TOOLS}</span>
      {serverError && <SectionError id={`${uid}-server`} text={serverError} />}
      <p className="text-xs text-muted-foreground">{T.TOOLS_HELP}</p>
      {form.tools.length === 0 ? (
        <p className="text-xs italic text-muted-foreground">{T.TOOLS_NONE}</p>
      ) : (
        <div className="flex flex-col">
          <div className={cn(RULE_ROW, "border-b border-border pb-1.5 text-xs font-semibold uppercase text-muted-foreground max-[399px]:hidden")}>
            <span>{T.TOOLS_TOOL}</span>
            <span>{T.TOOLS_EFFECT}</span>
            <span />
          </div>
          {form.tools.map((r, i) => {
            const blank = problem.blankTools.has(r.id);
            const invalid = blank || problem.duplicateTools.has(r.id);
            return (
              <div key={r.id} className="border-b border-border py-2 last:border-b-0">
                <div className={RULE_ROW}>
                  <Input
                    aria-label={T.TOOLS_TOOL_N(i + 1)}
                    aria-invalid={invalid || undefined}
                    aria-describedby={describedBy(blank && `${uid}-blank-${r.id}`, !blank && invalid && problem.tools && `${uid}-problem`)}
                    value={r.tool}
                    spellCheck={false}
                    placeholder={T.TOOLS_PLACEHOLDER}
                    className="h-8 font-mono text-sm max-[399px]:col-span-2"
                    onChange={(e) => patch(r.id, { tool: e.target.value, fresh: false })}
                  />
                  <select
                    aria-label={T.TOOLS_EFFECT_N(i + 1)}
                    value={r.effect}
                    aria-describedby={canApproveEach(r.tool) ? undefined : `${uid}-ae-${r.id}`}
                    onChange={(e) => patch(r.id, { effect: e.target.value as WallToolEffect })}
                    className={cn("h-8 w-full rounded-md border border-input bg-input-background px-2 font-mono text-sm font-medium", EFFECT_TONE[r.effect])}
                  >
                    {(["hold", "approve_each", "deny"] as const).map((e) => (
                      <option key={e} value={e} disabled={e === "approve_each" && !canApproveEach(r.tool)} className="text-foreground">
                        {e}
                      </option>
                    ))}
                  </select>
                  <Button type="button" variant="ghost" size="icon" className="size-8" aria-label={T.TOOLS_REMOVE_N(i + 1)} onClick={() => edit({ tools: form.tools.filter((x) => x.id !== r.id) })}>
                    <Trash2 className="size-4" aria-hidden="true" />
                  </Button>
                </div>
                {blank && <SectionError id={`${uid}-blank-${r.id}`} text={T.TOOLS_BLANK} />}
                {!canApproveEach(r.tool) && (
                  <p id={`${uid}-ae-${r.id}`} className="mt-1 text-xs text-muted-foreground">
                    <span className="font-mono text-foreground">approve_each</span> is greyed: {APPROVE_EACH_REASON}
                  </p>
                )}
              </div>
            );
          })}
        </div>
      )}
      <p className="text-xs text-muted-foreground">{T.TOOLS_STAR_HINT}</p>
      {problem.tools && <SectionError id={`${uid}-problem`} text={problem.tools} />}
      <div>
        <Button type="button" variant="ghost" size="sm" className="px-1" onClick={() => edit({ tools: [...form.tools, newToolRow("", "hold", true)] })}>
          <Plus className="size-4" aria-hidden="true" /> {T.TOOLS_ADD}
        </Button>
      </div>
    </div>
  );
}

/** Pushes: Refuse every push, and the paths never pushed under it. The paths stay editable, dimmed, while the switch is on. */
function PushRules({ form, edit, serverError, problem }: { form: Form; edit: (patch: Partial<Form>) => void; serverError?: string; problem: ReturnType<typeof formProblems> }) {
  const uid = React.useId();
  const patch = (id: number, path: string) => edit({ paths: form.paths.map((r) => (r.id === id ? { ...r, path, fresh: false } : r)) });
  const listed = form.paths.some((r) => r.path.trim() !== "");
  return (
    <div className="flex flex-col gap-2" role="group" aria-label={T.PUSHES} aria-describedby={describedBy(serverError && `${uid}-server`)}>
      <span className={LABEL}>{T.PUSHES}</span>
      {serverError && <SectionError id={`${uid}-server`} text={serverError} />}
      <label className="flex items-start gap-2 text-sm">
        <Switch checked={form.denyAll} onChange={(denyAll) => edit({ denyAll })} label={T.REFUSE_PUSH} className="mt-0.5" />
        <span className="flex flex-col gap-0.5">
          <span className="font-medium text-foreground">{T.REFUSE_PUSH}</span>
          <span className="text-xs text-muted-foreground">{T.REFUSE_PUSH_HELP}</span>
        </span>
      </label>
      <div className="flex flex-col gap-1.5" data-testid="walls-deny-paths">
        {/* Dimmed while every push is refused; the notes below sit outside it so they stay readable. */}
        <div className={cn("flex flex-col gap-1.5", form.denyAll && "opacity-60")} data-dimmed={form.denyAll || undefined}>
          <span className="text-sm font-medium text-foreground">{T.PATHS}</span>
          <p className="text-xs text-muted-foreground">{T.PATHS_HELP}</p>
          {form.paths.length === 0 && <p className="text-xs italic text-muted-foreground">{T.PATHS_NONE}</p>}
          {form.paths.map((r, i) => (
            <div key={r.id}>
              <div className="flex items-start gap-2">
                <Input
                  aria-label={T.PATHS_PATH_N(i + 1)}
                  aria-invalid={problem.blankPaths.has(r.id) || undefined}
                  aria-describedby={describedBy(problem.blankPaths.has(r.id) && `${uid}-blank-${r.id}`)}
                  value={r.path}
                  spellCheck={false}
                  className="h-8 flex-1 font-mono text-sm"
                  onChange={(e) => patch(r.id, e.target.value)}
                />
                <Button type="button" variant="ghost" size="icon" className="size-8" aria-label={T.PATHS_REMOVE_N(i + 1)} onClick={() => edit({ paths: form.paths.filter((x) => x.id !== r.id) })}>
                  <Trash2 className="size-4" aria-hidden="true" />
                </Button>
              </div>
              {problem.blankPaths.has(r.id) && <SectionError id={`${uid}-blank-${r.id}`} text={T.PATHS_BLANK} />}
            </div>
          ))}
          <div>
            <Button type="button" variant="ghost" size="sm" className="px-1" onClick={() => edit({ paths: [...form.paths, newPathRow("", true)] })}>
              <Plus className="size-4" aria-hidden="true" /> {T.PATHS_ADD}
            </Button>
          </div>
        </div>
        {listed && <p className="text-xs text-muted-foreground">{T.PATHS_ADO_NOTE}</p>}
        {listed && <p className="text-xs text-muted-foreground">{T.PATHS_BROKER_NOTE}</p>}
        {listed && form.denyAll && <p className="text-xs text-muted-foreground">{T.PATHS_OFF_NOTE}</p>}
      </div>
    </div>
  );
}

const RECORDING_CHOICES = RECORDING_LEVEL.LEVELS.filter((l) => l.value !== "off");

/** Record at least: a floor, never a way to record less. */
function RecordingFloor({ form, edit, serverError }: { form: Form; edit: (patch: Partial<Form>) => void; serverError?: string }) {
  const uid = React.useId();
  const chosen = RECORDING_CHOICES.find((l) => l.value === form.recording);
  return (
    <div className="flex flex-col gap-2">
      <span className={LABEL}>{T.RECORDING}</span>
      {serverError && <SectionError id={`${uid}-server`} text={serverError} />}
      <RadioGroup className="flex flex-wrap gap-x-4 gap-y-2" aria-label={T.RECORDING} aria-describedby={describedBy(serverError && `${uid}-server`)} aria-invalid={serverError ? true : undefined} value={form.recording ?? "none"} onValueChange={(v) => edit({ recording: v === "none" ? undefined : (v as Form["recording"]) })}>
        <label className="flex items-center gap-2 text-sm">
          <RadioGroupItem value="none" />
          {T.RECORDING_NONE}
        </label>
        {RECORDING_CHOICES.map((l) => (
          <label key={l.value} className="flex items-center gap-2 text-sm">
            <RadioGroupItem value={l.value} />
            {l.label}
          </label>
        ))}
      </RadioGroup>
      <p className="text-xs text-muted-foreground">{chosen ? chosen.description : T.RECORDING_UNSET}</p>
    </div>
  );
}

/** Stop when idle: No limit, or a whole number of seconds or minutes. A stored value shows exactly as it is stored. */
function IdleStop({ form, edit, serverError, problem }: { form: Form; edit: (patch: Partial<Form>) => void; serverError?: string; problem?: string }) {
  const uid = React.useId();
  const problemShown = form.idleOn && !!problem;
  return (
    <div className="flex flex-col gap-2">
      <span className={LABEL}>{T.IDLE}</span>
      {serverError && <SectionError id={`${uid}-server`} text={serverError} />}
      <RadioGroup className="gap-2" aria-label={T.IDLE} aria-describedby={describedBy(serverError && `${uid}-server`)} aria-invalid={serverError ? true : undefined} value={form.idleOn ? "after" : "none"} onValueChange={(v) => edit({ idleOn: v === "after" })}>
        <label className="flex items-center gap-2 text-sm">
          <RadioGroupItem value="none" />
          {T.LIMIT_NONE}
        </label>
        <div className="flex flex-wrap items-center gap-x-2 gap-y-1.5 text-sm">
          <label className="flex items-center gap-2">
            <RadioGroupItem value="after" />
            {T.IDLE_AFTER}
          </label>
          <Input
            aria-label={T.IDLE_NUMBER}
            aria-invalid={problemShown || undefined}
            aria-describedby={describedBy(problemShown && `${uid}-problem`)}
            inputMode="numeric"
            value={form.idleText}
            disabled={!form.idleOn}
            className="h-8 w-20 text-right"
            onChange={(e) => edit({ idleText: e.target.value })}
          />
          <select
            aria-label={T.IDLE_UNIT_LABEL}
            value={form.idleUnit}
            disabled={!form.idleOn}
            onChange={(e) => edit({ idleUnit: e.target.value as IdleUnit })}
            className="h-8 rounded-md border border-input bg-input-background px-2 text-sm disabled:opacity-50"
          >
            <option value="seconds">seconds</option>
            <option value="minutes">minutes</option>
          </select>
          <span>{T.IDLE_SUFFIX}</span>
        </div>
      </RadioGroup>
      {problemShown && <SectionError id={`${uid}-problem`} text={problem} />}
      <p className="text-xs text-muted-foreground">{T.IDLE_HELP}</p>
    </div>
  );
}

/**
 * The controlled form. `onEdit` merges a patch into the caller's Form. `errors` puts a server sentence under the
 * section it names; the checks the form can make itself (formProblems) show under theirs and keep Save off.
 */
export function WallsForm({ form, onEdit: edit, errors }: { form: Form; onEdit: (patch: Partial<Form>) => void; errors?: Partial<Record<FormSection, string>> }) {
  const problem = formProblems(form);
  // Two forms can be on the page at once (Your walls and a preset dialog): the error ids are this form's own.
  const uid = React.useId();
  const eid = (section: FormSection) => `${uid}-${section}`;
  // The unlisted-sites choice was filled in by picking "Only these sites", not chosen by the person.
  const [autoFirstUse, setAutoFirstUse] = React.useState(false);
  const unnamed = KIND_OPTIONS.filter((k) => !form.withheld.some((g) => g.kind === k));
  return (
    <div className="flex flex-col gap-[18px]">
      <div className="flex flex-col gap-2">
        <span className={LABEL}>{T.SITES}</span>
        {errors?.sites && <SectionError id={eid("sites")} text={errors.sites} />}
        <RadioGroup
          className="gap-2"
          aria-label={T.SITES}
          aria-describedby={describedBy(errors?.sites && eid("sites"))}
          aria-invalid={errors?.sites ? true : undefined}
          value={form.only ? "only" : "any"}
          // The unlisted sites of a list are refused unless the person chose otherwise.
          onValueChange={(v) => {
            if (v === "only") {
              setAutoFirstUse(form.firstUse === undefined || autoFirstUse);
              edit({ only: true, firstUse: form.firstUse ?? "always_deny" });
            } else {
              setAutoFirstUse(false);
              edit(autoFirstUse ? { only: false, firstUse: undefined } : { only: false });
            }
          }}
        >
          <label className="flex items-center gap-2 text-sm">
            <RadioGroupItem value="any" />
            {T.SITES_ANY}
          </label>
          <label className="flex items-center gap-2 text-sm">
            <RadioGroupItem value="only" />
            {T.SITES_ONLY}
          </label>
        </RadioGroup>
        {form.only && (
          <div className="flex flex-col gap-1.5 pl-6">
            <SiteList errorId={errors?.sites ? eid("sites") : undefined} label={T.SITES_ONLY} sites={form.allowed} text={form.allowedText} onSites={(allowed) => edit({ allowed })} onText={(allowedText) => edit({ allowedText })} />
            <p className="text-xs text-muted-foreground">{T.SITES_ONLY_HINT}</p>
          </div>
        )}
      </div>
      <div className="flex flex-col gap-2">
        <span className={LABEL}>{T.NEVER}</span>
        {errors?.never && <SectionError id={eid("never")} text={errors.never} />}
        <SiteList errorId={errors?.never ? eid("never") : undefined} label={T.NEVER} sites={form.denied} text={form.deniedText} onSites={(denied) => edit({ denied })} onText={(deniedText) => edit({ deniedText })} />
      </div>
      <div className="flex flex-col gap-2">
        <span className={LABEL}>{T.UNLISTED}</span>
        {errors?.unlisted && <SectionError id={eid("unlisted")} text={errors.unlisted} />}
        <div role="group" aria-label={T.UNLISTED} aria-describedby={describedBy(errors?.unlisted && eid("unlisted"))}>
          <Segmented<FirstUseMode | "">
            stackOnPhone
            value={form.firstUse ?? ""}
            options={UNLISTED}
            onChange={(v) => {
              setAutoFirstUse(false);
              edit({ firstUse: v || undefined });
            }}
          />
        </div>
      </div>
      <div className="flex flex-col gap-2">
        <span className={LABEL}>{T.BARRIER}</span>
        {errors?.barrier && <SectionError id={eid("barrier")} text={errors.barrier} />}
        <RadioGroup className="flex flex-wrap gap-4" aria-label={T.BARRIER} aria-describedby={describedBy(errors?.barrier && eid("barrier"))} aria-invalid={errors?.barrier ? true : undefined} value={form.floor ?? "none"} onValueChange={(v) => edit({ floor: v === "none" ? undefined : (v as ConfinementClass) })}>
          <label className="flex items-center gap-2 text-sm">
            <RadioGroupItem value="none" />
            {T.BARRIER_NONE}
          </label>
          {CC_ORDER.map((cc) => (
            <label key={cc} className="flex items-center gap-2">
              <RadioGroupItem value={cc} />
              <ConfinementChip value={cc} />
            </label>
          ))}
        </RadioGroup>
      </div>
      <div className="flex flex-col gap-2">
        <span className={LABEL}>{T.CLIPBOARD}</span>
        <p className="text-xs text-muted-foreground">{T.CLIPBOARD_HINT}</p>
        {errors?.clipboard && <SectionError id={eid("clipboard")} text={errors.clipboard} />}
        <div className="flex flex-wrap gap-x-5 gap-y-2" role="group" aria-label={T.CLIPBOARD} aria-describedby={describedBy(errors?.clipboard && eid("clipboard"))}>
          {DIRECTIONS.map((d) => (
            <label key={d.id} className="flex items-center gap-2 text-sm">
              <Checkbox checked={form.blocked[d.id]} onCheckedChange={(on) => edit({ blocked: { ...form.blocked, [d.id]: on === true } })} />
              {d.label}
            </label>
          ))}
        </div>
      </div>
      <div className="flex flex-col gap-2">
        <span className={LABEL}>{T.CREDENTIALS}</span>
        {errors?.credentials && <SectionError id={eid("credentials")} text={errors.credentials} />}
        <div className="flex flex-wrap items-center gap-1.5" role="group" aria-label={T.CREDENTIALS} aria-describedby={describedBy(errors?.credentials && eid("credentials"))}>
          {form.withheld.map((g, i) => (
            <Chip key={`${g.kind}${i}`} className="gap-1">
              {T.KINDS[g.kind] ?? g.kind}
              <button type="button" aria-label={T.REMOVE_SITE(T.KINDS[g.kind] ?? g.kind)} className="text-muted-foreground hover:text-foreground" onClick={() => edit({ withheld: form.withheld.filter((_, j) => j !== i) })}>
                <X className="size-3" aria-hidden="true" />
              </button>
            </Chip>
          ))}
          {unnamed.length > 0 && (
            <select
              aria-label={T.CREDENTIALS}
              value=""
              className={cn("h-8 w-44 rounded-md border border-input bg-input-background px-2 text-sm")}
              onChange={(e) => e.target.value && edit({ withheld: [...form.withheld, { kind: e.target.value }] })}
            >
              <option value="">{T.ADD_KIND}</option>
              {unnamed.map((k) => (
                <option key={k} value={k}>
                  {T.KINDS[k]}
                </option>
              ))}
            </select>
          )}
        </div>
      </div>
      <CapabilityList
        id="walls-github"
        title={T.GITHUB_CAPS}
        forge="GitHub"
        pushName={GITHUB_PUSH_NAME}
        groups={GITHUB_GROUPS}
        only={form.githubOnly}
        picked={form.github}
        onChange={(githubOnly, github) => edit({ githubOnly, github })}
        serverError={errors?.github}
        problem={problem.github}
      />
      <CapabilityList
        id="walls-ado"
        title={T.ADO_CAPS}
        forge="Azure DevOps"
        pushName={adoCapName("code_write")}
        groups={ADO_GROUPS}
        only={form.adoOnly}
        picked={form.ado}
        onChange={(adoOnly, ado) => edit({ adoOnly, ado })}
        serverError={errors?.ado}
        problem={problem.ado}
      />
      <ToolRules form={form} edit={edit} serverError={errors?.tools} problem={problem} />
      <PushRules form={form} edit={edit} serverError={errors?.pushes} problem={problem} />
      <RecordingFloor form={form} edit={edit} serverError={errors?.recording} />
      <IdleStop form={form} edit={edit} serverError={errors?.idle} problem={problem.idle} />
    </div>
  );
}
