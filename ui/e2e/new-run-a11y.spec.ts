/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// New Run, accessibility and layout. The repo carries no axe dependency, so this checks the
// four things a person feels, on every step of an agent task from Start to the Launch dialog, at
// a phone (390), a tablet (900) and a desktop (1440) width:
//   1. Keyboard: Tab from the step heading (where a step puts focus) reaches the step's own primary control without a
//      trap, and every stop shows a visible focus indicator (an outline or a ring) and has a name.
//   2. Contrast: every visible text node meets WCAG AA (4.5:1, or 3:1 for large text) against the
//      backgrounds stacked behind it. Text over a gradient or an image is counted, not graded.
//   3. Layout: the page never scrolls sideways and no control is wider than the viewport.
//   4. Names: every visible button, link, input, select and textarea has an accessible name.
// WARDYN_NEWRUN_SHOTS=<dir> also saves a full-page screenshot per step and width into that
// directory, for the review set; unset, no screenshot is taken.
import type { Page } from "@playwright/test";
import { test, expect, chooseFiles, chooseRunMode, chooseShape, continueNewRun, firstHarness, gotoConsole, mockPreflightClear, navToRoute, startGuided } from "./fixtures";
import { NR_FILES } from "../src/app/components/wardyn/copy/new-run/files";
import { NR_HARNESSES } from "../src/app/components/wardyn/copy/new-run/harnesses";
import { NR_LAUNCH } from "../src/app/components/wardyn/copy/new-run/launch";
import { NR_REVIEW } from "../src/app/components/wardyn/copy/new-run/review";
import { NR_SHAPE } from "../src/app/components/wardyn/copy/new-run/shape";
import { NR_SHARED } from "../src/app/components/wardyn/copy/new-run/shared";
import { NR_START } from "../src/app/components/wardyn/copy/new-run/start";
import { NR_TOOLS } from "../src/app/components/wardyn/copy/new-run/tools";

const WIDTHS = [390, 900, 1440] as const;
const SHOTS = process.env.WARDYN_NEWRUN_SHOTS;

const escape = (s: string) => s.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");

interface Stop {
  tag: string;
  name: string;
  indicator: boolean;
}

/** Tab through the page from its top and report each stop, until `reach` names the focused control or the cap. */
async function tabToReach(page: Page, reach: RegExp): Promise<{ stops: Stop[]; reached: boolean }> {
  // A person arrives on a step with focus on its heading (the step moves it there); start from the page's h1.
  await page.evaluate(() => {
    // In a dialog the person starts where the dialog put focus.
    if (document.querySelector("[role=dialog]")) return;
    const h1 = document.querySelector("main h1, h1") as HTMLElement | null;
    if (h1) {
      h1.tabIndex = -1;
      h1.focus();
    } else (document.activeElement as HTMLElement | null)?.blur();
  });
  const stops: Stop[] = [];
  const seen = new Set<string>();
  for (let i = 0; i < 250; i++) {
    await page.keyboard.press("Tab");
    const stop = await page.evaluate((): Stop | null => {
      const el = document.activeElement as HTMLElement | null;
      if (!el || el === document.body) return null;
      const byId = (ids: string | null) => (ids ?? "").split(/\s+/).map((id) => document.getElementById(id)?.textContent ?? "").join(" ");
      const labels = (el as HTMLInputElement).labels ? Array.from((el as HTMLInputElement).labels ?? []).map((l) => l.textContent ?? "").join(" ") : "";
      const name = (el.getAttribute("aria-label") || byId(el.getAttribute("aria-labelledby")) || labels || el.innerText || el.getAttribute("title") || "").replace(/\s+/g, " ").trim();
      const cs = getComputedStyle(el);
      const outline = cs.outlineStyle !== "none" && parseFloat(cs.outlineWidth) > 0;
      return { tag: el.tagName.toLowerCase() + (el.id ? `#${el.id}` : ""), name, indicator: outline || cs.boxShadow !== "none" };
    });
    if (!stop) continue;
    if (seen.has(`${stop.tag}|${stop.name}`)) break; // the order has come round
    seen.add(`${stop.tag}|${stop.name}`);
    stops.push(stop);
    if (reach.test(stop.name)) return { stops, reached: true };
  }
  return { stops, reached: false };
}

interface PageFacts {
  overflowX: number;
  wide: string[];
  unnamed: string[];
  lowContrast: string[];
  ungraded: number;
  graded: number;
  controls: number;
}

/** Layout, names and contrast of what is on screen now. Runs in the page. */
function readPage(): PageFacts {
  const ctx = document.createElement("canvas").getContext("2d", { willReadFrequently: true })!;
  const rgba = (css: string): [number, number, number, number] => {
    ctx.clearRect(0, 0, 1, 1);
    ctx.fillStyle = "#000";
    ctx.fillStyle = css;
    ctx.fillRect(0, 0, 1, 1);
    const d = ctx.getImageData(0, 0, 1, 1).data;
    return [d[0], d[1], d[2], d[3] / 255];
  };
  const lum = (c: number[]) => {
    const f = (v: number) => ((v /= 255) <= 0.03928 ? v / 12.92 : ((v + 0.055) / 1.055) ** 2.4);
    return 0.2126 * f(c[0]) + 0.7152 * f(c[1]) + 0.0722 * f(c[2]);
  };
  const visible = (el: Element) => {
    const r = el.getBoundingClientRect();
    const cs = getComputedStyle(el);
    return r.width > 0 && r.height > 0 && cs.visibility !== "hidden" && cs.display !== "none" && cs.opacity !== "0";
  };
  const label = (el: Element) => `${el.tagName.toLowerCase()}${(el as HTMLElement).id ? "#" + (el as HTMLElement).id : ""} "${(el.textContent ?? "").replace(/\s+/g, " ").trim().slice(0, 40)}"`;
  const facts: PageFacts = { overflowX: document.documentElement.scrollWidth - document.documentElement.clientWidth, wide: [], unnamed: [], lowContrast: [], ungraded: 0, graded: 0, controls: 0 };

  for (const el of Array.from(document.querySelectorAll("button, a[href], input, select, textarea, [role=button], [role=radio], [role=switch], [role=checkbox]"))) {
    if (!visible(el) || (el as HTMLInputElement).type === "hidden") continue;
    facts.controls++;
    if (el.getBoundingClientRect().width > window.innerWidth + 1) facts.wide.push(label(el));
    const h = el as HTMLElement;
    const byId = (ids: string | null) => (ids ?? "").split(/\s+/).map((id) => document.getElementById(id)?.textContent ?? "").join("");
    const labels = (h as HTMLInputElement).labels ? Array.from((h as HTMLInputElement).labels ?? []).map((l) => l.textContent ?? "").join("") : "";
    const name = h.getAttribute("aria-label") || byId(h.getAttribute("aria-labelledby")) || labels || h.innerText || h.getAttribute("title") || "";
    if (name.trim() === "") facts.unnamed.push(label(el));
  }

  const walker = document.createTreeWalker(document.body, NodeFilter.SHOW_TEXT);
  const seen = new Set<Element>();
  for (let n = walker.nextNode(); n; n = walker.nextNode()) {
    const el = n.parentElement;
    if (!el || seen.has(el) || (n.textContent ?? "").trim() === "" || !visible(el) || el.closest("[aria-hidden=true], script, style")) continue;
    seen.add(el);
    const cs = getComputedStyle(el);
    // Stack the backgrounds from the element out to the first opaque one.
    let bg: number[] = [255, 255, 255, 1];
    const layers: number[][] = [];
    let graded = true;
    for (let a: Element | null = el; a; a = a.parentElement) {
      const acs = getComputedStyle(a);
      if (acs.backgroundImage !== "none") { graded = false; break; }
      const c = rgba(acs.backgroundColor);
      if (c[3] > 0) { layers.push(c); if (c[3] >= 1) break; }
    }
    if (!graded) { facts.ungraded++; continue; }
    for (const c of layers.reverse()) bg = [c[0] * c[3] + bg[0] * (1 - c[3]), c[1] * c[3] + bg[1] * (1 - c[3]), c[2] * c[3] + bg[2] * (1 - c[3]), 1];
    facts.graded++;
    const fg = rgba(cs.color);
    const fgOver = [fg[0] * fg[3] + bg[0] * (1 - fg[3]), fg[1] * fg[3] + bg[1] * (1 - fg[3]), fg[2] * fg[3] + bg[2] * (1 - fg[3])];
    const [hi, lo] = [lum(fgOver), lum(bg)].sort((x, y) => y - x);
    const ratio = (hi + 0.05) / (lo + 0.05);
    const px = parseFloat(cs.fontSize);
    const large = px >= 24 || (px >= 18.66 && parseInt(cs.fontWeight, 10) >= 700);
    const disabled = !!el.closest(":disabled, [aria-disabled=true]");
    if (!disabled && ratio < (large ? 3 : 4.5)) facts.lowContrast.push(`${ratio.toFixed(2)}:1 ${label(el)}`);
  }
  return facts;
}

for (const width of WIDTHS) {
  test(`New Run at ${width}px: keyboard path, focus, contrast, layout and names on every step`, async ({ page }) => {
    test.setTimeout(180_000);
    await mockPreflightClear(page);
    // Open the console at desktop width, where its sidebar is up, then narrow to the width under test.
    await gotoConsole(page);
    await navToRoute(page, "/runs/new");
    await page.setViewportSize({ width, height: width < 600 ? 844 : 900 });
    await expect(page.getByRole("heading", { name: NR_START.TITLE, level: 1 })).toBeVisible();

    const findings: string[] = [];
    let shot = 0;
    const audit = async (step: string, reach: RegExp) => {
      // Let the step settle: fonts, the server's reads and the focus move the heading takes.
      await page.waitForLoadState("networkidle");
      const facts = await page.evaluate(readPage);
      // The audit must have looked at something: a page read that finds nothing proves nothing.
      if (facts.graded < 5 || facts.controls < 2) findings.push(`${step}: the audit read ${facts.graded} text nodes and ${facts.controls} controls`);
      if (facts.overflowX > 1) findings.push(`${step}: the page scrolls sideways by ${facts.overflowX}px`);
      for (const w of facts.wide) findings.push(`${step}: wider than the viewport: ${w}`);
      for (const u of facts.unnamed) findings.push(`${step}: no accessible name: ${u}`);
      for (const c of facts.lowContrast) findings.push(`${step}: low contrast ${c}`);
      const { stops, reached } = await tabToReach(page, reach);
      if (!reached) findings.push(`${step}: Tab never reached ${reach} in ${stops.length} stops: ${stops.map((x) => x.name || x.tag).join(" | ").slice(0, 600)}`);
      for (const s of stops) {
        if (!s.indicator) findings.push(`${step}: no visible focus indicator on ${s.tag} "${s.name.slice(0, 40)}"`);
        if (s.name === "") findings.push(`${step}: focus lands on an unnamed ${s.tag}`);
      }
      if (SHOTS) await page.screenshot({ path: `${SHOTS}/new-run-${String(++shot).padStart(2, "0")}-${step}-${width}.png`, fullPage: true });
    };
    // A control's name can carry a glyph or a lead-in the person also hears, so the target is matched inside it.
    const named = (name: string) => new RegExp(escape(name));
    const continueName = (step: string) => named(NR_SHARED.CONTINUE_TO(step));

    await audit("start", named(NR_START.GUIDED_FOOT));
    await startGuided(page);
    await audit("shape", named(NR_SHAPE.AGENT_TITLE));
    await chooseShape(page, "agent_task");
    await chooseRunMode(page, "interactive");
    await audit("run", continueName(NR_SHARED.STEP_HARNESSES));
    await continueNewRun(page, NR_HARNESSES.TITLE);
    await firstHarness(page);
    await audit("harnesses", continueName(NR_SHARED.STEP_FILES));
    await continueNewRun(page, NR_FILES.TITLE);
    await chooseFiles(page);
    await audit("files", continueName(NR_SHARED.STEP_TOOLS));
    await continueNewRun(page, NR_TOOLS.TITLE);
    await audit("tools", named(NR_TOOLS.REVIEW_RUN));
    await continueNewRun(page, NR_REVIEW.TITLE);
    await expect(page.getByRole("button", { name: NR_REVIEW.LAUNCH_RUN })).toBeEnabled();
    await audit("review", named(NR_REVIEW.LAUNCH_RUN));
    await page.getByRole("button", { name: NR_REVIEW.LAUNCH_RUN }).click();
    const dialog = page.getByRole("dialog", { name: NR_LAUNCH.TITLE });
    await expect(dialog).toBeVisible();
    // Launch is shut until the server's check of this exact run has answered.
    await expect(dialog.getByText(NR_LAUNCH.CHECKED_JUST_NOW)).toBeVisible();
    // And Launch stays shut until the run has a title.
    await dialog.getByLabel(NR_LAUNCH.TITLE_LABEL).fill("keyboard walk");
    // Keyboard in the dialog: focus is inside it, and Tab stays inside it.
    await page.keyboard.press("Tab");
    expect(await page.evaluate(() => !!document.activeElement?.closest("[role=dialog]"))).toBe(true);
    await audit("launch-dialog", named(NR_LAUNCH.LAUNCH));
    expect(await page.evaluate(() => !!document.activeElement?.closest("[role=dialog]"))).toBe(true);

    expect(findings, findings.join("\n")).toEqual([]);
  });
}
