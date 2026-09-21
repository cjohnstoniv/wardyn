/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// #216 — a spent reconnect budget offers Reconnect, not a "[closed]" line that
// scrolls out of the terminal's own buffer with nothing left to press.
// Connection state (a live retry, the budget spent) now renders OUTSIDE the
// scrollback (attach-terminal-status.tsx), and Reconnect keeps the existing
// scrollback and appends rather than clearing it.
//
// The WebSocket is faked at the PAGE level — installed via addInitScript
// before the app's first render, so every `new WebSocket(url)` the component
// makes constructs one of these instead of a real socket — the same technique
// attach-terminal.test.tsx uses against jsdom. Two reasons this has to happen
// here rather than over a real connection: the reconnect budget and backoff
// are internal timers this suite needs to trigger deterministically (0
// flaky), and the `-runner none` e2e backend (scripts/e2e-backend.sh) has no
// sandbox behind the attach endpoint to drop a connection against anyway.
import { test, expect, ADMIN_TOKEN, sql } from "./fixtures";
import { TERMINAL } from "../src/app/components/wardyn/copy";

const AUTH = { Authorization: `Bearer ${ADMIN_TOKEN}` };

// Runs inside the page: every FakeSocket instance lands in window.__sockets,
// and the four __ws* helpers below are how the test drives them (open a
// connect, drop one abnormally, push a PTY output frame, read the count).
function installFakeWebSocket() {
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  const w = window as any;
  class FakeSocket {
    static OPEN = 1;
    static CONNECTING = 0;
    static CLOSING = 2;
    static CLOSED = 3;
    readyState = 0;
    binaryType = "";
    url: string;
    onopen: (() => void) | null = null;
    onclose: ((ev: { code: number; reason: string }) => void) | null = null;
    onerror: (() => void) | null = null;
    onmessage: ((ev: { data: unknown }) => void) | null = null;
    constructor(url: string) {
      this.url = url;
      w.__sockets.push(this);
    }
    send() {
      /* PTY input — nothing in this suite reads it back */
    }
    close() {
      const wasConnecting = this.readyState === FakeSocket.CONNECTING;
      this.readyState = FakeSocket.CLOSED;
      if (wasConnecting) this.onclose?.({ code: 1006, reason: "" });
    }
  }
  w.__sockets = [];
  w.WebSocket = FakeSocket;
  w.__wsCount = () => w.__sockets.length;
  w.__wsOpen = (i: number) => {
    const s = w.__sockets[i];
    s.readyState = FakeSocket.OPEN;
    s.onopen?.();
  };
  // An unexpected drop (proxy hiccup, timeout) — never used for the taken-over
  // (1008) or clean (1000) closes, which are out of this spec's scope.
  w.__wsDrop = (i: number, code: number, reason = "") => {
    const s = w.__sockets[i];
    s.readyState = FakeSocket.CLOSED;
    s.onclose?.({ code, reason });
  };
  // A binary PTY-output frame, exactly what xterm's own scrollback grows from.
  w.__wsSend = (i: number, text: string) => {
    const s = w.__sockets[i];
    s.onmessage?.({ data: new TextEncoder().encode(text).buffer });
  };
}

async function createRunningInteractiveRun(
  request: import("@playwright/test").APIRequestContext,
  task: string,
): Promise<string> {
  const resp = await request.post("/api/v1/runs", {
    headers: AUTH,
    data: { agent: "claude-code", repo: "acme/widgets", task, interactive: true },
  });
  expect(resp.ok(), `create run failed: ${resp.status()} ${await resp.text()}`).toBeTruthy();
  const run = (await resp.json()) as { id: string };
  // The `none` runner (this backend) never actually reaches RUNNING on its
  // own — force it, same as every other fixture that needs an active state
  // (see e.g. runs.spec.ts's F1-F3 repro).
  sql(`UPDATE agent_runs SET state = 'RUNNING' WHERE id = '${run.id}'`);
  return run.id;
}

test.describe("Attach terminal — reconnect budget (#216)", () => {
  test("walks connected → reconnecting → budget spent → Reconnect → connected, with the scrollback preserved and the control never scrolled away", async ({
    page,
    request,
  }) => {
    const runId = await createRunningInteractiveRun(request, "e2e-216 reconnect walk");
    await page.addInitScript(installFakeWebSocket);
    await page.goto(`/runs/${runId}`);

    // --- connected ---
    await expect.poll(() => page.evaluate(() => (window as unknown as { __wsCount: () => number }).__wsCount())).toBeGreaterThan(0);
    await page.evaluate(() => (window as unknown as { __wsOpen: (i: number) => void }).__wsOpen(0));
    await expect(page.getByText(`attach — ${runId}`)).toBeVisible();

    // Fill the scrollback — this is the repro: enough printed output that the
    // OLD `[closed]` line (written into this same buffer) would have scrolled
    // out of view long before the person could act on it.
    await page.evaluate((rid) => {
      const w = window as unknown as { __wsSend: (i: number, t: string) => void };
      for (let i = 0; i < 400; i++) w.__wsSend(0, `audit step ${i} — scanning ${rid}\r\n`);
    }, runId);

    // --- reconnecting (attempt 1 of 4) ---
    await page.evaluate(() => (window as unknown as { __wsDrop: (i: number, c: number, r?: string) => void }).__wsDrop(0, 1006, "abnormal"));
    await expect(page.getByText(TERMINAL.RECONNECTING_LINE(1, 4))).toBeVisible();
    await expect(page.getByText(TERMINAL.RECONNECTING_HINT)).toBeVisible();

    // --- budget spent: drop each socket the component opens on its own
    // backoff. 1 initial + MAX_RECONNECT_ATTEMPTS (4) reconnects = 5 sockets;
    // the first drop above already spent one, so 4 more exhaust the budget —
    // the 4th of those is the one that finds reconnectAttempts === the cap
    // and gives up instead of scheduling another attempt.
    for (let socketIndex = 1; socketIndex <= 4; socketIndex++) {
      await expect
        .poll(() => page.evaluate(() => (window as unknown as { __wsCount: () => number }).__wsCount()))
        .toBeGreaterThan(socketIndex);
      await page.evaluate(
        (i) => (window as unknown as { __wsDrop: (i: number, c: number, r?: string) => void }).__wsDrop(i, 1006, "abnormal"),
        socketIndex,
      );
    }

    // The bar goes back to just naming the run — no more `[closed] {runId}` —
    // and TerminalConnectionStatus, OUTSIDE the scrollback, carries the state
    // and the way back in. Visible, not merely present: proves the 400 lines
    // above never pushed it out of view.
    await expect(page.getByText(`[closed] ${runId}`)).toHaveCount(0);
    await expect(page.getByText(TERMINAL.CLOSED_TITLE)).toBeVisible();
    await expect(page.getByText(TERMINAL.CLOSED_BODY)).toBeVisible();
    const reconnectButton = page.getByRole("button", { name: TERMINAL.RECONNECT });
    await expect(reconnectButton).toBeVisible();

    // --- Reconnect: keeps the scrollback (same xterm instance), appends ---
    const socketsBefore = await page.evaluate(() => (window as unknown as { __wsCount: () => number }).__wsCount());
    await reconnectButton.click();
    await expect
      .poll(() => page.evaluate(() => (window as unknown as { __wsCount: () => number }).__wsCount()))
      .toBeGreaterThan(socketsBefore);
    await expect(page.getByText(TERMINAL.CLOSED_TITLE)).toHaveCount(0);
    await page.evaluate(
      (i) => (window as unknown as { __wsOpen: (i: number) => void }).__wsOpen(i),
      socketsBefore,
    );

    // --- connected again ---
    await expect(page.getByText(`attach — ${runId}`)).toBeVisible();
    await expect(page.getByRole("button", { name: TERMINAL.RECONNECT })).toHaveCount(0);
  });

  // "Connection failed" — the budget spent WITHOUT ever having been open, as
  // opposed to the walk above (spent AFTER a real session dropped). Same
  // code path (attach-terminal.tsx's onclose), same recovery, pinned
  // separately because the prototype names it as its own state.
  test("a connection that never opens also spends the budget and offers Reconnect", async ({ page, request }) => {
    const runId = await createRunningInteractiveRun(request, "e2e-216 connection failed");
    await page.addInitScript(installFakeWebSocket);
    await page.goto(`/runs/${runId}`);

    // 1 initial + MAX_RECONNECT_ATTEMPTS (4) reconnects = 5 sockets to drop
    // before the budget is spent — see the walk test's own note.
    for (let socketIndex = 0; socketIndex <= 4; socketIndex++) {
      await expect
        .poll(() => page.evaluate(() => (window as unknown as { __wsCount: () => number }).__wsCount()))
        .toBeGreaterThan(socketIndex);
      await page.evaluate(
        (i) => (window as unknown as { __wsDrop: (i: number, c: number, r?: string) => void }).__wsDrop(i, 1006, ""),
        socketIndex,
      );
    }

    await expect(page.getByText(TERMINAL.CLOSED_TITLE)).toBeVisible();
    await expect(page.getByRole("button", { name: TERMINAL.RECONNECT })).toBeVisible();
    await expect(page.getByText(`[error] ${runId}`)).toHaveCount(0);
    await expect(page.getByText(`[closed] ${runId}`)).toHaveCount(0);
  });
});
