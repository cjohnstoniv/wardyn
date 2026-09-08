/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Shared transport for the Wardyn API client — same-origin fetch() against
// /api/v1. Every per-domain module (runs.ts, approvals.ts, …) imports wfetch +
// the JSON helpers from here, so there is exactly ONE fetch wrapper, one auth
// token store, and one 401 handler across the whole client. Split out of the
// former monolithic lib/api.ts so unused domains tree-shake per route chunk.
import { lsGet, lsSet, ssGet, ssSet } from "../storage";
import { CC_ORDER, type ConfinementClass } from "../types";

const BASE = "/api/v1";
const TOKEN_KEY = "wardyn_admin_token";

// ------------------------------------------------------------
// Auth token + 401 handling
// ------------------------------------------------------------
let _unauthorized: (() => void) | null = null;

// The admin bearer defaults to sessionStorage (cleared when the tab/browser
// closes) so a full-admin token is not left at rest across restarts. It lands in
// localStorage ONLY when the operator opts into "remember on this device". A
// token stored under an older build (localStorage) keeps working: getToken falls
// back to localStorage, so this change is transparent to existing sessions.
export function getToken(): string | null {
  return ssGet(TOKEN_KEY) ?? lsGet(TOKEN_KEY);
}

// setToken(token, remember): remember=true persists to localStorage (survives
// restart); default (false) uses sessionStorage. Either way the OTHER store is
// cleared so the token lives in exactly one place. token=null clears both.
export function setToken(token: string | null, remember = false): void {
  if (!token) {
    ssSet(TOKEN_KEY, null);
    lsSet(TOKEN_KEY, null);
    return;
  }
  if (remember) {
    lsSet(TOKEN_KEY, token);
    ssSet(TOKEN_KEY, null);
  } else {
    ssSet(TOKEN_KEY, token);
    lsSet(TOKEN_KEY, null);
  }
}

export function onUnauthorized(fn: () => void): void {
  _unauthorized = fn;
}

export class HttpError extends Error {
  status: number;
  constructor(status: number, message: string) {
    super(message);
    this.status = status;
    this.name = "HttpError";
  }
}

// Rank on the canonical weakest→strongest ladder, for clamping a run's requested
// class up to its policy floor before create (a weaker request 422s server-side).
// Derived from CC_ORDER, never a local copy: an unknown class ranks 0 (below CC1),
// so the clamp stays fail-closed.
export const ccRank = (cc: string): number => CC_ORDER.indexOf(cc as ConfinementClass) + 1;

// Every console request is bounded. A daemon that ACCEPTS a connection and
// never writes a response is not hypothetical here — internal/db/db.go:110-121
// describes reaching it with no Wardyn bug at all (one idle psql transaction
// exhausts the pool, "every other query in the process starts blocking"
// against an http.Server that "deliberately sets no WriteTimeout") — and an
// un-bounded fetch() waits for it forever. Forever is not a slow screen: the
// shell gates EVERY route behind health()+whoami() settling (app-shell's
// useMeta -> App.tsx's roleResolved), so one stalled read is a permanent
// spinner with no error, no retry and nothing a reload changes. The deadline
// turns that into the rejection every caller already handles: the shell's
// "Control plane unreachable" banner, sign-in's "Could not reach the control
// plane.", a launch button that un-disables.
//
// 60s, not a snappy 5-10s: this bounds a HANG, it is not a latency budget, and
// the slowest legitimate console call (an import step, a policy verify) must
// still fit under it. A call that genuinely needs longer passes its own
// timeoutMs.
export const WFETCH_TIMEOUT_MS = 60_000;

// TIMEOUT_STATUS: no HTTP response ever happened, so there is no status to
// report. Callers that branch on `e.status === 401` are unaffected, and the
// message is the sentence sign-in already shows for an unreachable daemon —
// this path adds no new console copy, it just stops the wait.
export const TIMEOUT_STATUS = 0;
const TIMEOUT_MESSAGE = "Could not reach the control plane.";

// deadlineSignal composes the caller's signal (if any) with our timeout, so a
// caller-driven abort still works and the deadline still applies.
function deadlineSignal(caller: AbortSignal | null | undefined, timeoutMs: number): AbortSignal {
  const deadline = AbortSignal.timeout(timeoutMs);
  return caller ? AbortSignal.any([caller, deadline]) : deadline;
}

// Central fetch wrapper:
//  (a) attaches Bearer token when a wardyn_admin_token is set,
//  (b) always sends the OIDC session cookie (credentials: 'include'),
//  (c) routes HTTP 401 to the module-level onUnauthorized handler,
//  (d) bounds the request — see WFETCH_TIMEOUT_MS.
export async function wfetch(
  path: string,
  init: RequestInit = {},
  timeoutMs: number = WFETCH_TIMEOUT_MS,
): Promise<Response> {
  const headers = new Headers(init.headers);
  const token = getToken();
  if (token) headers.set("Authorization", `Bearer ${token}`);
  if (init.body != null && !headers.has("Content-Type")) {
    headers.set("Content-Type", "application/json");
  }
  headers.set("Accept", headers.get("Accept") ?? "application/json");

  let res: Response;
  try {
    res = await fetch(`${BASE}${path}`, {
      ...init,
      headers,
      credentials: "include",
      signal: deadlineSignal(init.signal, timeoutMs),
    });
  } catch (e) {
    // A caller's OWN abort stays an abort (it is their control flow, not a
    // failure); only the deadline is reported as the unreachable-daemon case.
    if (init.signal?.aborted) throw e;
    // Read .name off the value, not `e instanceof Error`: an aborted fetch
    // rejects with a DOMException, which is not an Error in every runtime the
    // console and its jsdom test env run in.
    const name = (e as { name?: unknown } | null)?.name;
    if (name === "TimeoutError" || name === "AbortError") {
      throw new HttpError(TIMEOUT_STATUS, TIMEOUT_MESSAGE);
    }
    throw e;
  }

  if (res.status === 401) {
    _unauthorized?.();
    throw new HttpError(401, "Unauthorized");
  }
  return res;
}

export async function asJson<T>(res: Response): Promise<T> {
  if (!res.ok) throw new HttpError(res.status, await errText(res));
  return (await res.json()) as T;
}

// The ONE parser for the control plane's `{"error":"<human message>"}` envelope:
// surface that message verbatim (readable in a toast / inline error), fall back to
// the raw body, then the status text. It returns rather than throws, so it serves
// both the paths where a non-2xx is an EXPECTED, actionable outcome the caller
// renders inline (e.g. verifyWorkspace's 422/503/409) and asJson, which wraps it
// in an HttpError. Change the envelope here and both follow.
export async function errText(res: Response): Promise<string> {
  try {
    const body = await res.text();
    if (!body) return res.statusText;
    try {
      const j = JSON.parse(body) as { error?: unknown };
      return typeof j.error === "string" && j.error ? j.error : body;
    } catch {
      return body;
    }
  } catch {
    return res.statusText;
  }
}

// Explicit page size for the console's list polls. wardynd paginates every list
// endpoint (default 200, hard max 1000); the console asks for the max so its
// Runs / Audit / Workspaces views and the attention-badge poll keep showing the
// full recent set instead of silently inheriting — and being reshaped by — a
// change to the server default. If a deployment ever outgrows 1000 rows in one
// view, move that view onto real ?offset= paging (the server + SDK already
// support it; the response sets X-Wardyn-Truncated when more rows exist).
export const LIST_LIMIT = 1000;

// withLimit appends ?limit= (merging with an existing query string) so a poll
// sends an explicit page size rather than relying on the server default.
export function withLimit(path: string, limit: number = LIST_LIMIT): string {
  return `${path}${path.includes("?") ? "&" : "?"}limit=${limit}`;
}

// A nil Go slice encodes as JSON null, not [] — coerce so list callers always get
// an array to map over. ponytail: one backend, no envelope; if a wrapped shape ever
// ships, read its named key at that call site (see secrets.ts's `{names: […]}`).
export function unwrapList<T>(payload: unknown): T[] {
  return Array.isArray(payload) ? (payload as T[]) : [];
}

// The three answers a mount-time auth probe can honestly give. R4/F027: this
// used to be a plain boolean, so a daemon 5xx and a dead network both returned
// exactly `false` — indistinguishable from a real 401 — and App.tsx turned that
// single bit into "unauthed" and rendered the sign-in gate with no hint that
// anything was down. sign-in.tsx:116-123 had ALREADY made this split by hand
// for submitToken, for the same reason and in the same words ("probeAuth
// collapsed every failure ... to the same boolean `false`"); the sibling call
// site was left on the old shape. Splitting it HERE is the one place both can
// share, rather than a third hand-rolled copy.
//
//   "authed"      the protected row came back — this caller is signed in.
//   "unauthed"    a REAL 401: no session cookie, or a rejected admin token.
//   "unreachable" the daemon did not answer the question (any other non-2xx,
//                 or a transport failure). NOT a statement about the caller's
//                 credentials, and must never be reported as one.
export type AuthProbe = "authed" | "unauthed" | "unreachable";

// Probe auth by hitting a protected endpoint. It needs only a yes/no on the
// response status, so it asks for a single row (?limit=1) rather than pulling
// the whole runs list just to discard it.
export async function probeAuth(): Promise<AuthProbe> {
  try {
    const res = await wfetch(withLimit("/runs", 1), { method: "GET" });
    // wfetch has already thrown HttpError(401) for a real rejection, so a
    // non-ok response here is a 403/5xx: the request carried whatever
    // credentials the caller has and the daemon still could not answer.
    return res.ok ? "authed" : "unreachable";
  } catch (e) {
    if (e instanceof HttpError && e.status === 401) return "unauthed";
    return "unreachable";
  }
}

// Small typed coercion helpers shared by the audit/grant/recording projections.
export function str(v: unknown): string | undefined {
  return typeof v === "string" ? v : undefined;
}
export function num(v: unknown): number | undefined {
  return typeof v === "number" ? v : undefined;
}
