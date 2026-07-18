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

// Confinement tiers weakest→strongest, for clamping a run's requested class up to
// its policy floor before create (a weaker request 422s server-side).
export const ccRank = (cc: string): number =>
  ({ CC1: 1, CC2: 2, CC3: 3 })[cc as "CC1" | "CC2" | "CC3"] ?? 0;

// Central fetch wrapper:
//  (a) attaches Bearer token when a wardyn_admin_token is set,
//  (b) always sends the OIDC session cookie (credentials: 'include'),
//  (c) routes HTTP 401 to the module-level onUnauthorized handler.
export async function wfetch(path: string, init: RequestInit = {}): Promise<Response> {
  const headers = new Headers(init.headers);
  const token = getToken();
  if (token) headers.set("Authorization", `Bearer ${token}`);
  if (init.body != null && !headers.has("Content-Type")) {
    headers.set("Content-Type", "application/json");
  }
  headers.set("Accept", headers.get("Accept") ?? "application/json");

  const res = await fetch(`${BASE}${path}`, {
    ...init,
    headers,
    credentials: "include",
  });

  if (res.status === 401) {
    _unauthorized?.();
    throw new HttpError(401, "Unauthorized");
  }
  return res;
}

export async function asJson<T>(res: Response): Promise<T> {
  if (!res.ok) {
    let detail = res.statusText;
    try {
      const body = await res.text();
      if (body) {
        // The control plane returns `{"error": "<human message>"}` on failures.
        // Surface that message verbatim (readable in a toast / inline error) and
        // fall back to the raw body only when it is not that shape.
        try {
          const j = JSON.parse(body) as { error?: unknown };
          detail = typeof j.error === "string" && j.error ? j.error : body;
        } catch {
          detail = body;
        }
      }
    } catch {
      /* ignore */
    }
    throw new HttpError(res.status, detail);
  }
  return (await res.json()) as T;
}

// Read an error/response body as a human string WITHOUT throwing (unlike asJson):
// prefer the control plane's `{"error":"…"}` message, fall back to the raw body,
// then the status text. Used where a non-2xx is an EXPECTED, actionable outcome
// the caller renders inline (e.g. verifyWorkspace's 422/503/409).
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
// Fleet / Audit / Workspaces views and the attention-badge poll keep showing the
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

// Some backends wrap collections as { items: [...] }; tolerate both.
export function unwrapList<T>(payload: unknown): T[] {
  if (Array.isArray(payload)) return payload as T[];
  if (payload && typeof payload === "object") {
    const obj = payload as Record<string, unknown>;
    for (const key of ["items", "data", "results"]) {
      if (Array.isArray(obj[key])) return obj[key] as T[];
    }
  }
  return [];
}

// Probe auth by hitting a protected endpoint. It needs only a yes/no on the
// response status, so it asks for a single row (?limit=1) rather than pulling
// the whole runs list just to discard it.
export async function probeAuth(): Promise<boolean> {
  try {
    const res = await wfetch(withLimit("/runs", 1), { method: "GET" });
    return res.ok;
  } catch {
    return false;
  }
}

// Small typed coercion helpers shared by the audit/grant/recording projections.
export function str(v: unknown): string | undefined {
  return typeof v === "string" ? v : undefined;
}
export function num(v: unknown): number | undefined {
  return typeof v === "number" ? v : undefined;
}
