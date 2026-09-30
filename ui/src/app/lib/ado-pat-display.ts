/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// What the per-person Azure DevOps token surfaces show, derived from the wire
// (types/ado-pat.ts) and worded by ado-pat-copy.ts. Pure: no React, no fetch.
// Every function says nothing rather than inventing a fact the wire did not
// carry, the rule scm-access-display.ts follows for the sign-in lane.
import { ADO_PAT } from "./ado-pat-copy";
import { ADO_PAT_REASON } from "./api/ado-pat";
import { adoCapName } from "./ado-access-copy";
import type { ADOEntraConfig, GitProvider } from "./api/providers";
import type { ADORunToken, SCMAccessPAT } from "./types/ado-pat";

// en-GB on purpose: the copy is written in British English, and the clock is
// 24-hour, as the mock draws it.
export function formatClock(iso: string): string {
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return iso;
  return d.toLocaleTimeString("en-GB", { hour: "2-digit", minute: "2-digit", hourCycle: "h23" });
}

export function formatDay(iso: string): string {
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return iso;
  return d.toLocaleDateString("en-GB", { day: "numeric", month: "long" });
}

const DAY_MS = 24 * 60 * 60 * 1000;

/** Whole days until iso, rounded up (a token with 20 hours left has 1 day). */
export function daysLeft(iso: string, now: number): number {
  const t = new Date(iso).getTime();
  return Number.isNaN(t) ? 0 : Math.max(0, Math.ceil((t - now) / DAY_MS));
}

/** The organisation as people name it: the first path segment of a
 *  dev.azure.com address, else the address without its scheme. */
export function adoOrgLabel(address: string): string {
  try {
    const u = new URL(address);
    if (u.hostname === "dev.azure.com") return u.pathname.split("/").filter(Boolean)[0] ?? u.host;
    return `${u.host}${u.pathname.replace(/\/+$/, "")}`;
  } catch {
    return address;
  }
}

/** Azure DevOps' own page for a person's personal access tokens. */
export function adoTokensURL(address: string): string {
  try {
    const u = new URL(address);
    const base = u.hostname === "dev.azure.com" ? `${u.origin}/${u.pathname.split("/").filter(Boolean)[0] ?? ""}` : `${u.origin}${u.pathname.replace(/\/+$/, "")}`;
    return `${base}/_usersSettings/tokens`;
  } catch {
    return "";
  }
}

// ---- The person's card ----

export type PatTone = "success" | "warning" | "danger" | "neutral";

export interface PatCardView {
  title: string;
  chip: { label: string; tone: PatTone };
  /** Body paragraphs, in order. */
  body: string[];
  /** The one action the state offers. */
  action: "connect" | "add_token" | "replace_token" | null;
  /** Whether a connected minted card offers Disconnect. */
  disconnect: boolean;
}

/** Whether this access answer belongs to a per-person token row, and which. */
export function adoPatMode(access?: SCMAccessPAT): "minted_pat" | "own_pat" | null {
  return access?.token_mode === "minted_pat" || access?.token_mode === "own_pat" ? access.token_mode : null;
}

// The person's Azure DevOps card for a minted_pat or own_pat row; null when the
// row is neither, or the state is one this console does not recognise.
export function patCardView(access: SCMAccessPAT, title: string, now: number = Date.now()): PatCardView | null {
  const mode = adoPatMode(access);
  if (mode === "minted_pat") return mintedCard(access, title);
  if (mode === "own_pat") return ownCard(access, title, now);
  return null;
}

function mintedCard(access: SCMAccessPAT, title: string): PatCardView | null {
  switch (access.state) {
    case "not_configured":
      return { title, chip: { label: ADO_PAT.CHIP_NOT_CONNECTED, tone: "neutral" }, body: [ADO_PAT.MEMBER_NOT_CONNECTED], action: "connect", disconnect: false };
    case "live": {
      const body: string[] = [ADO_PAT.MEMBER_CONNECTED];
      const last = access.last_token;
      // Only a finished token has both times the line names.
      if (last?.revoked_at) body.push(ADO_PAT.MEMBER_LAST_TOKEN(formatClock(last.created_at), formatClock(last.revoked_at)));
      return { title, chip: { label: ADO_PAT.CHIP_CONNECTED, tone: "success" }, body, action: null, disconnect: true };
    }
    case "expired_signin":
      return { title, chip: { label: ADO_PAT.CHIP_SIGN_IN_AGAIN, tone: "warning" }, body: [ADO_PAT.SIGN_IN_AGAIN_BODY], action: "connect", disconnect: false };
    case "blocked":
      return { title, chip: { label: ADO_PAT.CHIP_BLOCKED, tone: "danger" }, body: [ADO_PAT.BLOCKED_BODY], action: null, disconnect: false };
    default:
      return null;
  }
}

function ownCard(access: SCMAccessPAT, title: string, now: number): PatCardView | null {
  // An Azure DevOps Server row names itself, and says why there is no sign-in.
  const cardTitle = access.server ? ADO_PAT.OWN_SERVER_TITLE : title;
  const org = adoOrgLabel(access.org ?? "");
  const expiryLine = access.expires_at ? ADO_PAT.OWN_EXPIRING_LINE(org, formatDay(access.expires_at)) : null;
  const serverNote = access.server ? [ADO_PAT.OWN_SERVER_NOTE] : [];
  switch (access.state) {
    case "not_configured":
      return { title: cardTitle, chip: { label: ADO_PAT.CHIP_NOT_CONNECTED, tone: "neutral" }, body: serverNote, action: "add_token", disconnect: false };
    case "live":
      return { title: cardTitle, chip: { label: ADO_PAT.CHIP_CONNECTED, tone: "success" }, body: [...serverNote, ...(expiryLine ? [expiryLine] : [])], action: "replace_token", disconnect: false };
    case "expiring":
      return {
        title: cardTitle,
        chip: { label: ADO_PAT.OWN_CHIP_EXPIRING(access.expires_at ? daysLeft(access.expires_at, now) : 0), tone: "warning" },
        body: [...serverNote, ...(expiryLine ? [expiryLine] : [])],
        action: "replace_token",
        disconnect: false,
      };
    case "expired":
      return { title: cardTitle, chip: { label: ADO_PAT.OWN_CHIP_EXPIRED, tone: "danger" }, body: [...serverNote, ADO_PAT.OWN_EXPIRED_BODY], action: "add_token", disconnect: false };
    default:
      return null;
  }
}

// ---- The own-token dialog ----

// The labels Azure DevOps' own "Create a new personal access token" page shows
// for the scopes a capability can need. The person ticks these boxes there, so
// the dialog names them in that wording, not Wardyn's. A write scope covers the
// read beside it, which is why the list drops the covered read.
const SCOPE_LABEL: Record<string, string> = {
  "vso.code": "Code (Read)",
  "vso.code_write": "Code (Read & write)",
  "vso.code_manage": "Code (Read, write, & manage)",
  "vso.project": "Project and Team (Read)",
  "vso.project_manage": "Project and Team (Read, write, & manage)",
  "vso.work": "Work Items (Read)",
  "vso.work_write": "Work Items (Read & write)",
  "vso.wiki": "Wiki (Read)",
  "vso.wiki_write": "Wiki (Read & write)",
  "vso.build": "Build (Read)",
  "vso.build_execute": "Build (Read & execute)",
  "vso.release": "Release (Read)",
  "vso.release_execute": "Release (Read, write, & execute)",
  "vso.release_manage": "Release (Read, write, execute, & manage)",
  "vso.packaging": "Packaging (Read)",
  "vso.packaging_write": "Packaging (Read & write)",
  "vso.serviceendpoint": "Service Connections (Read)",
  "vso.serviceendpoint_manage": "Service Connections (Read, query, & manage)",
  "vso.graph": "Graph (Read)",
  "vso.identity": "Identity (Read)",
  "vso.test": "Test Management (Read)",
  "vso.analytics": "Analytics (Read)",
};

// scope -> the scopes whose label already includes it.
const COVERED_BY: Record<string, string[]> = {
  "vso.code": ["vso.code_write", "vso.code_manage"],
  "vso.code_write": ["vso.code_manage"],
  "vso.project": ["vso.project_manage"],
  "vso.work": ["vso.work_write"],
  "vso.wiki": ["vso.wiki_write"],
  "vso.build": ["vso.build_execute"],
  "vso.release": ["vso.release_execute", "vso.release_manage"],
  "vso.release_execute": ["vso.release_manage"],
  "vso.packaging": ["vso.packaging_write"],
  "vso.serviceendpoint": ["vso.serviceendpoint_manage"],
};

/** The scope labels the own-token dialog lists: sorted by label, a covered read
 *  dropped, a scope with no label shown as itself. */
export function ownTokenScopeLabels(scopes: string[] | undefined): string[] {
  const have = new Set(scopes ?? []);
  return [...have]
    .filter((s) => !(COVERED_BY[s] ?? []).some((w) => have.has(w)))
    .map((s) => SCOPE_LABEL[s] ?? s)
    .sort((a, b) => a.localeCompare(b));
}

// ---- New Run ----

export type PatRefusalNote = "blocked" | "connect";

/** Which launch note a refused token creation calls for, by the wire reason:
 *
 *  - ado_pat_policy_blocked: the organisation restricts who may create tokens,
 *    so the person is told to ask for the allow list ("blocked");
 *  - ado_pat_consent_needed: the person's sign-in cannot create tokens (lost,
 *    or never consented), so they are asked to connect again ("connect");
 *  - ado_pat_lifespan_policy: the admin's longest life is too long. That is
 *    the admin's row to fix (the organisation check reports it there), and
 *    nothing the person can do, so the launch strip's own sentence is all;
 *  - ado_pat_mint_refused: unclassified; the strip's sentence is all. */
export function patRefusalNote(reason: string): PatRefusalNote | null {
  if (reason === ADO_PAT_REASON.POLICY_BLOCKED) return "blocked";
  if (reason === ADO_PAT_REASON.CONSENT_NEEDED) return "connect";
  return null;
}

/** The capability names New Run's token line lists: the policy's, else the
 *  row's default profile. Empty when neither is known. */
export function newRunTokenCaps(policyCaps: unknown, defaults: string[] | undefined): string[] {
  const list = Array.isArray(policyCaps) && policyCaps.length > 0 ? (policyCaps as string[]) : (defaults ?? []);
  return list.map(adoCapName);
}

// ---- The run page ----

export interface RunTokenLine {
  text: string;
  /** Revoked and superseded: drawn muted. */
  old: boolean;
}

export interface RunTokenView {
  lines: RunTokenLine[];
  /** One "Access added" line per widening, in order. */
  added: string[];
  paused: boolean;
  /** The clock time a failed renewal's token stops working, when one failed. */
  renewalFailedAt: string | null;
  /** The clock time each unrevokable token expires. */
  revokeFailedAt: string[];
}

/** A run's tokens as the page draws them, oldest first. `paused` is the run's own
 *  state; the token list alone cannot say a run is paused. */
export function runTokenView(tokens: ADORunToken[], paused: boolean): RunTokenView {
  const sorted = [...tokens].sort((a, b) => Date.parse(a.created_at) - Date.parse(b.created_at));
  const lines = sorted.map((t, i) => {
    const created = formatClock(t.created_at);
    const expires = formatClock(t.valid_to);
    if (!t.revoked_at) return { text: ADO_PAT.RUN_TOKEN_LINE(created, expires), old: false };
    const reason = (t.revoke_reason && ADO_PAT.REVOKE_REASON[t.revoke_reason]) || "";
    // A revoked token that a later one replaced is history; the last line of a
    // finished run stays plain, as the mock draws it.
    const text = reason
      ? ADO_PAT.RUN_TOKEN_LINE_REVOKED(created, expires, formatClock(t.revoked_at), reason)
      : `${ADO_PAT.RUN_TOKEN_LINE(created, expires)} · revoked ${formatClock(t.revoked_at)}`;
    return { text, old: i < sorted.length - 1 || paused };
  });
  return {
    lines,
    added: sorted
      .filter((t) => (t.added_capabilities?.length ?? 0) > 0)
      .map((t) => ADO_PAT.RUN_ACCESS_ADDED(formatClock(t.created_at), (t.added_capabilities ?? []).map(adoCapName).join(", "))),
    paused,
    renewalFailedAt: formatClockOrNull(sorted.find((t) => t.renewal_failed && !t.revoked_at)?.valid_to),
    revokeFailedAt: sorted.filter((t) => t.revoke_failed).map((t) => formatClock(t.valid_to)),
  };
}

function formatClockOrNull(iso: string | undefined): string | null {
  return iso ? formatClock(iso) : null;
}

// ---- The admin's row ----

/** Whether a row is the one the upgrade converted and switched off, waiting for
 *  the admin to choose how people connect: an Azure DevOps row, off, per person,
 *  that holds an own-token block or is a git-only Server row. */
export function adoRowNeedsChoice(row: GitProvider): boolean {
  if (row.kind !== "azure_devops" || !row.disabled || row.credential_source !== "per_user") return false;
  if (row.entra) return row.entra.token_mode === "own_pat";
  return (row.lanes ?? []).length === 1 && row.lanes?.[0] === "pat";
}

/** The mode a row is set to, with the unset default read as bearer. */
export function adoTokenMode(row: GitProvider): "bearer" | "minted_pat" | "own_pat" {
  return row.entra?.token_mode ?? "bearer";
}

const PAT_HOURS_MAX = 168;
const PAT_DAYS_MAX = 90;

/** Whether text is a whole number of hours (days) the server accepts. */
export function patRangeOk(text: string, max: number): boolean {
  const n = Number(text);
  return text.trim() !== "" && Number.isInteger(n) && n >= 1 && n <= max;
}

export const patHoursOk = (text: string) => patRangeOk(text, PAT_HOURS_MAX);
export const patDaysOk = (text: string) => patRangeOk(text, PAT_DAYS_MAX);

/** The lifetime the row's own mode reads that the server would refuse: the one
 *  thing this section can write that a Save is certain to bounce. An unset
 *  value reads as its default and is fine. */
export function patLifetimeInvalid(cfg: ADOEntraConfig | undefined): boolean {
  if (!cfg) return false;
  if (cfg.token_mode === "minted_pat" && cfg.pat_max_hours !== undefined) return !patHoursOk(String(cfg.pat_max_hours));
  if (cfg.token_mode === "own_pat" && cfg.pat_max_days !== undefined) return !patDaysOk(String(cfg.pat_max_days));
  return false;
}
