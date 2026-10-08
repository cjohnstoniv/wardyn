/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The Summary view of a policy: every key under the name the console already
// uses for it, in headed sections, each name shown once with its values listed
// beneath. Raw keys belong to the YAML and JSON views; a key this file does not
// know still shows, under its raw name, in "Other settings".
//
// The spec is read defensively. It may be a hand-typed draft, so a known key
// holding the wrong shape is shown raw rather than read as something it is not.
import * as React from "react";
import { adoAccessSummary, ADOAccessSummary } from "../ado-access-summary";
import { CC_META } from "../cc-meta";
import { GIT_PAT_SCOPE, POLICY_UI_APPS } from "../copy";
import { PENDING_NAME, POLICY_DOCUMENT as D } from "../copy/policy-document";
import { EFFECT_PAST, splitToolRules } from "../policy-tool-rules";
import { Chip } from "../primitives";
import { readPATScope } from "../../../lib/git-pat-scope";
import { asFirstUseMode } from "../../../lib/types";
import type { ConfinementClass, RunPolicySpec, ToolRule } from "../../../lib/types";
import type { PolicyPreviewPending } from "../../../lib/types/policy-preview";
import { POLICY_TAB, SUMMARY } from "../../screens/run-detail/policy-tab-copy";
import { toolRulesTail } from "./policy-facts";

export const REDACTED = "<redacted>";

export type PolicyMark = "added" | "removed";

/** Which entries launch changed, so the Summary can flag them (run page only). */
export interface PolicyChangeMarks {
  of(field: string, entry: string): PolicyMark | undefined;
  /** Entries launch took out of a list; they are no longer in the spec. */
  removed(field: string): string[];
}

/** Barrier facts that are not part of the spec. A draft requests; a run used. */
export interface PolicyFacts {
  usedClass?: ConfinementClass;
  requestedClass?: ConfinementClass;
}

type ChipSpec = { label: string; tone: "neutral" | "warning"; title?: string };
type Value = { text?: string; mono?: boolean; hidden?: boolean; mark?: PolicyMark; chips?: ChipSpec[] };
type Row = { label: string; raw?: boolean; values: Value[] };
type Group = { heading: string; intro?: string; rows: Row[]; items?: string[]; ado?: string[] };

type Bag = Record<string, unknown>;
const isBag = (v: unknown): v is Bag => v !== null && typeof v === "object" && !Array.isArray(v);
const asString = (v: unknown) => (typeof v === "string" && v ? v : undefined);
const asNumber = (v: unknown) => (typeof v === "number" && Number.isFinite(v) ? v : undefined);
const asBool = (v: unknown) => (typeof v === "boolean" ? v : undefined);
const asStrings = (v: unknown) => (Array.isArray(v) && v.every((x) => typeof x === "string") ? (v as string[]) : undefined);
const asBags = (v: unknown) => (Array.isArray(v) && v.every(isBag) ? (v as Bag[]) : undefined);
const asBag = (v: unknown) => (isBag(v) ? v : undefined);

const ccLabel = (cc: string) => CC_META[cc as ConfinementClass]?.label ?? cc;
const text = (t: string, extra?: Partial<Value>): Value => ({ text: t, ...extra });
const mono = (t: string, extra?: Partial<Value>): Value => ({ text: t, mono: true, ...extra });

export function cpuText(millis?: number): string {
  return millis ? SUMMARY.cpuValue(String(Number((millis / 1000).toFixed(2)))) : SUMMARY.standardLimit;
}

export const mibText = (n?: number) => (n ? SUMMARY.mibValue(n) : SUMMARY.standardLimit);

export function firstUseText(approval: unknown, holdSeconds: number | undefined): string {
  const mode = asFirstUseMode(approval);
  if (mode === "wait_for_review") return SUMMARY.held(holdSeconds || 30);
  return mode === "deny_with_review" ? SUMMARY.refusedThenApproval : SUMMARY.refused;
}

/** True when the reader was shown a blanked value anywhere in the spec. */
export function specIsRedacted(value: unknown): boolean {
  if (value === REDACTED) return true;
  if (Array.isArray(value)) return value.some(specIsRedacted);
  return isBag(value) && Object.values(value).some(specIsRedacted);
}

// What a grant is for: its repositories, else its host, else the secret it reads.
function grantName(scope: Bag): string {
  if (Array.isArray(scope.repos)) return scope.repos.map(String).join(", ");
  return asString(scope.host) ?? asString(scope.secret_name) ?? asString(scope.name) ?? "";
}

function grantValues(grant: Bag): Value[] {
  const scope = asBag(grant.scope) ?? {};
  const approval: ChipSpec[] = grant.requires_approval === true ? [{ label: SUMMARY.needsApproval, tone: "warning" }] : [];
  if (grant.kind !== "git_pat") {
    const name = grantName(scope);
    return [name === REDACTED ? { hidden: true, chips: approval } : mono(name, { chips: approval })];
  }
  // A git access token is narrowed: its host, then what the run's token may do.
  const pat = readPATScope(scope);
  const narrowed: ChipSpec[] = [];
  if (pat.access === "read") narrowed.push({ label: SUMMARY.readOnly, tone: "neutral", title: GIT_PAT_SCOPE.HONESTY_TOKEN });
  if (pat.api) narrowed.push({ label: GIT_PAT_SCOPE.RUN_API, tone: "neutral", title: GIT_PAT_SCOPE.HONESTY_TOKEN });
  const repos =
    pat.repos === undefined
      ? text(GIT_PAT_SCOPE.RUN_REPOS_ALL)
      : pat.repos.length === 0
        ? text(GIT_PAT_SCOPE.REPOS_NONE)
        : mono(pat.repos.join(", "));
  return [mono(pat.host, { chips: [...approval, ...narrowed] }), repos];
}

function summarize(
  spec: RunPolicySpec,
  marks: PolicyChangeMarks | undefined,
  facts: PolicyFacts | undefined,
  pending: readonly PolicyPreviewPending[] | undefined,
): Group[] {
  const raw = spec as unknown as Bag;
  const known = new Set<string>();
  // A key is shown under its display name only when it holds the shape that
  // row reads. An absent key shows nothing; anything else falls to Other settings.
  const take = <T,>(key: string, read: (v: unknown) => T | undefined): T | undefined => {
    const v = raw[key];
    if (v === undefined || v === null) {
      known.add(key);
      return undefined;
    }
    const out = read(v);
    if (out !== undefined) known.add(key);
    return out;
  };
  const groups: Group[] = [];
  const add = (heading: string, rows: Row[]) => rows.length > 0 && groups.push({ heading, rows });
  const row = (label: string, values: Value[]): Row => ({ label, values });
  const entries = (items: string[], field: string): Value[] => [
    ...items.map((h) => mono(h, { mark: marks?.of(field, h) })),
    ...(marks?.removed(field) ?? []).filter((h) => !items.includes(h)).map((h) => mono(h, { mark: "removed" })),
  ];

  const barrier: Row[] = [];
  if (facts?.usedClass) barrier.push(row(SUMMARY.used, [text(ccLabel(facts.usedClass))]));
  if (facts?.requestedClass) barrier.push(row(D.REQUESTED_CLASS, [text(ccLabel(facts.requestedClass))]));
  const floor = take("min_confinement_class", asString);
  if (floor) barrier.push(row(SUMMARY.minimum, [text(ccLabel(floor))]));
  add(SUMMARY.barrier, barrier);

  const network: Row[] = [];
  const allowAll = take("allow_all_egress", asBool);
  const allowed = take("allowed_domains", asStrings);
  if (allowAll) network.push(row(SUMMARY.allowedHosts, [text(D.ALLOW_ALL)]));
  else if (allowed) {
    const hosts = entries(allowed, "allowed_domains");
    network.push(row(SUMMARY.allowedHosts, hosts.length > 0 ? hosts : [text(SUMMARY.none)]));
  }
  const blocked = entries(take("denied_domains", asStrings) ?? [], "denied_domains");
  if (blocked.length > 0) network.push(row(SUMMARY.blockedHosts, blocked));
  // The wire still accepts the legacy boolean here.
  const firstUse = take("first_use_approval", (v) => (typeof v === "string" || typeof v === "boolean" ? v : undefined));
  if (firstUse !== undefined) {
    const hold = take("first_use_hold_seconds", asNumber);
    const waits = asFirstUseMode(firstUse) === "wait_for_review";
    network.push(
      row(SUMMARY.otherHost, [
        text(firstUseText(firstUse, hold)),
        ...(hold && !waits ? [text(SUMMARY.held(hold))] : []),
      ]),
    );
  }
  const holds = take("max_holds", asNumber);
  if (holds) network.push(row(D.HOLDS_AT_ONCE, [text(String(holds))]));
  const methods = take("allowed_methods", asStrings);
  if (methods) network.push(row(SUMMARY.requestTypes, [text(methods.length > 0 ? methods.join(", ") : SUMMARY.allMethods)]));
  const inspection = take("llm_inspection", asBag);
  if (inspection) {
    const on = typeof inspection.mode === "string" && inspection.mode !== "" && inspection.mode !== "off";
    network.push(row(SUMMARY.traffic, [text(on ? SUMMARY.on : SUMMARY.off)]));
  }
  add(SUMMARY.network, network);

  // One row per kind of credential, each grant listed under it.
  const byKind = new Map<string, Row>();
  for (const grant of take("eligible_grants", asBags) ?? []) {
    const kind = String(grant.kind ?? "");
    const label = SUMMARY.grantKinds[kind];
    const held = byKind.get(kind) ?? { label: label ?? kind, raw: !label, values: [] };
    held.values.push(...grantValues(grant));
    byKind.set(kind, held);
  }
  add(SUMMARY.credentials, [...byKind.values()]);

  const ado = take("azure_devops_capabilities", asStrings);
  // The real one-line summary; capabilities it does not know are still listed.
  if (ado && ado.length > 0) {
    groups.push(adoAccessSummary(ado).parts.length > 0 ? { heading: SUMMARY.ado, rows: [], ado } : { heading: SUMMARY.ado, rows: [], items: ado });
  }

  const files: Row[] = [];
  const mounts = take("workspace_mounts", asBags);
  if (mounts && mounts.length > 0) {
    files.push(
      row(
        SUMMARY.folders,
        mounts.map((m) => {
          const target = String(m.target ?? "");
          const source = asString(m.source);
          const hidden = !source || source === REDACTED;
          const readOnly = m.read_only !== false ? ` · ${SUMMARY.readOnly}` : "";
          return mono(`${target}${hidden ? "" : ` ← ${source}`}${readOnly}`, {
            hidden,
            mark: marks?.of("workspace_mounts", target),
          });
        }),
      ),
    );
  }
  const repos = take("workspace_repos", asBags);
  if (repos && repos.length > 0) {
    files.push(
      row(
        SUMMARY.repos,
        repos.map((r) => {
          const repo = String(r.repo ?? "");
          const ref = asString(r.ref);
          const target = asString(r.target);
          return mono(`${repo}${ref ? ` at ${ref}` : ""}${target ? ` → ${target}` : ""}`, {
            mark: marks?.of("workspace_repos", ref ? `${repo}@${ref}` : repo),
          });
        }),
      ),
    );
  }
  add(SUMMARY.files, files);

  const tools: Row[] = [];
  const rules = take("tool_rules", asBags);
  if (rules) {
    // Each named rule, then the "*" default only where the policy spells it out.
    const { named, defaultEffect, explicitDefault } = splitToolRules(rules as unknown as ToolRule[]);
    const values = named.map((r) => text(D.TOOL_RULE(String(r.tool), EFFECT_PAST[r.effect] ?? String(r.effect))));
    if (explicitDefault) values.push(text(toolRulesTail(defaultEffect)));
    if (values.length > 0) tools.push(row(SUMMARY.toolRules, values));
  }
  const push = take("push_rules", asBag);
  const deny = asStrings(push?.deny_paths) ?? [];
  const hold = asStrings(push?.require_review_paths) ?? [];
  if (deny.length > 0) tools.push(row(SUMMARY.pushDeny, deny.map((p) => mono(p, { mark: marks?.of("push_rules", p) }))));
  if (hold.length > 0) tools.push(row(SUMMARY.pushHold, hold.map((p) => mono(p, { mark: marks?.of("push_rules", p) }))));
  const anyBranch = take("git_push_any_branch", asBool);
  if (anyBranch !== undefined) tools.push(row(SUMMARY.pushes, [text(anyBranch ? SUMMARY.anyBranch : SUMMARY.ownBranch)]));
  add(SUMMARY.tools, tools);

  const apps = take("ui_apps", asBags);
  if (apps && apps.length > 0) {
    add(SUMMARY.apps, [
      row(
        POLICY_UI_APPS.label,
        apps.map((a) => {
          const name = String(a.name ?? "");
          return mono(POLICY_UI_APPS.value(name, Number(a.port), asString(a.path) ?? "/"), { mark: marks?.of("ui_apps", name) });
        }),
      ),
    ]);
  }

  const limits: Row[] = [];
  const res = take("resources", asBag) ?? {};
  const cpu = asNumber(res.cpu_millis);
  const memory = asNumber(res.memory_mib);
  const pids = asNumber(res.pids_limit);
  const disk = asNumber(res.disk_mib);
  if (cpu) limits.push(row(SUMMARY.cpu, [text(cpuText(cpu))]));
  if (memory) limits.push(row(SUMMARY.memory, [text(mibText(memory))]));
  if (pids) limits.push(row(SUMMARY.processes, [text(String(pids))]));
  if (disk) limits.push(row(SUMMARY.disk, [text(mibText(disk))]));
  const stop = take("auto_stop_after_sec", asNumber);
  if (stop && stop > 0) limits.push(row(SUMMARY.idle, [text(D.STOPS_AFTER(Math.max(1, Math.round(stop / 60))))]));
  add(SUMMARY.limits, limits);

  add(
    D.OTHER_SETTINGS,
    Object.keys(raw)
      .filter((k) => !known.has(k))
      .map((k) => ({ label: k, raw: true, values: [mono(JSON.stringify(raw[k]))] })),
  );

  // Launch-only checks are one titled list, never shown as an enforced result.
  if (pending && pending.length > 0) {
    groups.push({ heading: D.CHECKED_AT_LAUNCH, intro: D.PENDING, rows: [], items: pending.map((k) => PENDING_NAME[k] ?? k) });
  }
  return groups;
}

function MarkChip({ mark }: { mark: PolicyMark }) {
  return mark === "added" ? (
    <Chip tone="info">{POLICY_TAB.chipAdded}</Chip>
  ) : (
    <Chip tone="neutral">{POLICY_TAB.chipRemoved}</Chip>
  );
}

// A value the reader may not see. Focusable, so the reason is reachable without a pointer.
function HiddenChip() {
  return (
    <span title={POLICY_TAB.hiddenTip} tabIndex={0} role="note" aria-label={`${POLICY_TAB.hidden}. ${POLICY_TAB.hiddenTip}`}>
      <Chip tone="neutral">{POLICY_TAB.hidden}</Chip>
    </span>
  );
}

export function PolicySummary({
  spec,
  marks,
  facts,
  pending,
  headingLevel = 3,
}: {
  spec: RunPolicySpec;
  marks?: PolicyChangeMarks;
  facts?: PolicyFacts;
  pending?: readonly PolicyPreviewPending[];
  /** The level of the section headings, set by where the document sits on its page. */
  headingLevel?: 3 | 4;
}) {
  const groups = summarize(spec, marks, facts, pending);
  const Heading = headingLevel === 4 ? "h4" : "h3";
  if (groups.length === 0) return <p className="text-sm text-muted-foreground">{SUMMARY.none}</p>;
  return (
    <div
      data-testid="policy-summary"
      className="grid items-start gap-x-6 gap-y-4 [grid-template-columns:repeat(auto-fit,minmax(min(300px,100%),1fr))]"
    >
      {groups.map((g) => (
        <section key={g.heading} className="flex min-w-0 flex-col gap-2 border-t border-border pt-3">
          <Heading className="text-sm font-semibold text-foreground">{g.heading}</Heading>
          {g.intro && <p className="text-sm text-muted-foreground">{g.intro}</p>}
          {g.items && (
            <ul className="grid gap-x-4 gap-y-1 [grid-template-columns:repeat(auto-fit,minmax(min(180px,100%),1fr))]">
              {g.items.map((item) => (
                <li key={item} className="text-sm text-foreground">
                  {item}
                </li>
              ))}
            </ul>
          )}
          {g.ado && <ADOAccessSummary caps={g.ado} />}
          {g.rows.length > 0 && (
            <dl className="grid grid-cols-[minmax(0,9.5rem)_minmax(0,1fr)] gap-x-4 gap-y-2">
              {g.rows.map((r) => (
                <React.Fragment key={r.label}>
                  <dt className={r.raw ? "font-mono text-sm text-muted-foreground" : "text-sm text-muted-foreground"}>{r.label}</dt>
                  <dd className="min-w-0">
                    <ul className="flex flex-col gap-0.5">
                      {r.values.map((v, i) => (
                        <li key={i} className="flex flex-wrap items-baseline gap-2 text-sm text-foreground [overflow-wrap:anywhere]">
                          {v.text && <span className={v.mono ? "font-mono" : undefined}>{v.text}</span>}
                          {v.hidden && <HiddenChip />}
                          {v.chips?.map((c) => (
                            <span key={c.label} title={c.title}>
                              <Chip tone={c.tone}>{c.label}</Chip>
                            </span>
                          ))}
                          {v.mark && <MarkChip mark={v.mark} />}
                        </li>
                      ))}
                    </ul>
                  </dd>
                </React.Fragment>
              ))}
            </dl>
          )}
        </section>
      ))}
    </div>
  );
}
