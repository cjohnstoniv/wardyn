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

// Auth token + 401 handling
// #483: a 401 on a console that WAS signed in keeps the page mounted and opens
// a sign-in dialog over it (App.tsx), so the handler needs no path to return
// to — only whether the refused request was a WRITE, and whose Save it was: a
// save that hit the expiry is never re-sent, and the screen that made it has
// to say so.
export interface Refused {
  write: boolean;
  /** The owning screen's id, when the request was that screen's Save (WfetchInit.save). */
  save?: string;
}
let _unauthorized: ((refused: Refused) => void) | null = null;

// #483: while the console is signed out mid-page — the dialog or the
// read-only bar — no write leaves this tab. Whoever holds a session by then
// (another tab can have signed in as someone else) must never receive one
// person's draft; the write is refused here, unsent, and reported as a 401 so
// the dialog asks again. Reads still go out: only the dialog's own principal
// check can end the hold (App.tsx sets it from the reauth phase).
let _signedOutHold = false;
export function setSignedOutHold(on: boolean): void {
  _signedOutHold = on;
}

/** RequestInit plus `save`: the owning screen's id when this request is that
 *  screen's Save, so a refused one is named beside that Save and nowhere else. */
export type WfetchInit = RequestInit & { save?: string };

// The full sign-in screen's notice for a session that ended (an amber
// warning, not the error box). wfetch cannot tell an expired SSO session from
// a revoked admin token — both arrive as a bare 401 — so this names neither.
export const SESSION_ENDED_REASON = "You were signed out. Sign in again to continue.";

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

export function onUnauthorized(fn: (refused: Refused) => void): void {
  _unauthorized = fn;
}

export class HttpError extends Error {
  status: number;
  /** The envelope's machine-readable class, "" when the body carries none.
   *  `model_credential` and `git_credential` (#386) are the create-time
   *  refusals the New Run rail answers with a sign-in/connect dialog
   *  (runs.ts's isCredentialRefusal / isGitCredentialRefusal). */
  reason: string;
  /** The git_credential 422's Azure DevOps org address (#386's launch door,
   *  review finding F1) — "" when the body carries none. The dialog names it
   *  from HERE, not from a preflight fact: a 422 can be the very first thing
   *  a caller hears about the row. */
  org: string;
  constructor(status: number, message: string, reason = "", org = "") {
    super(message);
    this.status = status;
    this.reason = reason;
    this.org = org;
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

// LAUNCH_DEADLINE_MS — the deadline for a console call that brings a SANDBOX up.
//
// The default above bounds a hang; it is not a latency budget, and for these
// calls it was being spent as one. POST /runs is synchronous through
// CreateSandbox (runs.go), which on k8s waits canaryWaitTimeout (3 min,
// canary.go) ON TOP of a cold image pull — a server-side worst case that
// legitimately exceeds 60s, at which point the console reports the daemon
// unreachable over a launch that is working fine and drops the run id it was
// about to be handed. Five minutes covers the substrate's own ceiling with room
// to spare; a longer deadline cannot break a call that already works today.
//
// The sign-in launch is NOT on this list: POST /setup/harness-login answers
// before dispatch now (internal/api/harnesscred_launch.go), so it is a fast
// call again and the default bound is the right one for it.
export const LAUNCH_DEADLINE_MS = 300_000;

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
// drainBody — read and discard a response body no caller will ever read.
//
// A fetch Response whose body is never consumed leaves its stream open, so the
// request never completes: it holds its connection until the deadline aborts
// it, and the document never reaches network-idle. Nothing else drains it —
// securityHeaders answers every API route Cache-Control: no-store, so no cache
// layer is reading the body on the console's behalf either.
//
// Swallows its own failure: a truncated or already-consumed body must never
// change the verdict the caller already read off the status.
async function drainBody(res: Response): Promise<void> {
  try {
    await res.text();
  } catch {
    /* nothing to discard */
  }
}

export async function wfetch(
  path: string,
  { save, ...init }: WfetchInit = {},
  timeoutMs: number = WFETCH_TIMEOUT_MS,
): Promise<Response> {
  const method = (init.method ?? "GET").toUpperCase();
  const refused: Refused = { write: method !== "GET" && method !== "HEAD", save };
  if (refused.write && _signedOutHold) {
    _unauthorized?.(refused);
    throw new HttpError(401, "Unauthorized");
  }
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
    // A REAL 401 means the bearer THIS REQUEST sent was rejected
    // (expired/revoked/foreign admin token) — leaving it stored would replay
    // the same rejected credential on every request from here to sign-in.
    // Scoped to exactly this branch: a store-independent 5xx (adminAuth
    // checks the bearer before the store is ever touched, so a Postgres
    // blip never 401s a good token) never reaches here.
    //
    // Compare against `token` (captured above, BEFORE this request went out)
    // rather than re-reading storage here: a slow/stale request sent with NO
    // token (or an OLD one) can resolve its 401 AFTER a newer token has
    // already been stored — e.g. an unauthenticated mount probe still in
    // flight when sign-in stores a fresh token a moment later. Clearing on
    // the FRESH read would wipe that newer, perfectly valid credential out
    // from under the request that just set it. Only a 401 whose storage
    // STILL holds the exact token it was sent with is the real rejection.
    if (token && getToken() === token) setToken(null);
    // The 401 body is thrown over, never handed to a caller — drain it here or
    // the rejected request stays open on its connection (see drainBody).
    await drainBody(res);
    _unauthorized?.(refused);
    throw new HttpError(401, "Unauthorized");
  }
  return res;
}

export async function asJson<T>(res: Response): Promise<T> {
  if (!res.ok) {
    const { message, reason, org } = await errEnvelope(res);
    throw new HttpError(res.status, message, reason, org);
  }
  return (await res.json()) as T;
}

// The ONE parser for the control plane's `{"error":"<human message>"}` envelope:
// surface that message verbatim (readable in a toast / inline error), fall back to
// the raw body, then the status text. It returns rather than throws, so it serves
// both the paths where a non-2xx is an EXPECTED, actionable outcome the caller
// renders inline (e.g. verifyWorkspace's 422/503/409) and asJson, which wraps it
// in an HttpError. Change the envelope here and both follow. `reason` is the
// envelope's optional machine-readable class ("" when absent or not a string),
// carried only on the envelope path — a raw body has no class to read.
// Readability/DoS, not injection — React escapes whatever this
// returns either way. But a misrouted request can land on a web server or
// proxy in front of wardynd instead of the daemon itself, and that answer's
// body is neither ours nor small (an nginx/ALB error page, a captive-portal
// interstitial). Surfacing it verbatim in a toast is a body-of-unknown-size
// rendered as text; the guard below caps what the raw-body fallback will
// ever hand back, in bytes and in shape (never something that opens like a
// markup document). The `{"error":"…"}` envelope is unaffected — a real API
// error message is always small and is extracted before the guard runs.
const RAW_BODY_MAX_CHARS = 500;
function isRawBodyDisplayable(body: string): boolean {
  return body.length <= RAW_BODY_MAX_CHARS && !/^\s*</.test(body);
}

export async function errEnvelope(res: Response): Promise<{ message: string; reason: string; org: string }> {
  try {
    const body = await res.text();
    if (!body) return { message: res.statusText, reason: "", org: "" };
    try {
      const j = JSON.parse(body) as { error?: unknown; reason?: unknown; org?: unknown };
      if (typeof j.error === "string" && j.error) {
        return {
          message: j.error,
          reason: typeof j.reason === "string" ? j.reason : "",
          org: typeof j.org === "string" ? j.org : "",
        };
      }
    } catch {
      // not JSON — fall through to the raw-body guard below
    }
    return { message: isRawBodyDisplayable(body) ? body : res.statusText, reason: "", org: "" };
  } catch {
    return { message: res.statusText, reason: "", org: "" };
  }
}

export async function errText(res: Response): Promise<string> {
  return (await errEnvelope(res)).message;
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

// The three answers a mount-time auth probe can honestly give. R4/F027: a
// plain boolean here would make a daemon 5xx and a dead network both return
// exactly `false` — indistinguishable from a real 401 — so App.tsx would turn
// that single bit into "unauthed" and render the sign-in gate with no hint
// that anything was down. sign-in.tsx:116-123 already makes this split by hand
// for submitToken, for the same reason and in the same words ("probeAuth
// collapsed every failure ... to the same boolean `false`"). Splitting it HERE
// is the one place both call sites can share, rather than a third hand-rolled
// copy.
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
    const verdict: AuthProbe = res.ok ? "authed" : "unreachable";
    // The probe wants the STATUS and nothing else — but the row it asked for
    // still arrived, and an unread body never completes the request (see
    // drainBody). This one fires on EVERY cold document load, so an unread
    // body here would leak on every route and in every role.
    await drainBody(res);
    return verdict;
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
