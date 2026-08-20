/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

/*
 * The demo recording's presentation layer.
 *
 * Playwright dispatches synthetic input, so the operating system's mouse
 * pointer never moves — a screen capture would show buttons activating with
 * nothing pointing at them. This injects its own: a ring that lands on whatever
 * the driver is about to click, a caption bar that narrates, and chapter cards
 * between acts.
 *
 * Everything here is `pointer-events: none` and lives in its own fixed layer,
 * so it can never intercept a click the driver is trying to make, and it never
 * participates in layout. It is installed via addInitScript so it survives a
 * full document load (the console is a SPA, but /setup → /runs after "Finish
 * setup" is a real navigation in some flows).
 */

import { mkdirSync, writeFileSync } from "node:fs";
import path from "node:path";
import type { Locator, Page } from "@playwright/test";
import { NARRATION_DIR, narrationZero, narrationZeroMs, speak } from "./narrator";

const MOUNT = "__wardyn_demo_overlay";

// Timings. Deliberately generous — this is footage a human watches, not a test
// racing to green. `beat()` is the only sleep the spec is allowed to use for
// pacing; waiting for the APP is always an expect/waitFor, never a sleep.
export const PACE = {
  ring: 550, // ring settles on the target before the click
  afterClick: 700, // let the UI respond before the next caption
  read: 2200, // long enough to read a caption
  chapter: 2600, // chapter card dwell
};

/** Install the overlay on every document this page loads. Call once, first. */
export async function installOverlay(page: Page): Promise<void> {
  narrationZero();
  await page.addInitScript((mountId: string) => {
    const boot = () => {
      if (document.getElementById(mountId)) return;
      const style = document.createElement("style");
      style.textContent = `
        #${mountId}, #${mountId} * { pointer-events: none !important; }
        #${mountId} {
          position: fixed; inset: 0; z-index: 2147483000;
          font: 500 20px/1.4 ui-sans-serif, system-ui, sans-serif;
        }
        #${mountId} .ring {
          position: fixed; border-radius: 12px; opacity: 0;
          border: 3px solid #f59e0b;
          box-shadow: 0 0 0 3px rgba(245,158,11,.22), 0 0 22px 4px rgba(245,158,11,.45);
          transition: all .42s cubic-bezier(.22,.61,.36,1);
        }
        #${mountId} .cap {
          position: fixed; left: 50%; bottom: 44px; transform: translateX(-50%) translateY(10px);
          max-width: 74vw; padding: 14px 26px; border-radius: 999px;
          background: rgba(11,13,18,.93); color: #f4f4f5;
          border: 1px solid rgba(255,255,255,.13);
          box-shadow: 0 10px 40px rgba(0,0,0,.5);
          opacity: 0; transition: opacity .3s ease, transform .3s ease;
          text-align: center; white-space: pre-wrap;
        }
        #${mountId} .cap.on { opacity: 1; transform: translateX(-50%) translateY(0); }
        #${mountId} .chap {
          position: fixed; inset: 0; display: flex; flex-direction: column;
          align-items: center; justify-content: center; gap: 14px;
          background: rgba(9,11,15,.93); opacity: 0; transition: opacity .45s ease;
        }
        #${mountId} .chap.on { opacity: 1; }
        #${mountId} .chap b { font-size: 54px; font-weight: 650; color: #fafafa; letter-spacing: -.02em; }
        #${mountId} .chap span { font-size: 22px; color: #a1a1aa; }
      `;
      document.head.appendChild(style);

      const root = document.createElement("div");
      root.id = mountId;
      root.innerHTML = `<div class="ring"></div><div class="cap"></div><div class="chap"><b></b><span></span></div>`;
      document.body.appendChild(root);

      const q = (s: string) => root.querySelector(s) as HTMLElement;
      // Exposed to the driver via page.evaluate. Kept on window because
      // addInitScript state does not survive into the page's own scope
      // otherwise.
      (window as unknown as Record<string, unknown>).__demo = {
        caption(text: string) {
          const c = q(".cap");
          c.textContent = text;
          c.classList.toggle("on", !!text);
        },
        ring(box: { x: number; y: number; width: number; height: number } | null) {
          const r = q(".ring");
          if (!box) {
            r.style.opacity = "0";
            return;
          }
          // Appearing from hidden: JUMP to the target and only fade in. The
          // default all-property transition would fly the invisible ring in
          // from wherever the last one died — sampled mid-flight it straddles
          // two unrelated elements and reads as pointing at both (persona
          // round 1 caught it twice). Moves between two VISIBLE targets keep
          // the flight; that motion is the "look here now" cue.
          const hidden = r.style.opacity !== "1";
          if (hidden) r.style.transition = "opacity .42s ease";
          const pad = 6;
          r.style.left = `${box.x - pad}px`;
          r.style.top = `${box.y - pad}px`;
          r.style.width = `${box.width + pad * 2}px`;
          r.style.height = `${box.height + pad * 2}px`;
          r.style.opacity = "1";
          if (hidden) {
            requestAnimationFrame(() => {
              r.style.transition = "";
            });
          }
        },
        chapter(title: string, sub: string) {
          const c = q(".chap");
          (c.querySelector("b") as HTMLElement).textContent = title;
          (c.querySelector("span") as HTMLElement).textContent = sub;
          c.classList.toggle("on", !!title);
        },
      };
    };
    if (document.body) boot();
    else document.addEventListener("DOMContentLoaded", boot);
  }, MOUNT);
}

// The overlay only exists after installOverlay + a navigation. Every call is
// wrapped so a page that somehow lacks it degrades to a no-op rather than
// failing the recording over a caption.
async function call(page: Page, fn: string, ...args: unknown[]): Promise<void> {
  await page
    .evaluate(
      ([name, a]) => {
        const d = (window as unknown as Record<string, Record<string, (...x: unknown[]) => void>>).__demo;
        d?.[name as string]?.(...(a as unknown[]));
      },
      [fn, args] as [string, unknown[]],
    )
    .catch(() => {
      /* overlay absent — narration is cosmetic, never fatal */
    });
}

// Wall-clock instant the current spoken line finishes. beat() and act() wait it
// out, which is what makes the hold `max(existing beat, audio)` without any
// caller — or walkthrough.spec.ts — having to know narration exists.
let speechUntil = 0;

/** Hold until the current line has finished speaking. No-op when silent. */
async function awaitSpeech(page: Page): Promise<void> {
  const remaining = speechUntil - Date.now();
  speechUntil = 0;
  if (remaining > 0) await page.waitForTimeout(remaining);
}

/** Show (or with no text, hide) the narration bar. */
export async function caption(page: Page, text: string): Promise<void> {
  await call(page, "caption", text);
  // +300ms so the next visual does not start on the last syllable.
  const durMs = await speak(text);
  if (durMs > 0) speechUntil = Date.now() + durMs + 300;
}

/*
 * Fast-forward: mark a stretch the viewer should not have to sit through.
 *
 * Nothing is compressed here — the browser records in real time and cannot do
 * otherwise. ffwdStart/ffwdEnd just record the span, and scripts/demo-ffwd.py
 * squeezes those seconds out of the finished mp4 afterwards (and slides every
 * later narration cue up by exactly the time it removed).
 *
 * TIME BASIS: the same zero narration.json's cues use, because demo-ffwd.py has
 * to re-time both against one clock. That zero is set by installOverlay, a few
 * tens of ms after the context — and so recordVideo — starts, so both the cues
 * and these spans sit that same constant behind true video time. The narration
 * mux has always assumed that skew away; a span held for minutes cares even
 * less than a caption held for seconds.
 *
 * SILENT ON PURPOSE: the chip goes on screen through the raw caption DOM call,
 * never through caption(), because caption() speaks. A line spoken inside a
 * span is a line played over frames the encoder is about to throw away — the
 * voice would land 12x early and every cue after it would be wrong.
 */
const SPEEDUPS = path.join(NARRATION_DIR, "speedups.json");
const spans: Array<{ startMs: number; endMs: number }> = [];
let ffwdFrom = -1;

/** Begin a fast-forwarded span. Say nothing until ffwdEnd. */
export async function ffwdStart(page: Page): Promise<void> {
  narrationZero(); // a --silent take never calls speak(), so the clock may not be running yet
  ffwdFrom = Date.now() - narrationZeroMs();
  await call(page, "caption", "▸▸ fast-forward — the agent is working");
}

/** End the span and persist it beside narration.json for the encoder. */
export async function ffwdEnd(page: Page): Promise<void> {
  await call(page, "caption", "");
  if (ffwdFrom < 0) return; // ffwdEnd without a start — nothing to compress
  spans.push({ startMs: ffwdFrom, endMs: Date.now() - narrationZeroMs() });
  ffwdFrom = -1;
  try {
    // Rewritten after every span, for narrator.flush()'s reason: a take that
    // dies late should still leave the encoder the spans it did finish.
    mkdirSync(NARRATION_DIR, { recursive: true });
    writeFileSync(SPEEDUPS, JSON.stringify({ zero: narrationZeroMs(), spans }, null, 2));
  } catch {
    /* the take matters more than the fast-forward */
  }
}

/** Full-screen act divider. */
export async function chapter(page: Page, title: string, sub: string): Promise<void> {
  await call(page, "chapter", title, sub);
  // Spoken as one line so the card does not clear between its own two halves.
  const durMs = await speak(sub ? `${title}. ${sub}.` : title);
  await page.waitForTimeout(Math.max(PACE.chapter, durMs + 300));
  await call(page, "chapter", "", "");
  await page.waitForTimeout(400);
}

/** Park the ring on a target (or clear it). */
export async function spotlight(page: Page, target: Locator | null): Promise<void> {
  if (!target) {
    await call(page, "ring", null);
    return;
  }
  await target.scrollIntoViewIfNeeded().catch(() => {});
  const box = await target.boundingBox();
  await call(page, "ring", box);
  await page.waitForTimeout(PACE.ring);
}

/**
 * The driver's main verb: narrate, point, click. Anything the viewer should
 * see happen goes through here so the ring and the click never disagree.
 */
export async function act(page: Page, target: Locator, text?: string): Promise<void> {
  if (text) await caption(page, text);
  await spotlight(page, target);
  await target.click();
  await spotlight(page, null);
  await page.waitForTimeout(PACE.afterClick);
  // act()'s own pacing is ~1.7s; a narrated line is often longer, and cutting it
  // off mid-sentence is the whole failure this exists to avoid.
  await awaitSpeech(page);
}

/** A readable pause. Use for pacing only — never to wait on the app. */
export async function beat(page: Page, ms: number = PACE.read): Promise<void> {
  await page.waitForTimeout(ms);
  await awaitSpeech(page);
}

/**
 * Type a command into an attached xterm and press Enter.
 *
 * xterm.js takes keystrokes through an offscreen helper textarea, so the
 * reliable path is: click the rendered screen to focus it, then use the
 * keyboard. `scope` narrows to one terminal when a page holds several (the
 * demos funnel renders one per step).
 */
export async function typeInTerminal(page: Page, cmd: string, scope: Page | Locator = page): Promise<void> {
  const screen = scope.locator(".xterm-screen").first();
  await screen.waitFor({ state: "visible", timeout: 60_000 });
  await spotlight(page, screen);
  await screen.click();
  await spotlight(page, null);
  // Slowly enough to read on camera; xterm echoes each keystroke over the PTY.
  await page.keyboard.type(cmd, { delay: 45 });
  await beat(page, 500);
  await page.keyboard.press("Enter");
}
