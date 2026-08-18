/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

/*
 * The recording rig: one browser, one context, one page, for one take.
 *
 * Lifted out of walkthrough.spec.ts unchanged, because the 0.5 series is ten
 * separate videos and every one of them needs exactly this and nothing else of
 * the walkthrough. Importing this module REGISTERS its beforeAll/afterAll on
 * the importing spec file — that is the whole interface: a spec gets the rig by
 * importing it, and reads the page out of stage() inside each test body.
 *
 * A spec that imports this keeps its OWN `test.skip(!process.env.WARDYN_DEMO,
 * …)` guard. Without it, a bare `pnpm exec playwright test --project=demo`
 * points a headed browser at a developer's live stack and starts clicking
 * Launch — see the header of walkthrough.spec.ts.
 */

import path from "node:path";
import { fileURLToPath } from "node:url";
import { test, chromium, type Browser, type BrowserContext, type Page } from "@playwright/test";
import { caption, installOverlay } from "./overlay";

// One page for the whole recording — a per-test page would flash a new window
// between acts. describe.serial + a shared page is the standard shape for a
// walkthrough whose steps genuinely depend on each other.
let browser: Browser | undefined;
let context: BrowserContext | undefined;
let page: Page;

// Where the browser's own recording lands. Resolved from this file so it does
// not depend on the process cwd; scripts/record-demo.sh joins console.webm onto
// the terminal segment to make the final video.
// WARDYN_DEMO_WORK_DIR lets record-demo.sh give each --video its OWN scratch
// directory. Without it every take writes console.webm and narration.json to
// one shared path, so two takes running at once silently overwrite each other's
// picture and narration — and the loser still exits 0. The default is the
// original shared path, so a bare `playwright test --project=demo` is unchanged.
const VIDEO_DIR =
  process.env.WARDYN_DEMO_WORK_DIR ||
  path.resolve(path.dirname(fileURLToPath(import.meta.url)), "../../test-results/demo-video");
const VIDEO_OUT = path.join(VIDEO_DIR, "console.webm");

/**
 * The page every act drives.
 *
 * Only valid from beforeAll onwards, so call it INSIDE a test body — at module
 * scope it hands back an unassigned binding.
 */
export function stage(): Page {
  return page;
}

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
    // Headless (the demo project's default now): there is no OS window to
    // place, getWindowForTarget either errors or answers with virtual bounds.
    // Either way placement is meaningless — the recording is the viewport by
    // construction — so bail rather than fail a take over a window that does
    // not exist. A headed run (DEMO_CDP with a real browser) still gets the
    // full check below.
    const win = (await session.send("Browser.getWindowForTarget").catch(() => null)) as {
      windowId: number;
    } | null;
    if (!win) return;
    const { windowId } = win;
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
