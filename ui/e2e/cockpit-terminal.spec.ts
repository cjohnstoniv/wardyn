/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

/*
 * Run cockpit terminal (issue #132) — pins the browser-level behaviour
 * attach-terminal-session.test.tsx (issue #134) pins at the component level:
 * an observer arriving, in-place promotion on the SAME socket, two tabs, a
 * reconnect after an abnormal close, and the no-re-dial rule after a
 * take-over. See attach-stub.ts's file doc for why every case here is
 * hermetic — this daemon runs `-runner none`, so a real attach would refuse
 * before it opens; the run read, the ticket POST and the socket itself are
 * all intercepted rather than left to hit a runner that will never answer.
 *
 * TRAP (repo law): terminal assertions on `toContainText` starve — the xterm
 * buffer does not settle the way that matcher expects. Every text assertion
 * below polls `.xterm-screen`'s innerText via demos.ts's pollScreen instead.
 */
import type { WebSocketRoute } from "@playwright/test";
import { test, expect, gotoConsole, navToRoute } from "./fixtures";
import {
  attachModeFrame,
  findRunningFixture,
  newAuthedPage,
  stubAttachSocket,
  stubAttachTicket,
  stubInteractiveRun,
  stubTakeover,
} from "./attach-stub";
import { pollScreen } from "./demo/demos";
import { RUN_COCKPIT, TERMINAL } from "../src/app/components/wardyn/copy";

test.describe.configure({ mode: "serial" });

test.describe("Run cockpit terminal", () => {
  test("an observer arrives to the read-only treatment and the take-over affordance", async ({ page }) => {
    const { id: runId } = await findRunningFixture(page);
    await stubInteractiveRun(page, runId);
    await stubAttachTicket(page, runId);
    await stubAttachSocket(page, (_n, ws) => {
      ws.send(attachModeFrame(true, { principal: "bob@e2e.example" }));
    });

    await gotoConsole(page);
    await navToRoute(page, `/runs/${runId}`);

    const pane = page.getByTestId("run-terminal-pane");
    await expect(pane.locator(".xterm-screen").first()).toBeVisible();

    // The state chip and the watching label are both the SERVER's own words,
    // never inferred from silence (attach-terminal.tsx's own contract).
    await expect(pane.getByText(RUN_COCKPIT.heldBy("bob@e2e.example"))).toBeVisible();
    await expect(pane.getByText(RUN_COCKPIT.watchingReadOnly)).toBeVisible();
    await expect(pane.getByText(RUN_COCKPIT.driving)).toHaveCount(0);

    // The footer: the hint, and exactly the one action it offers.
    await expect(pane.getByText(RUN_COCKPIT.heldHint)).toBeVisible();
    await expect(pane.getByRole("button", { name: RUN_COCKPIT.takeOver })).toBeVisible();
  });

  test("an in-place promotion on the same socket clears the observer chrome and typed input lands", async ({
    page,
  }) => {
    const { id: runId } = await findRunningFixture(page);
    await stubInteractiveRun(page, runId);
    await stubAttachTicket(page, runId);

    // Captured so the promotion frame below rides the SAME connection — no
    // second `page.routeWebSocket` registration, no reconnect.
    let serverWs: WebSocketRoute | null = null;
    await stubAttachSocket(page, (_n, ws) => {
      serverWs = ws;
      ws.send(attachModeFrame(true, { principal: "bob@e2e.example" }));
      // Echo a binary (PTY input) frame straight back as PTY OUTPUT — the
      // same round trip a real shell gives an operator — so a typed
      // character is provable on screen, not merely "a frame was sent".
      ws.onMessage((msg) => {
        if (Buffer.isBuffer(msg)) ws.send(msg);
      });
    });

    await gotoConsole(page);
    await navToRoute(page, `/runs/${runId}`);

    const pane = page.getByTestId("run-terminal-pane");
    const screen = pane.locator(".xterm-screen").first();
    await expect(screen).toBeVisible();
    await expect(pane.getByRole("button", { name: RUN_COCKPIT.takeOver })).toBeVisible();

    // The regression attach-terminal-session.test.tsx names as the one that
    // matters most: read_only true -> false on the SAME socket, no reconnect.
    serverWs!.send(attachModeFrame(false, { principal: "me@e2e.example" }));

    await expect(pane.getByRole("button", { name: RUN_COCKPIT.takeOver })).toHaveCount(0);
    await expect(pane.getByText(RUN_COCKPIT.heldHint)).toHaveCount(0);
    await expect(pane.getByText(RUN_COCKPIT.driving)).toBeVisible();

    // term.focus() fires on promotion (D3, attach-terminal.tsx) — click the
    // screen anyway, the same reliable path overlay.ts's typeInTerminal uses:
    // xterm takes keystrokes through an offscreen textarea.
    await screen.click();
    const marker = "promoted-input-ok";
    await page.keyboard.type(marker);
    await pollScreen(screen, new RegExp(marker), "typed input after promotion never echoed back to the screen");
  });

  test("two tabs: the writer stays driving, the observer stays read-only, and each names the other correctly", async ({
    page,
    context,
  }) => {
    const { id: runId } = await findRunningFixture(page);
    await stubInteractiveRun(context, runId);
    await stubAttachTicket(context, runId);
    // Connection order decides the role: the first tab to attach is the
    // writer (no PTY to share yet), the second lands read-only — the shared
    // tmux session's own rule (attach-terminal.tsx's file doc).
    await stubAttachSocket(context, (n, ws) => {
      if (n === 1) ws.send(attachModeFrame(false));
      else ws.send(attachModeFrame(true, { principal: "tab-a@e2e.example" }));
    });

    await gotoConsole(page);
    await navToRoute(page, `/runs/${runId}`);
    const paneA = page.getByTestId("run-terminal-pane");
    await expect(paneA.locator(".xterm-screen").first()).toBeVisible();
    await expect(paneA.getByText(RUN_COCKPIT.driving)).toBeVisible();

    const page2 = await newAuthedPage(context);
    await gotoConsole(page2);
    await navToRoute(page2, `/runs/${runId}`);
    const paneB = page2.getByTestId("run-terminal-pane");
    await expect(paneB.locator(".xterm-screen").first()).toBeVisible();
    await expect(paneB.getByText(RUN_COCKPIT.heldBy("tab-a@e2e.example"))).toBeVisible();
    await expect(paneB.getByRole("button", { name: RUN_COCKPIT.takeOver })).toBeVisible();

    // Tab A never grew the observer chrome — arriving second is what earns it.
    await expect(paneA.getByRole("button", { name: RUN_COCKPIT.takeOver })).toHaveCount(0);

    await page2.close();
  });

  test("a reconnect happens after an abnormal close", async ({ page }) => {
    const { id: runId } = await findRunningFixture(page);
    await stubInteractiveRun(page, runId);
    await stubAttachTicket(page, runId);

    let latestWs: WebSocketRoute | null = null;
    const { opens } = await stubAttachSocket(page, (_n, ws) => {
      latestWs = ws;
      ws.send(attachModeFrame(false));
    });

    await gotoConsole(page);
    await navToRoute(page, `/runs/${runId}`);
    const pane = page.getByTestId("run-terminal-pane");
    const screen = pane.locator(".xterm-screen").first();
    await expect(screen).toBeVisible();
    await expect(pane.getByText(RUN_COCKPIT.driving)).toBeVisible();
    expect(opens()).toBe(1);

    // Server-initiated abnormal close — the persistent tmux session survives
    // this on the real daemon, so the client re-attaches (attach-terminal.tsx's
    // bounded reconnect), unlike the 1008/taken-over case below.
    //
    // Connection state renders OUTSIDE the scrollback, in
    // attach-terminal-status.tsx's persistent strip — never as bracket text
    // written into the xterm buffer (#216, closed by PR #298 "Offer Reconnect
    // instead of a scrollable [closed] line", which is the approved canon).
    // The `[connection lost — reconnecting]` / `[reconnected]` lines this test
    // used to poll the SCREEN for were removed from the product by that PR,
    // three hours before this spec was even written — a stale assertion, not
    // a product regression.
    await latestWs!.close({ code: 1006, reason: "abnormal" });

    await expect(pane.getByText(TERMINAL.RECONNECTING_LINE(1, 4))).toBeVisible();
    await expect
      .poll(() => opens(), { timeout: 10_000, message: "no second socket opened after the abnormal close" })
      .toBeGreaterThanOrEqual(2);
    await expect(pane.getByText(RUN_COCKPIT.driving)).toBeVisible();
    await expect(pane.getByText(TERMINAL.RECONNECTING_LINE(1, 4))).toHaveCount(0);
  });

  // The server's evict-and-reconnect path is a take-over by a principal with NO
  // queued observer socket: handleAttachTakeover frees the slot and answers
  // promoted:false. In the console that is a panel that was itself displaced
  // (close 1008), so its socket is already gone and doTakeover reclaims with a
  // fresh attach. An OPEN read-only panel never gets promoted:false: its own
  // queued socket is what the server promotes in place (#507, the next test).
  test("a displaced panel's take-over reconnects exactly once — it does not re-dial", async ({ page }) => {
    const { id: runId } = await findRunningFixture(page);
    await stubInteractiveRun(page, runId);
    await stubAttachTicket(page, runId);
    // The server's full answer for a taker with no socket to promote.
    await page.route(`**/api/v1/runs/${runId}/attach/takeover`, (route) =>
      route.fulfill({
        json: { taken_over: true, previous_holder: "bob@e2e.example", previous_source: "web", promoted: false },
      }),
    );
    // The no-re-dial window below is a page.clock jump, not a real sleep.
    await page.clock.install();

    let firstWs: WebSocketRoute | null = null;
    const { opens } = await stubAttachSocket(page, (n, ws) => {
      if (n === 1) firstWs = ws;
      ws.send(attachModeFrame(false, { principal: "me@e2e.example" }));
    });

    await gotoConsole(page);
    await navToRoute(page, `/runs/${runId}`);
    const pane = page.getByTestId("run-terminal-pane");
    await expect(pane.locator(".xterm-screen").first()).toBeVisible();
    await expect(pane.getByText(RUN_COCKPIT.driving)).toBeVisible();

    // bob takes the terminal from us: the server closes our socket 1008 with
    // the reason the UI matches, and the panel does not reconnect on its own.
    await firstWs!.close({ code: 1008, reason: "taken over by bob@e2e.example" });
    await expect(pane.getByText(RUN_COCKPIT.displacedHint("bob@e2e.example"))).toBeVisible();
    expect(opens()).toBe(1);

    await pane.getByRole("button", { name: RUN_COCKPIT.takeOver }).click();
    await page.getByRole("alertdialog").getByRole("button", { name: RUN_COCKPIT.takeOver }).click();

    // promoted:false, so doTakeover RECLAIMS: exactly ONE further open.
    await expect
      .poll(() => opens(), { timeout: 10_000, message: "the reclaim never opened its one further socket" })
      .toBe(2);
    await expect(pane.getByText(RUN_COCKPIT.driving)).toBeVisible();

    // The bound the issue names: "does not re-dial" means exactly one
    // further open across a whole window, never merely "no crash" — assert
    // no THIRD socket opens once the reclaim has settled. The window runs past
    // RECONNECT_MAX_DELAY_MS (attach-terminal.tsx, 5s), so any backoff re-dial
    // has fired; the driving check adds a round trip so a re-dial's socket
    // reaches opens() before it is read.
    await page.clock.fastForward("00:06");
    await expect(pane.getByText(RUN_COCKPIT.driving)).toBeVisible();
    expect(opens()).toBe(2);
  });

  test("a take-over that promoted the caller's own socket in place does not re-dial (#507)", async ({ page }) => {
    const { id: runId } = await findRunningFixture(page);
    await stubInteractiveRun(page, runId);
    await stubAttachTicket(page, runId);
    // The server's real shape when it flipped our own queued observer socket
    // to writer in place, rather than evicting-then-reconnecting.
    await stubTakeover(page, runId, { promoted: true, previousHolder: "bob@e2e.example" });

    let serverWs: WebSocketRoute | null = null;
    const { opens } = await stubAttachSocket(page, (_n, ws) => {
      serverWs = ws;
      ws.send(attachModeFrame(true, { principal: "bob@e2e.example" }));
      // Echo a binary (PTY input) frame straight back as PTY OUTPUT — same
      // round trip the in-place-promotion case above proves writability with.
      ws.onMessage((msg) => {
        if (Buffer.isBuffer(msg)) ws.send(msg);
      });
    });

    await gotoConsole(page);
    await navToRoute(page, `/runs/${runId}`);
    const pane = page.getByTestId("run-terminal-pane");
    const screen = pane.locator(".xterm-screen").first();
    await expect(screen).toBeVisible();
    await expect(pane.getByRole("button", { name: RUN_COCKPIT.takeOver })).toBeVisible();

    await pane.getByRole("button", { name: RUN_COCKPIT.takeOver }).click();
    await page.getByRole("alertdialog").getByRole("button", { name: RUN_COCKPIT.takeOver }).click();

    // doTakeover reads promoted:true and returns early while wsRef is still
    // OPEN (#507) — no evict, no reconnect, so opens() never grows past 1.
    // Assert against the SAME text the reclaim test polls up to 2, so this is
    // a genuine "stays at 1" bound, not just "never checked".
    await expect
      .poll(() => opens(), { timeout: 3_000, message: "a promoted take-over must not open a second socket" })
      .toBe(1);

    // The server's own promotion notice: an in-flight attach-mode frame,
    // read_only:false, on the SAME socket doTakeover left open.
    serverWs!.send(attachModeFrame(false, { principal: "me@e2e.example" }));

    await expect(pane.getByRole("button", { name: RUN_COCKPIT.takeOver })).toHaveCount(0);
    await expect(pane.getByText(RUN_COCKPIT.heldHint)).toHaveCount(0);
    await expect(pane.getByText(RUN_COCKPIT.driving)).toBeVisible();
    expect(opens()).toBe(1);

    // Writable proof, same technique as the in-place-promotion case above.
    await screen.click();
    const marker = "reclaim-promoted-input-ok";
    await page.keyboard.type(marker);
    await pollScreen(screen, new RegExp(marker), "typed input after a promoted take-over never echoed back to the screen");
  });

  // SF-31: unit tests cover decideKey (attach-terminal-keys.test.ts's per-layout
  // matrix) and a mocked handler (attach-terminal.test.tsx's F144 describe) —
  // the advertised chord was never pressed against a REAL xterm, the one
  // widget that also owns Tab/Shift-Tab/Escape (the trap WCAG 2.1.2 requires
  // this advertised exit for). This is that proof.
  test("the advertised escape chord leaves the terminal without a keyboard trap and reaches no PTY (#133)", async ({
    page,
  }) => {
    const { id: runId } = await findRunningFixture(page);
    await stubInteractiveRun(page, runId);
    await stubAttachTicket(page, runId);
    const received: Buffer[] = [];
    await stubAttachSocket(page, (_n, ws) => {
      // Driving, not observing — read-only would make "nothing was sent"
      // vacuous instead of a proof the chord itself is what stopped it.
      ws.send(attachModeFrame(false));
      ws.onMessage((msg) => {
        if (Buffer.isBuffer(msg)) {
          received.push(msg);
          ws.send(msg);
        }
      });
    });

    await gotoConsole(page);
    await navToRoute(page, `/runs/${runId}`);
    const pane = page.getByTestId("run-terminal-pane");
    const screen = pane.locator(".xterm-screen").first();
    await expect(screen).toBeVisible();
    await expect(pane.getByText(RUN_COCKPIT.driving)).toBeVisible();

    // Land inside the trap first — ordinary typed input DOES reach the PTY on
    // this same socket, so the chord's "nothing sent" below is a contrast,
    // not a tautology.
    await screen.click();
    await page.keyboard.type("still typing");
    await pollScreen(screen, /still typing/, "ordinary typed input never echoed — the stub itself is broken");
    expect(received.length, "ordinary typing must reach the PTY").toBeGreaterThan(0);
    received.length = 0;

    await page.keyboard.press("Control+Shift+Backspace");
    // The panel (tabIndex=-1, attach-terminal.tsx's landing pad), never
    // xterm's own hidden textarea — same identification
    // attach-terminal.test.tsx's F144 describe pins at the unit level.
    await expect
      .poll(() => page.evaluate(() => document.activeElement?.getAttribute("tabindex")))
      .toBe("-1");
    await expect
      .poll(() => page.evaluate(() => document.activeElement?.tagName))
      .not.toBe("TEXTAREA");
    expect(received.length, "the chord must reach no PTY").toBe(0);

    // Ctrl+] — the silent US-only fallback (#133) — is the same exit, still
    // typeable with no AltGr involved.
    await screen.click();
    await page.keyboard.press("Control+BracketRight");
    await expect
      .poll(() => page.evaluate(() => document.activeElement?.getAttribute("tabindex")))
      .toBe("-1");
    expect(received.length, "the Ctrl+] fallback must reach no PTY either").toBe(0);
  });
});
