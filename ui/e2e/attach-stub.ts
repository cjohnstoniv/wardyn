/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Hermetic attach stubs shared by cockpit-terminal.spec.ts.
//
// WHY hermetic (issue #132): this daemon runs `-runner none`
// (scripts/e2e-backend.sh) — there is no runner to actually open a PTY, so a
// real attach WebSocket refuses before the upgrade completes. Every case in
// the sibling spec intercepts three things: the run read (so TerminalPane
// mounts AttachTerminal instead of the autonomous/starting notice), the
// attach-ticket POST, and the socket itself — the same technique
// providers.spec.ts's `attachWithRealTerminal` already establishes for the
// harness-login pane's REAL-terminal cases.
//
// The seeded backend's "e2e fixture 2" row is already RUNNING (see
// scripts/e2e-backend.sh's row_number->state map), but the create body never
// sets `interactive` — so TerminalPane would otherwise render the autonomous
// "Output" branch instead of AttachTerminal. stubInteractiveRun splices just
// that one field onto the REAL response (route.fetch() + patch, runs.spec.ts's
// own precedent) rather than editing the shared seed script: every other
// field on the run (repo, created_by, task, the audit/approvals/layout reads
// other widgets on the page make) still comes from the real backend, so this
// stays scoped to this one spec file and touches no other spec's fixture.
import type { BrowserContext, Page, WebSocketRoute } from "@playwright/test";
import { ADMIN_TOKEN } from "./fixtures";
import type { AttachHolder, AttachModeMsg } from "../src/app/lib/types/runs";

const auth = { Authorization: `Bearer ${ADMIN_TOKEN}` };

// Matches attach-terminal.tsx's buildWsUrl: /api/v1/runs/{id}/attach[?ticket=…].
export const ATTACH_WS_PATTERN = /\/api\/v1\/runs\/[^/]+\/attach/;

/** The real seeded RUNNING fixture (task "e2e fixture 2") — its id, so every
 *  case navigates to a run that genuinely exists in the backend. */
export async function findRunningFixture(page: Page): Promise<{ id: string; created_by: string }> {
  const runs = await (await page.request.get("/api/v1/runs?limit=1000", { headers: auth })).json();
  const target = runs.find((r: { task: string; state: string }) => r.task === "e2e fixture 2" && r.state === "RUNNING");
  if (!target) throw new Error("seeded RUNNING fixture 2 not found");
  return target as { id: string; created_by: string };
}

/** Splice interactive:true onto the real run's GET response — see the file
 *  doc above for why this patches the response instead of the shared seed.
 *  Page- or context-scoped, so the two-tabs case's second page is covered by
 *  registering once on the context. */
export async function stubInteractiveRun(target: Page | BrowserContext, runId: string): Promise<void> {
  await target.route(`**/api/v1/runs/${runId}`, async (route) => {
    if (route.request().method() !== "GET") return route.fallback();
    const response = await route.fetch();
    const json = await response.json();
    json.interactive = true;
    json.state = "RUNNING";
    await route.fulfill({ response, json });
  });
}

/** POST /api/v1/runs/{id}/attach-ticket — a fixed, deterministic ticket (the
 *  browser cannot put the admin bearer on a WS handshake; see
 *  attach-terminal.tsx's isAdminTokenOnlyMode). */
export async function stubAttachTicket(target: Page | BrowserContext, runId: string): Promise<void> {
  await target.route(`**/api/v1/runs/${runId}/attach-ticket`, (route) =>
    route.fulfill({ json: { ticket: "e2e-cockpit-ticket" } }),
  );
}

/** POST /api/v1/runs/{id}/attach/takeover — the server's own 200. */
export async function stubTakeover(target: Page | BrowserContext, runId: string): Promise<void> {
  await target.route(`**/api/v1/runs/${runId}/attach/takeover`, (route) => route.fulfill({ json: {} }));
}

// The attach-mode control frame the daemon sends as a TEXT frame on every
// connect (internal/api/attach_holder.go) — same shape
// attach-terminal-session.test.tsx's attachModeFrame builds at the component
// level; this is its browser-level twin. Typed against the TS mirror that
// wire-parity.test.ts pins to the Go struct, so a rename there breaks this.
export function attachModeFrame(readOnly: boolean, holder?: Partial<AttachHolder> & { principal: string }): string {
  const holderDefaults: AttachHolder = { held: true, since: "2026-09-21T12:00:00Z", cols: 80, rows: 24, source: "web" };
  return JSON.stringify({
    type: "attach-mode",
    read_only: readOnly,
    holder: holder ? { ...holderDefaults, ...holder } : undefined,
  } satisfies AttachModeMsg);
}

/**
 * Wire one hermetic attach socket, page- or context-scoped (context so a
 * second tab's connection is covered too — the two-tabs case's whole point).
 * `onConnect` is called with the connection's 1-based ordinal and the fresh
 * `WebSocketRoute`, so a case can send a different attach-mode frame per
 * connection (writer first, observer second) or capture the handle to send a
 * later frame on the SAME socket (in-place promotion).
 */
export async function stubAttachSocket(
  target: Page | BrowserContext,
  onConnect: (n: number, ws: WebSocketRoute) => void,
): Promise<{ opens: () => number }> {
  let opens = 0;
  await target.routeWebSocket(ATTACH_WS_PATTERN, (ws) => {
    opens += 1;
    onConnect(opens, ws);
  });
  return { opens: () => opens };
}

/** A second tab on the same run — localStorage is per-origin, shared by every
 *  page in this BrowserContext once the FIRST page has written the admin
 *  token, but a tab opened before that write (or as the very first page of a
 *  fresh context) needs its own copy — same key `fixtures.ts`'s own `test`
 *  fixture writes, mirrored here since that key is not exported. */
export async function newAuthedPage(context: BrowserContext): Promise<Page> {
  const page = await context.newPage();
  await page.addInitScript(
    ([key, tok]) => {
      try {
        localStorage.setItem(key, tok);
      } catch {
        /* private mode — ignore */
      }
    },
    ["wardyn_admin_token", ADMIN_TOKEN],
  );
  return page;
}
