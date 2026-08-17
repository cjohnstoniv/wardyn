/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

/*
 * The demo recording driver.
 *
 * Drives a headed browser through the whole Wardyn story so a screen capture
 * has something to film: first light → the Getting Started funnel (barrier,
 * corporate network, model, the five egress demos, a workspace) → one real
 * governed run that does actual work and hits all three egress verdicts →
 * the audit trail and the session replay.
 *
 * This is NOT a test. It asserts only enough to keep itself honest and to know
 * when to advance; a failure here means the recording is wrong, not that the
 * product is broken. It runs against the REAL compose stack on :8080 (a live
 * runner, real sandboxes, a connected model) — the hermetic `-runner none` e2e
 * backend cannot start a demo sandbox at all.
 *
 * Driven by scripts/record-demo.sh (`make record-demo`). It self-skips without
 * WARDYN_DEMO=1 so a bare `pnpm e2e` can never launch a browser at a
 * developer's live stack and start clicking Launch.
 *
 * Selectors are getByRole + accessible names, matching ui/e2e/fixtures.ts and
 * the rest of the suite: a copy change breaks this loudly and in one place,
 * markup churn does not break it at all. Every literal string it targets is
 * catalogued in docs/DEMO-SCRIPT.md.
 */

import path from "node:path";
import { fileURLToPath } from "node:url";
import { test, expect, chromium, type Browser, type BrowserContext, type Locator, type Page } from "@playwright/test";
import {
  DEMO_TASK,
  DEMO_TITLE,
  DEMO_COMMAND,
  FUNNEL_DEMOS,
  HELD_HOST,
  MODEL_HOST,
  PROOF_RUN_TITLE,
  SHELL_ACT5,
  WORKSPACE_NAME,
  WORKSPACE_PATH,
} from "./task";
import { act, beat, caption, chapter, installOverlay, PACE, spotlight, typeInTerminal } from "./overlay";

test.skip(!process.env.WARDYN_DEMO, "demo recording — run via `make record-demo` (exports WARDYN_DEMO=1)");

// Sandboxes are real containers and the final act runs a real agent, so these
// are minutes, not seconds. They are ceilings for waiting on the PRODUCT; the
// pacing the viewer sees comes from overlay.ts.
const SANDBOX_UP = 180_000;
const APPROVAL_APPEARS = 300_000;
const RUN_FINISHES = 900_000;

// One page for the whole recording — a per-test page would flash a new window
// between acts. describe.serial + a shared page is the standard shape for a
// walkthrough whose steps genuinely depend on each other.
let browser: Browser | undefined;
let context: BrowserContext | undefined;
let page: Page;

// Where the browser's own recording lands. Resolved from this file so it does
// not depend on the process cwd; scripts/record-demo.sh joins console.webm onto
// the terminal segment to make the final video.
const VIDEO_DIR = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "../../test-results/demo-video");
const VIDEO_OUT = path.join(VIDEO_DIR, "console.webm");

test.describe.configure({ mode: "serial" });

test.beforeAll(async ({ browser: fixtureBrowser, playwright }) => {
  // Optional fidelity lane: attach to the real Windows Chrome tester already
  // knows how to launch (~/tester/bin/chrome-cdp.sh --launch) instead of the
  // WSLg Chromium Playwright brings up itself. Same spec either way.
  const cdp = process.env.DEMO_CDP;
  if (cdp) {
    // Attached to a browser we did not launch: it has no recordVideo, so this
    // lane relies on the desktop grab alone (and inherits its occlusion risk).
    browser = await chromium.connectOverCDP(cdp);
    const ctx = browser.contexts()[0] ?? (await browser.newContext());
    page = ctx.pages()[0] ?? (await ctx.newPage());
  } else {
    // The context is built BY HAND with recordVideo rather than taken from the
    // `page` fixture, because playwright.config's `use.video` only reaches
    // contexts the built-in fixtures create — browser.newPage() silently
    // ignores it and records nothing. One context for the whole file means one
    // continuous video across all six acts.
    // An EXPLICIT viewport, not null: the recording is the page, so matching it
    // exactly to the video canvas removes the grey band that window-sized
    // (viewport:null) framing leaves where the browser chrome ate the last
    // ~80px of the 1080.
    context = await fixtureBrowser.newContext({
      viewport: { width: 1920, height: 1080 },
      recordVideo: { dir: VIDEO_DIR, size: { width: 1920, height: 1080 } },
    });
    page = await context.newPage();
  }
  void playwright;

  // A stack booted with an admin token needs it in localStorage before first
  // navigation (the app probes /api/v1/runs on mount to decide auth). The
  // default containerized install is local-mode with no auth at all, so this
  // is inert there.
  const token = process.env.WARDYN_DEMO_TOKEN;
  if (token) {
    await page.addInitScript((t: string) => {
      try {
        localStorage.setItem("wardyn_admin_token", t);
      } catch {
        /* private mode — ignore */
      }
    }, token);
  }
  // Dark-first console, deterministically (theme-provider.tsx storage key) —
  // the same addInitScript trick the docs screenshots use.
  await page.addInitScript(() => {
    try {
      localStorage.setItem("wardyn-theme", "dark");
    } catch {
      /* ignore */
    }
  });
  await installOverlay(page);
  await placeWindowInFrame();
});

/**
 * Put the browser window INSIDE the capture rectangle, and prove it landed.
 *
 * `--window-position=0,0 --window-size=…` is a REQUEST the window manager may
 * ignore, and WSLg's compositor does: on a dual 2560x1440 desktop it placed the
 * window at x≈2292 — outside a 1920x1080+0+0 grab entirely. The recording then
 * succeeds, runs the full six acts, and captures whatever else happened to be
 * in that corner of the screen. (It filmed a browser game once.)
 *
 * CDP's Browser.setWindowBounds is not a hint — it moves the window. Then read
 * the bounds back and fail loudly if they are still outside the frame, because
 * "recorded the wrong pixels" is invisible until someone watches 6 minutes of
 * the wrong thing.
 */
async function placeWindowInFrame(): Promise<void> {
  // WxH+X+Y, same syntax record-demo.sh takes; "full" means no constraint.
  const capture = process.env.WARDYN_DEMO_CAPTURE || "1920x1080+0+0";
  if (capture === "full") return;
  const m = capture.match(/^(\d+)x(\d+)\+(\d+)\+(\d+)$/);
  if (!m) return;
  const [w, h, x, y] = m.slice(1).map(Number);

  const session = await page.context().newCDPSession(page).catch(() => null);
  if (!session) return;
  try {
    const { windowId } = (await session.send("Browser.getWindowForTarget")) as { windowId: number };
    // Size the WINDOW to the viewport PLUS the browser's own chrome. Setting
    // the window to 1920x1080 while the viewport is also 1920x1080 leaves the
    // page squeezed into whatever is left after the tab strip and address bar
    // (~90px), which reads on screen as a dead band under the app. Measure the
    // chrome rather than guessing it — it differs with zoom and channel.
    const chrome = await page
      .evaluate(() => ({
        w: window.outerWidth - window.innerWidth,
        h: window.outerHeight - window.innerHeight,
      }))
      .catch(() => ({ w: 0, h: 0 }));
    await session.send("Browser.setWindowBounds", {
      windowId,
      bounds: {
        left: x,
        top: y,
        width: w + Math.max(0, chrome.w),
        height: h + Math.max(0, chrome.h),
        windowState: "normal",
      },
    });
    const { bounds } = (await session.send("Browser.getWindowForTarget")) as {
      bounds: { left: number; top: number; width: number; height: number };
    };
    // Slack covers the browser chrome we deliberately added plus any snapping
    // the compositor does. This is a "did it land roughly where we asked"
    // check, not a pixel assertion — Acts 1-6 are recorded from inside the
    // page now, so the window's exact placement no longer decides the video.
    const inside =
      bounds.left >= x - 40 &&
      bounds.top >= y - 40 &&
      bounds.left + bounds.width <= x + w + 200 &&
      bounds.top + bounds.height <= y + h + 200;
    if (!inside) {
      throw new Error(
        `browser window is OUTSIDE the capture frame — it is at ` +
          `${bounds.width}x${bounds.height}+${bounds.left}+${bounds.top}, frame is ${capture}. ` +
          `The recording would film whatever else is in that corner of the screen. ` +
          `Set WARDYN_DEMO_CAPTURE to a rect that contains the window, or use DEMO_CDP with a browser you place yourself.`,
      );
    }
  } finally {
    await session.detach().catch(() => {});
  }
}

test.afterAll(async () => {
  await caption(page, "").catch(() => {});
  // Grab the video handle BEFORE closing: closing the context is what flushes
  // and finalizes the file, and saveAs() waits for that to finish.
  const video = page.video();
  if (context) await context.close().catch(() => {});
  if (video) {
    await video.saveAs(VIDEO_OUT).catch(() => {
      /* the raw per-context webm is still in VIDEO_DIR either way */
    });
  }
  // Leave the window open on the CDP lane — it is the human's own browser.
  if (browser) await browser.close().catch(() => {});
});

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

/** The funnel's footer "Next: <step>" button. */
function nextButton(): Locator {
  return page.getByRole("button", { name: /^Next:/ });
}

/** Which step the funnel is on, from the layout's "Step N of M" counter. */
async function stepIndex(): Promise<number> {
  const t = await page
    .getByText(/^Step \d+ of \d+$/)
    .first()
    .textContent()
    .catch(() => null);
  const m = t?.match(/\d+/);
  return m ? Number(m[0]) : -1;
}

/**
 * Advance one funnel step, and confirm it actually advanced.
 *
 * Two behaviours make a single click unreliable:
 *  - A blocked step with a remedy REPLACES Next with its own action button
 *    (setup-layout.tsx: `nextGate.blocked && nextGate.action ? <action> :
 *    <Next>`).
 *  - A step can answer Next by revealing MORE OF ITSELF instead of moving on.
 *    The mandatory Corporate network gate does exactly this: the first Next
 *    swaps its "Host proxy" tab for "Egress redirection" and stays on step 2.
 *
 * So the honest primitive is "press Next until the step counter changes",
 * not "press Next once". Both behaviours fall out of that without the driver
 * hardcoding which step is which.
 */
async function advance(text?: string): Promise<void> {
  const before = await stepIndex();
  for (let attempt = 0; attempt < 3; attempt++) {
    const next = nextButton();
    const caption = attempt === 0 ? text : undefined;
    if ((await next.count()) === 0) {
      // Blocked with a remedy: run it, then Next reappears.
      await act(page, page.locator("footer").getByRole("button").last(), caption);
    } else {
      await expect(next).toBeEnabled({ timeout: 120_000 });
      await act(page, next, caption);
    }
    try {
      await expect.poll(stepIndex, { timeout: 8_000 }).not.toBe(before);
      return;
    } catch {
      /* same step still — it revealed more of itself; press on */
    }
  }
  throw new Error(`funnel stuck on step ${before} after 3 attempts at Next`);
}

/**
 * Delete any workspace already named WORKSPACE_NAME, via the API.
 *
 * Setup, not choreography — it runs before Act 4 opens the dialog so the act
 * always films a real creation. Mirrors the way the docs-screenshot spec
 * re-stages its data out of band before capturing.
 */
async function clearWorkspace(): Promise<void> {
  const headers = process.env.WARDYN_DEMO_TOKEN
    ? { Authorization: `Bearer ${process.env.WARDYN_DEMO_TOKEN}` }
    : undefined;
  const res = await page.request.get("/api/v1/workspaces", { headers }).catch(() => null);
  if (!res?.ok()) return;
  const body = await res.json().catch(() => null);
  const items: { id?: string; name?: string }[] = Array.isArray(body)
    ? body
    : (body?.items ?? body?.workspaces ?? []);
  for (const w of items) {
    if (w?.id && w.name === WORKSPACE_NAME) {
      await page.request.delete(`/api/v1/workspaces/${w.id}`, { headers }).catch(() => {});
    }
  }
}

// NOTE: there is deliberately no per-demo scope here. `demo-card-<id>` exists
// only on the /demos catalog (demo-screen.tsx's DemoCard, which stacks all six
// on one page); the funnel step renders the shared DemoRunControls BARE
// (demos-step.tsx), one demo per step. So on this path the page IS the scope,
// and scoping to a card that never renders is how Act 3 fails. `demo-start-<id>`
// does live inside DemoRunControls, so starting is still addressed per demo.

// The scope-menu button labels this file actually needs — see
// ui/src/app/components/wardyn/copy.ts's APPROVAL_SCOPE_LABEL. "run" needs no
// entry: it's the split button's plain click, never the caret. "until" stays
// a visible-only menu option (a demo that makes the viewer wait out a clock
// is a bad demo), so it never appears here either. Every scoped decide() call
// in this file is an Approve, so this is Approve-flavored only — widen it (and
// the label lookup in decide() below) the day a Deny needs a non-"run" scope.
const SCOPE_MENU_LABEL: Record<"once" | "always", string> = { once: "Once", always: "Always" };

/**
 * Approve (or deny) the first pending egress approval on screen.
 *
 * `scope` is a demo card in Act 3 (the funnel renders one per step, so it has
 * to be narrowed) and the whole page in Act 5, where LiveApprovals sits inline
 * under the terminal and there is only one.
 *
 * `decisionScope` picks the split button's caret menu instead of its bare
 * click — see SCOPE_MENU_LABEL above for which scopes are wired.
 */
async function decide(
  scope: Page | Locator,
  choice: "Approve" | "Deny",
  text: string,
  host?: string,
  decisionScope: "run" | "once" | "always" = "run",
): Promise<void> {
  // ALWAYS decide a NAMED host, never "whatever is first in the queue".
  //
  // A real agent run raises approvals the demo never asked for: Claude Code
  // reaches for its telemetry endpoint (http-intake.logs.us5.datadoghq.com) and
  // under "Hold it for approval" that surfaces as a pending row — often BEFORE
  // the one the act is about. Taking .first() meant the driver approved a
  // telemetry host on camera while example.com sat pending and undecided. In a
  // governance demo, approving something you did not mean to approve is the
  // worst possible frame.
  const rows = scope.getByTestId("live-approval-row");
  const row = host ? rows.filter({ hasText: host }).first() : rows.first();
  await expect(row).toBeVisible({ timeout: APPROVAL_APPEARS });
  await caption(page, text);
  await beat(page, PACE.read);
  if (decisionScope === "run") {
    // The bare split-button click — today's default scope, unchanged.
    await act(page, row.getByRole("button", { name: choice }));
  } else {
    // A non-default scope lives behind the split button's caret, which opens
    // into a Radix portal — not a descendant of `row` in the DOM, so the
    // scope option itself is found on the page, not scoped to the row.
    // .first(): the row mounts TWO carets with this exact aria-label — Approve's
    // ScopeMenu and Deny's (live-approvals.tsx mounts ScopeMenu twice). A bare
    // match is a strict-mode violation that kills the take. DOM order is
    // Approve, Approve-caret, Deny, Deny-caret, so .first() is Approve's. The
    // repo's own suite already disambiguates this way (approvals.spec.ts).
    await act(page, row.getByRole("button", { name: "More options" }).first());
    // Scope the option to the OPEN MENU, not the page: the funnel rail renders
    // each step as a button whose accessible name starts with its label, so on
    // the "Once, or for good" step a page-wide /^Once/ matches the rail button
    // too — another strict-mode violation. Radix's DropdownMenuContent is
    // role="menu", and the options inside are plain buttons.
    await act(
      page,
      page.getByRole("menu").getByRole("button", { name: new RegExp(`^${SCOPE_MENU_LABEL[decisionScope]}`) }),
    );
  }
  if (choice === "Deny") {
    // Deny is irreversible for the session, so it confirms first.
    const confirm = page.getByRole("alertdialog");
    await expect(confirm).toBeVisible();
    await beat(page, PACE.read);
    await act(page, confirm.getByRole("button", { name: "Deny" }));
  }

  // PROVE THE DECISION LANDED. Without this, a rejected decide (a 400 from the
  // scope rules, say) leaves the row pending, the component toasts a failure —
  // and the driver narrates "Approved…" straight over it, then keeps going. A
  // green take with a visibly failed decision on camera is precisely the
  // failure class this project has already shipped three times.
  if (host) {
    await expect(rows.filter({ hasText: host })).toHaveCount(0, { timeout: 20_000 });
  }
}

// ---------------------------------------------------------------------------
// Act 1 — first light
// ---------------------------------------------------------------------------

test("act 1 — first light", async () => {
  test.setTimeout(180_000);
  await page.goto("/");
  await page.bringToFront();

  // Fail here rather than 15 minutes into a silent, caption-less take: every
  // narration call degrades to a no-op by design, so nothing downstream would
  // ever complain about an overlay that failed to install.
  await expect
    .poll(() => page.evaluate(() => typeof (window as unknown as Record<string, unknown>).__demo), { timeout: 15_000 })
    .toBe("object");

  // A fresh install has no runs and no dismissed tour, so firstRunLanding()
  // (setup-gate.ts) redirects / → /setup and the hero renders. That redirect is
  // the real first-light behaviour and worth filming — but it is conditional on
  // has_runs being false, and Act 2's corporate-network probe LAUNCHES A RUN to
  // prove the path. So the moment this driver has run once, `/` lands on /runs
  // instead. Fall through to /setup rather than making every iteration of the
  // driver require a full stack reset. (The hero itself only needs an unset
  // wardyn-onboarding-seen, which a fresh browser profile always gives us.)
  const hero = page.getByRole("heading", { name: "Run anything. Keep your keys.", level: 1 });
  if (!(await hero.isVisible().catch(() => false))) {
    await page.goto("/setup");
  }
  await expect(hero).toBeVisible({ timeout: 60_000 });

  await chapter(page, "Wardyn", "A governed sandbox, from nothing, in one command");
  await caption(page, "One command brought this up. No account, no cloud — it is all on this machine.");
  await beat(page, PACE.read);
  await caption(page, "Every run gets its own identity, its own barrier, and no resident credentials.");
  await beat(page, PACE.read);

  await act(page, page.getByRole("button", { name: /^Get started/ }), "Ten steps. Only two are gates — the barrier, and proving the network.");
  await expect(page.getByText(/^Step 1 of/)).toBeVisible({ timeout: 30_000 });
});

// ---------------------------------------------------------------------------
// Act 2 — essentials
// ---------------------------------------------------------------------------

test("act 2 — barrier, network, model", async () => {
  test.setTimeout(300_000);
  await chapter(page, "Essentials", "The barrier, the network path, the model");

  await expect(page.getByRole("heading", { name: "Pick your barrier", level: 2 })).toBeVisible({ timeout: 30_000 });
  await caption(page, "Fence, Wall, Vault — three strengths of isolation. Wardyn detects which this host can actually do.");
  await beat(page, PACE.read + 900);
  await advance("Taking the tier this machine reports as ready.");

  await expect(page.getByRole("heading", { name: "Corporate network", level: 2 })).toBeVisible({ timeout: 30_000 });
  await caption(page, "Nothing downstream can be trusted until a sandbox can actually reach the network — so this step proves it.");
  await beat(page, PACE.read);
  const testConn = page.getByRole("button", { name: /Test connectivity/i });
  if (await testConn.isVisible().catch(() => false)) {
    await act(page, testConn, "The probe runs from inside a sandbox. Only a passing probe unlocks Next.");
    await beat(page, PACE.read);
  }
  await advance();

  await expect(page.getByRole("heading", { name: "Connect your model", level: 2 })).toBeVisible({ timeout: 30_000 });
  await caption(page, "The model was connected from the terminal a moment ago — the token is encrypted at rest and injected by the proxy.");
  await beat(page, PACE.read + 900);
  await caption(page, "It never enters the sandbox. The agent will reach Anthropic without ever holding the credential.");
  await beat(page, PACE.read);
  await advance();
});

// ---------------------------------------------------------------------------
// Act 3 — the guardrails (funnel steps 4-8)
// ---------------------------------------------------------------------------

test("act 3 — the five guardrail demos", async () => {
  test.setTimeout(1_500_000);
  await chapter(page, "The guardrails", "Five sandboxes, five ways the boundary holds");

  for (const demo of FUNNEL_DEMOS) {
    await expect(page.getByRole("heading", { name: demo.label, level: 2 })).toBeVisible({ timeout: 60_000 });
    await caption(page, `${demo.label} — ${demo.caption}`);
    await beat(page, PACE.read + 700);

    await act(page, page.getByTestId(`demo-start-${demo.id}`), "Starting a throwaway sandbox under exactly that policy.");
    await expect(page.locator(".xterm-screen").first()).toBeVisible({ timeout: SANDBOX_UP });
    await beat(page, PACE.read);

    for (const [i, cmd] of demo.cmds.entries()) {
      // fail-then-approve and once-or-for-good both run the same command
      // twice: the first is refused and raises the approval, the retry after
      // approval is what gets through — the SCOPE granted is what differs.
      if (demo.approve && i === 1) {
        await decide(
          page,
          "Approve",
          "The refusal raised an approval. Granting it — then the very same command again.",
          "example.com",
          demo.scope,
        );
        await caption(
          page,
          demo.scope === "once"
            ? "A plain Approve would keep it allowed for the rest of this run — Once covers only this one connection."
            : "A plain Approve keeps it allowed for the rest of this run (the split button's caret offers other options).",
        );
      }
      await typeInTerminal(page, cmd);
      // Let the command actually RESOLVE before moving on. This is not pacing:
      // ending a demo while a curl is still in flight kills the sandbox before
      // its decision reaches the audit log, and the proof silently goes missing
      // from a take that looks perfectly fine. Derive the wait from the
      // command's own --max-time. Demos that involve an approval are excluded —
      // their curl is SUPPOSED to still be hanging when we decide it.
      const maxTime = demo.approve ? null : cmd.match(/--max-time (\d+)/);
      await beat(page, maxTime ? (Number(maxTime[1]) + 2) * 1000 : PACE.read + 800);

      // The retry after an approval MUST visibly succeed. This is the payoff of
      // both approve-demos, and without the assertion a failed retry just
      // raises a fresh approval that the NEXT step latches onto — so the take
      // stays green while narrating "approved, and it goes through" over a
      // terminal showing two refusals and no success. (The proxy itself warns a
      // `once` grant is spent before success is guaranteed.)
      if (demo.approve && i === 1) {
        await expect(page.locator(".xterm-screen").first()).toContainText(/HTTP\/2 200|HTTP\/1\.1 200/, {
          timeout: 45_000,
        });
      }
    }

    if (demo.id === "once-or-for-good") {
      // Once really does mean once: the grant the retry above just spent is
      // gone, so the SAME command run a third time is refused all over again
      // and raises a brand-new approval — the entire point of this demo.
      // Deliberately left undecided: proving the re-raise appeared is the
      // beat, not deciding it a second time.
      await typeInTerminal(page, demo.cmds[0]);
      await caption(
        page,
        "Same command, one more time. Once already spent itself on the last connection — refused again, and a fresh approval appears.",
      );
      await expect(page.getByTestId("live-approval-row").filter({ hasText: "example.com" })).toBeVisible({
        timeout: APPROVAL_APPEARS,
      });
      await beat(page, PACE.read + 900);
    }

    if (demo.approve && demo.cmds.length === 1) {
      // held-at-the-door: the curl is still hanging at the proxy right now.
      await decide(page, "Approve", "The command has not failed — it is hanging, held open at the proxy, waiting for a human.", "example.com");
      await caption(page, "Approved inside the window, so that same in-flight request completes. No retry.");
      await beat(page, PACE.read + 900);

      // ...and the other half of a live decision: a host you refuse.
      await typeInTerminal(page, "curl -sSI --max-time 60 https://wikipedia.org");
      await beat(page, 1200);
      await decide(page, "Deny", "A second host, held the same way — this one gets refused.", "wikipedia.org");
      await beat(page, PACE.read);
    }

    if (demo.id === "lines-that-cant-be-crossed") {
      // Prove the block from the TERMINAL, not the audit panel.
      //
      // The demo card's own copy says you'll see "a deny row in the Audit
      // panel" for these two. You will not, and it is not a bug in the block —
      // it is the block being stronger than advertised. https://example.com
      // goes out via CONNECT through the egress proxy, so the proxy decides it
      // and logs an allow. A direct-to-IP http:// request has no proxy in its
      // path and the sandbox carries NO DEFAULT ROUTE, so it dies at the
      // network layer in ~0ms — curl (7), never reaching the proxy that would
      // have recorded a decision. Asserting the audit row here would be
      // asserting the app's overclaim.
      await expect(page.locator(".xterm-screen").first()).toContainText(/Failed to connect to 169\.254\.169\.254/, {
        timeout: 60_000,
      });
      await caption(page, "Egress was wide open — and these two never even got a connection. There is no route off the box for them.");
      await beat(page, PACE.read + 1200);
    }

    // The decisions are on the record before we move on. Deliberately skipped
    // for lines-that-cant-be-crossed: its two headline denials never reach the
    // proxy (see above), so its panel shows only the allow — and narrating
    // "every one of those decisions is on the record" over that would be a
    // claim the trail does not support.
    const auditPanel = page.getByTestId("demo-audit-panel");
    if (demo.id !== "lines-that-cant-be-crossed" && (await auditPanel.isVisible().catch(() => false))) {
      await spotlight(page, auditPanel);
      await caption(page, "Every one of those decisions landed in the audit trail, live, as it happened.");
      await beat(page, PACE.read);
      await spotlight(page, null);
    }

    const endDemo = page.getByRole("button", { name: "End demo" });
    if (await endDemo.isVisible().catch(() => false)) await act(page, endDemo);
    await advance();
  }
});

// ---------------------------------------------------------------------------
// Act 4 — your work
// ---------------------------------------------------------------------------

test("act 4 — onboard a workspace", async () => {
  test.setTimeout(300_000);
  await chapter(page, "Your work", "Point it at something real");

  await expect(page.getByRole("heading", { name: "Onboard a workspace", level: 2 })).toBeVisible({ timeout: 60_000 });

  // Onboarding the SAME name twice is rejected and silently leaves the dialog
  // open, so a second run of the driver against a stack that already has this
  // workspace would fail here. Clear it first, off camera, so the act always
  // films an actual creation. (A full `make record-demo` resets the stack and
  // never needs this; iterating with --no-reset does.)
  await clearWorkspace();
  await caption(page, "A workspace is a repo or a directory a run is allowed to attach. Nothing is mounted that you did not onboard.");
  await beat(page, PACE.read + 600);

  // The step's trigger is state-dependent (step-bodies.tsx): an empty install
  // says "Onboard your first workspace", and only once one exists does it
  // become "Add workspace". The recording always runs against an empty one, but
  // matching both keeps an --no-reset iteration working.
  await act(page, page.getByRole("button", { name: /Onboard your first workspace|Add workspace/ }).first());
  const dlg = page.getByRole("dialog");
  // By ROLE, not text: "Add workspace" is both the dialog title and its submit
  // button, so a bare getByText is a strict-mode violation.
  await expect(dlg.getByRole("heading", { name: "Add workspace" })).toBeVisible();

  // Source/image cards are OptionCards — aria-pressed buttons, not radios
  // (form-primitives.tsx). Their accessible name carries the hint text too, so
  // match on a prefix rather than exactly.
  await act(page, dlg.getByRole("button", { name: /Local directory/ }), "A directory on this machine — no git host needed.");
  await spotlight(page, dlg.getByLabel("Path on this host"));
  await dlg.getByLabel("Path on this host").fill(WORKSPACE_PATH);
  await dlg.getByLabel("Name", { exact: true }).fill(WORKSPACE_NAME);
  await spotlight(page, null);
  await beat(page, PACE.read);

  const advanced = dlg.getByText("Advanced", { exact: true });
  if (await advanced.isVisible().catch(() => false)) {
    await act(page, advanced, "Under Advanced: where it lands inside the sandbox, and whether the agent may write to it.");
    await beat(page, PACE.read + 600);
  }

  // LOAD-BEARING: `writable` defaults to FALSE (add-workspace-dialog.tsx), so
  // without this the directory mounts READ-ONLY and the agent's edits never
  // reach the host — Act 5 runs, reports success, and leaves a workspace with
  // no diff in it. Granting write access on camera is the honest beat anyway:
  // a run that edits your code should have to be given permission to.
  const writable = dlg.getByRole("checkbox", { name: /Allow writes/ });
  if (await writable.isVisible().catch(() => false)) {
    await act(page, writable, "And this run may write to it — off by default, granted deliberately.");
    await beat(page, PACE.read);
  }

  await act(page, dlg.getByRole("button", { name: "Add workspace" }), "One call. No scan, no build — it is usable immediately.");
  await expect(dlg).toBeHidden({ timeout: 60_000 });
  await beat(page, PACE.read);

  await advance();
  await expect(page.getByRole("heading", { name: "Review readiness", level: 2 })).toBeVisible({ timeout: 60_000 });
  await caption(page, "What is blocking, what is worth a look, what is ready — checked against this host, not a checklist.");
  await beat(page, PACE.read + 900);

  await act(page, page.getByRole("button", { name: "Finish setup" }));
  await expect(page.getByRole("link", { name: /^Runs/ })).toBeVisible({ timeout: 60_000 });
});

// ---------------------------------------------------------------------------
// Act 5 — a real governed run
// ---------------------------------------------------------------------------

test("act 5 — a real run", async () => {
  test.setTimeout(2_700_000);
  await chapter(page, "A real run", "Actual work, under the same boundary");

  await act(page, page.getByRole("button", { name: "New run" }), "Now a real agent, on that workspace, doing real work.");
  await expect(page).toHaveURL(/\/runs\/new/, { timeout: 30_000 });

  await caption(page, "One page. Everything the run is allowed to do is spelled out on the right as you build it.");
  await beat(page, PACE.read + 700);

  // Name it first. The title is required (Launch stays disabled without one)
  // and it is the key the Runs board groups by, so it is also what this run
  // will be called everywhere it appears later in the film.
  const titleBox = page.getByLabel("Title");
  await spotlight(page, titleBox);
  await titleBox.fill(DEMO_TITLE);
  await spotlight(page, null);
  await caption(page, "Every run gets a name. Runs that share one are the same piece of work.");
  await beat(page, PACE.read);

  // What to run. SHELL_ACT5 records the model-free variant: same workspace, same
  // two hosts, same governance beats — a Shell command instead of an agent.
  // Wardyn's whole claim is that it governs ANY workload, so this is a true
  // demo, not a degraded one; it just does not show a coding agent writing code.
  if (SHELL_ACT5) {
    await act(
      page,
      page.getByRole("radio", { name: "Shell command" }),
      "A run does not have to be an agent — Wardyn governs any workload. This one is a plain shell command.",
    );
    const cmdBox = page.getByLabel("Command");
    await spotlight(page, cmdBox);
    await cmdBox.fill(DEMO_COMMAND);
    await spotlight(page, null);
    await caption(page, "Run the workspace's tests, then reach two hosts: one that stops and asks me, one nothing can open.");
    await beat(page, PACE.read + 1400);
  } else {
  await act(page, page.getByRole("radio", { name: "Agent task" }));

  // ORDER IS LOAD-BEARING, twice over.
  //
  // (1) initialWizardState() defaults to mode "interactive" (wizard-types.ts),
  // which launches an IDLE sandbox waiting for a human to type. The agent never
  // executes the task, so nothing reaches for example.com and the held approval
  // this act is built around can never appear. Batch is what makes it an
  // unattended agent run.
  //
  // (2) Batch must be clicked BEFORE the task is filled: an interactive run has
  // no Task field at all now (the server ignores task for one), so the box does
  // not exist until this click.
  await act(
    page,
    page.getByRole("radio", { name: /^Batch/ }),
    "Unattended: the agent does the work on its own, and I only step in when the policy stops it.",
  );

  const taskBox = page.getByLabel("Task");
  await spotlight(page, taskBox);
  await taskBox.fill(DEMO_TASK);
  await spotlight(page, null);
  await caption(page, "Add a function and a test — then reach two hosts: one that stops and asks me, one nothing can open.");
  await beat(page, PACE.read + 1400);
  }

  // Workspace. Selected by its VISIBLE TEXT, not its accessible name: this
  // trigger has no accessible name at all (the Agent select beside it is
  // labelled "Agent" via Field/Label htmlFor; this one was never wired up), so
  // getByRole("combobox", {name}) can never match it. Filtering on the
  // placeholder text is the honest workaround until the control gets a label —
  // an unnamed combobox is a real a11y gap, not just a test inconvenience.
  await act(
    page,
    page.getByRole("combobox").filter({ hasText: "Ephemeral scratch" }),
    "Attach the workspace we just onboarded.",
  );
  await act(page, page.getByRole("option", { name: new RegExp(WORKSPACE_NAME, "i") }).first());

  // Confinement — these are REAL radios whose accessible name is title + hint
  // ("Confined Default-deny. New hosts are held at the door…"), so prefix-match
  // the title. Note this is a DIFFERENT component from the Add-workspace
  // dialog's OptionCard, which is an aria-pressed <button>. Same look, two
  // roles: check the a11y snapshot rather than assuming.
  await act(page, page.getByRole("radio", { name: /^Confined/ }), "Confined: default-deny egress, and only what we list gets through.");

  // Network — the load-bearing part of the whole run.
  await act(page, page.getByRole("button", { name: /Edit hosts/ }), "This is the part that matters.");
  const net = page.getByRole("dialog");
  await expect(net.getByText("Network for this run")).toBeVisible();
  await beat(page, PACE.read);

  // Host chips are aria-pressed TOGGLES, and the wizard SEEDS this one on
  // (initialWizardState's allowedDomains). Clicking it therefore REMOVES it —
  // on camera the ✓ vanishes and the header ticks to "0 hosts" while the
  // narration says "allow it". Point at it when it is already on; only click
  // when it is genuinely off.
  const modelChip = net.getByRole("button", { name: new RegExp(MODEL_HOST.replace(/\./g, "\\.")) }).first();
  if ((await modelChip.getAttribute("aria-pressed")) === "true") {
    await caption(page, `${MODEL_HOST} is already on the list — without it the agent has no model at all.`);
    await spotlight(page, modelChip);
    await beat(page, PACE.read);
    await spotlight(page, null);
  } else {
    await act(page, modelChip, `Allow ${MODEL_HOST} and nothing else — without it the agent has no model at all.`);
  }
  // The unlisted-host rules ARE radios, but each card's accessible name
  // includes its explanatory body copy — prefix-match the title.
  await act(
    page,
    net.getByRole("radio", { name: /^Hold it for approval/ }),
    "And for any host that is not on the list: stop, and ask me — nothing off this list goes out unseen.",
  );
  await beat(page, PACE.read);
  await act(page, net.getByRole("button", { name: "Save hosts" }));
  await expect(net).toBeHidden({ timeout: 30_000 });

  await caption(page, "That is the entire blast radius of this run, declared before it starts.");
  await beat(page, PACE.read + 800);
  await act(page, page.getByRole("button", { name: "Launch run" }));
  await expect(page).toHaveURL(/\/runs\/[0-9a-f-]{8,}/i, { timeout: 60_000 });
  // Act 6 recaps THIS run's audit/recording — captured now, before the
  // `always` proof detour below navigates away from it.
  const mainRunUrl = page.url();

  // This run is BATCH, so run-detail renders the static "Output" notice, NOT an
  // attachable terminal — claiming a live terminal here would narrate over a
  // motionless card. Say what is actually on screen.
  await caption(page, "No terminal to type into — this one is unattended. Wardyn records the whole session instead.");
  await beat(page, PACE.read + 1200);
  await caption(
    page,
    SHELL_ACT5
      ? "Right now, inside the box: it is running the workspace's tests, then reaching for those two hosts."
      : "Right now, inside the box: it is adding the function, adding the test, running it.",
  );
  await beat(page, PACE.read + 1600);

  // The held request surfaces under the very output that caused it. Always,
  // not a plain Approve: this is the beat that teaches the permanent scope —
  // see the workspace + proof-run detour below, once the run finishes.
  await decide(
    page,
    "Approve",
    `${SHELL_ACT5 ? "The run" : "The agent"} just reached for ${HELD_HOST}. It is not on the list — so it is stopped at the proxy, right now, waiting on me.`,
    HELD_HOST,
    "always",
  );
  await caption(
    page,
    "Approved — and with Always, not just This run. It's saved to the workspace itself, so it outlives this run too. The run gets through.",
  );
  await beat(page, PACE.read + 1200);

  // Name the OTHER pending row rather than leaving it unexplained on screen for
  // the rest of the act. Claude Code calls its own telemetry endpoint from
  // inside the sandbox; the policy stops it and it sits there un-decided. That
  // is the most persuasive unscripted moment in the whole film — an agent
  // caught reaching somewhere nobody asked it to, by a boundary that was not
  // written with it in mind — and silence over it just reads as an unexplained
  // warning.
  const telemetryRow = page.getByTestId("live-approval-row").filter({ hasText: "datadoghq.com" });
  if (await telemetryRow.count()) {
    await spotlight(page, telemetryRow.first());
    await caption(
      page,
      "And that other row is the agent's own telemetry endpoint, reaching out on its own. Nobody asked for it, it is not on the list — so it stays refused until I say otherwise.",
    );
    await beat(page, PACE.read + 900);
    await spotlight(page, null);
  }

  await caption(page, "Next it probes the cloud-metadata address — where cloud credentials live. There is no route off the box for it at all.");
  await beat(page, PACE.read + 1400);
  await caption(page, "No approval was raised for it, because no approval could have granted it.");
  await beat(page, PACE.read + 900);

  // Terminal state, whichever it is — a denial must not brick the run.
  // RunStateBadge renders TITLE CASE labels from runStateMeta ("Completed",
  // not "COMPLETED"); matching the wire enum here would wait out the full
  // timeout and never see it.
  const finished = page.getByText(/^(Completed|Failed|Stopped|Killed)$/).first();
  await expect(finished).toBeVisible({ timeout: RUN_FINISHES });
  await beat(page, PACE.read);

  // -------------------------------------------------------------------------
  // The `always` beat's second half: the workspace's own permanent list, and
  // a brand-new run that starts with the held host already open — no hold,
  // no approval, nothing to click. "Approved" is a claim; this is the receipt.
  // -------------------------------------------------------------------------
  await caption(page, "That approval was Always, not just This run — it's saved to the workspace itself. Here's the receipt.");
  await beat(page, PACE.read + 900);

  await act(page, page.getByRole("link", { name: "Workspaces" }));
  await act(page, page.getByRole("row", { name: new RegExp(WORKSPACE_NAME, "i") }));
  await expect(page.getByRole("heading", { name: WORKSPACE_NAME, level: 1 })).toBeVisible({ timeout: 30_000 });

  const grantedHost = page.getByText(HELD_HOST, { exact: true }).first();
  await spotlight(page, grantedHost);
  await caption(
    page,
    `${HELD_HOST} is on the workspace's own permanent list now — every future run of "${WORKSPACE_NAME}" starts with it already allowed.`,
  );
  await beat(page, PACE.read + 1200);
  await spotlight(page, null);

  await act(page, page.getByRole("button", { name: "New run" }), "One more run, same workspace — watch what does NOT happen this time.");
  await expect(page).toHaveURL(/\/runs\/new/, { timeout: 30_000 });

  const proofTitleBox = page.getByLabel("Title");
  await spotlight(page, proofTitleBox);
  await proofTitleBox.fill(PROOF_RUN_TITLE);
  await spotlight(page, null);

  // Interactive is the wizard's own default (unlike the batch run above, this
  // one is left alone) — a bare shell is all this needs, not the agent CLI.
  await act(
    page,
    page.getByRole("radio", { name: /^Terminal/ }),
    "A bare shell this time. No agent, no task — just prove the point.",
  );
  await act(
    page,
    page.getByRole("combobox").filter({ hasText: "Ephemeral scratch" }),
    "The same workspace again.",
  );
  await act(page, page.getByRole("option", { name: new RegExp(WORKSPACE_NAME, "i") }).first());
  // Confined stays the default; Network stays untouched — that omission IS
  // the point: nothing to configure, the workspace already vouches for it.
  await act(page, page.getByRole("button", { name: "Launch run" }));
  await expect(page).toHaveURL(/\/runs\/[0-9a-f-]{8,}/i, { timeout: 60_000 });

  await expect(page.locator(".xterm-screen").first()).toBeVisible({ timeout: SANDBOX_UP });
  // .xterm-screen renders when AttachTerminal MOUNTS, before the PTY websocket
  // is up — typing here eats the first characters and the shell reports
  // "command not found" on camera. Act 3 survives only because it always beats
  // between this expect and its first command; the proof run had no such gap.
  await beat(page, PACE.read);
  await typeInTerminal(page, `curl -sSI https://${HELD_HOST}`);
  await beat(page, PACE.read + 800);
  await caption(page, "No hold, no prompt, nothing to click — the workspace already vouches for this host.");
  // The idle hint, not a pending row: proof nothing was raised to decide.
  await expect(page.getByTestId("live-approvals-idle")).toBeVisible();
  await beat(page, PACE.read + 900);

  // Back to the main run: Act 6 recaps ITS audit trail and recording, not
  // this proof run's.
  await page.goto(mainRunUrl);
  await expect(page.getByRole("tab", { name: /Audit/ })).toBeVisible({ timeout: 30_000 });
});

// ---------------------------------------------------------------------------
// Act 6 — the receipts
// ---------------------------------------------------------------------------

test("act 6 — the receipts", async () => {
  test.setTimeout(300_000);
  await chapter(page, "The receipts", "Everything above, on the record");

  await act(page, page.getByRole("tab", { name: /Audit/ }), "Every decision, attributed: the human, the agent, the system.");
  await beat(page, PACE.read + 1600);
  await caption(page, "The allow, the request that stopped and waited, my approval — all of it, append-only. The metadata probe is not even here: it never got a connection to log.");
  await beat(page, PACE.read + 1400);

  await act(page, page.getByRole("tab", { name: /Recording/ }), "And the session itself was recorded, so it can be replayed.");
  await beat(page, PACE.read + 1600);

  await caption(page, "Run anything. Keep your keys.");
  await beat(page, PACE.chapter);
  await caption(page, "");
});
