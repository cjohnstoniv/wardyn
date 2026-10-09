/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The Access panel's rows (#1914), as data: one row per component the run
// carries, from the facts the two dry-run reads return. Pure — the rows, their
// tone, the sentences under them and the issues that hold Launch all come from
// here, so the panel, the panel nav and the line above Launch cannot disagree.
//
// D14: a CUSTOM component that needs input, or was refused, holds Launch. A Git
// provider or a model provider that needs input is a warning only; it is
// resolved at its own launch door and never blocks.
import type { ComponentFact, ComponentSecretFact } from "../../../lib/types/components";
import type { SetupItem } from "../../../lib/types";
import { ACCESS_ROWS as T } from "../../wardyn/copy/components";
import type { LaunchIssue } from "./new-run-launch-gates";

/** The wire's four, plus the one D14 names that no server version sends yet. */
export type AccessRowStatus = ComponentFact["status"] | "refused";
export type AccessRowTone = "success" | "warning" | "danger" | "neutral";

export interface AccessRow {
  /** The fact's id, stable across reads. */
  id: string;
  /** The row's DOM id: what an issue link focuses. */
  domId: string;
  kind: ComponentFact["kind"];
  provider?: ComponentFact["provider"];
  title: string;
  /** Why the row is there. */
  reason: string;
  status: AccessRowStatus;
  statusLabel: string;
  tone: AccessRowTone;
  /** Holds Launch (D14). */
  blocking: boolean;
  /** The sentence that holds Launch, printed in the row and named above Launch. */
  issueText: string | null;
  /** What the status itself says when no requirement does. */
  statusNote: string | null;
  /** What the run's shape makes true of it, in the order the design lists them. */
  disclosures: string[];
  requirements: SetupItem[];
  hosts: string[];
  secrets: ComponentSecretFact[];
  configKeys: string[];
  org?: string;
  repos: string[];
  laneNote: string | null;
}

const ROW_DOM_PREFIX = "nr-access-row-";
export const rowDomId = (id: string): string => ROW_DOM_PREFIX + id;
/** The fact id behind a row's DOM id, undefined for any other id. */
export const rowIdFromDomId = (domId: string): string | undefined =>
  domId.startsWith(ROW_DOM_PREFIX) ? domId.slice(ROW_DOM_PREFIX.length) : undefined;

/**
 * One fact per id: the preview's, in its order, each replaced by the preflight's
 * answer for the same id (preflight grades more: a Git provider's connection),
 * then any the preview did not carry.
 */
export function overlayFacts(
  preview: readonly ComponentFact[] | undefined,
  preflight: readonly ComponentFact[] | undefined,
): ComponentFact[] {
  const graded = new Map((preflight ?? []).map((f) => [f.id, f]));
  const out = (preview ?? []).map((f) => graded.get(f.id) ?? f);
  const seen = new Set(out.map((f) => f.id));
  return [...out, ...(preflight ?? []).filter((f) => !seen.has(f.id))];
}

function titleOf(f: ComponentFact): string {
  if (f.kind === "git_provider") return f.provider === "azure_devops" ? T.TITLE.azure_devops : T.TITLE.github;
  return f.name?.trim() || T.TITLE.custom_unnamed;
}

function disclosuresOf(f: ComponentFact): string[] {
  const out: string[] = [];
  if (f.self_defined) out.push(T.DISCLOSURE.SELF_DEFINED);
  if (f.tls_intercept) out.push(T.DISCLOSURE.HEADER);
  if (f.secrets?.some((s) => s.delivery === "env" || s.delivery === "file")) out.push(T.DISCLOSURE.RESIDENT);
  // The cap sentences are the organisation's rule: only when the server set one.
  if (f.autonomy_cap === "L1") out.push(T.DISCLOSURE.CAP_L1);
  if (f.autonomy_cap === "L0") out.push(T.DISCLOSURE.CAP_L0);
  if (f.high_risk) out.push(T.DISCLOSURE.HIGH_RISK);
  if (f.vault_floor) out.push(T.DISCLOSURE.VAULT_FLOOR);
  return out;
}

function toneOf(status: AccessRowStatus, blocking: boolean): AccessRowTone {
  if (blocking) return "danger";
  if (status === "ready") return "success";
  if (status === "needs_input" || status === "unavailable" || status === "refused") return "warning";
  return "neutral";
}

export function accessRow(f: ComponentFact): AccessRow {
  // Widened on purpose: `refused` is in the design but not in the wire's list.
  const status = f.status as AccessRowStatus;
  const custom = f.kind === "custom";
  const blocking = custom && (status === "needs_input" || status === "refused");
  const title = titleOf(f);
  return {
    id: f.id,
    domId: rowDomId(f.id),
    kind: f.kind,
    provider: f.provider,
    title,
    reason: T.REASON[f.reason] ?? "",
    status,
    statusLabel: T.STATUS[status] ?? T.STATUS.unknown,
    tone: toneOf(status, blocking),
    blocking,
    issueText: blocking ? (status === "refused" ? T.ISSUE_REFUSED(title) : T.ISSUE_NEEDS_INPUT(title)) : null,
    statusNote: status === "unavailable" ? T.UNAVAILABLE[f.kind] : null,
    disclosures: disclosuresOf(f),
    requirements: f.requirements ?? [],
    hosts: f.hosts ?? [],
    secrets: f.secrets ?? [],
    configKeys: f.config_keys ?? [],
    org: f.org,
    repos: f.repos ?? [],
    laneNote: f.lane ? T.LANE[f.lane] ?? null : null,
  };
}

export function accessRows(
  preview: readonly ComponentFact[] | undefined,
  preflight: readonly ComponentFact[] | undefined,
): AccessRow[] {
  return overlayFacts(preview, preflight).map(accessRow);
}

/** The rows that hold Launch, as the issues the panel nav counts and the line above Launch links to. */
export function accessIssues(rows: readonly AccessRow[]): LaunchIssue[] {
  return rows.flatMap((r) =>
    r.issueText ? [{ panel: "access" as const, focus: r.domId, text: r.issueText, inline: true }] : [],
  );
}
