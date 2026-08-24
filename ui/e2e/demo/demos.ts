/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

/*
 * Driving ONE demo card on the Getting Started funnel.
 *
 * Extracted from 03-what-it-stops.spec.ts on 2026-08-24, when that 15-act
 * mega-episode was split into 03a (core) + 03b/03c/03d (optional detours).
 * Every helper below is the mega-spec's own, moved verbatim — four files now
 * need them, and a copy per file is four places for a locator to rot.
 *
 * WHERE THE DEMOS LIVE. One demos surface: Getting Started. The /demos route is
 * gone (App.tsx redirects it into the funnel), each demo is a sub-step of an
 * "Egress demos" / "Secrets demos" phase reachable at `/setup?step=<demo-id>`,
 * and the per-demo card carries `demo-card-<id>`. Every card is reached by an
 * explicit `?step=` navigation rather than by walking Next, so a gated card (a
 * missing model, a missing secret) never derails a take — the deep-link
 * corrector (setup-screen.tsx) falls a dropped step back to its nearest
 * surviving neighbour, which is why each spec STAGES the secrets its steps gate
 * on before it rolls.
 *
 * Selectors are getByRole + accessible names + the `demo-*` testids, matching
 * the rest of the suite: a copy change breaks the takes loudly and in one place.
 */

import { expect, type Locator, type Page } from "@playwright/test";
import { act, beat, caption, centerInFrame, ffwdEnd, ffwdStart, PACE, spotlight } from "./overlay";

// Sandboxes are real containers — every deep-driven demo launches one. Minutes,
// not seconds. Ceilings for waiting on the PRODUCT; the pacing the viewer sees
// comes from overlay.ts. (funnel.ts's decide() has its own APPROVAL_APPEARS
// ceiling for a held request, since that one waits on a HUMAN.)
export const SANDBOX_UP = 240_000;
export const COMMAND_ECHOES = 60_000;

/** Hold for a SHORT stanza — the owner's staccato lines drag on PACE.read. */
export const BEAT_SHORT = 1400;

// ---------------------------------------------------------------------------
// The 03x episodes' secrets. wardyn-demo-key is written ON CAMERA in 03a (the
// masked-entry lesson, lifted from old-07 beat 6); every other secret — and
// wardyn-demo-key itself in the detour episodes, which never film that write —
// is staged off camera in a beforeAll, because the per-kind demos carry
// `needsSecret` and stepOrder(status) DROPS a demo whose secret is missing — a
// `?step=` to a dropped card silently re-corrects to a neighbour and films the
// wrong thing.
// Values are canary sentinels (>= secretmask MinLen 8), never real credentials
// — only strings the specs grep the sandbox screen for.
// ---------------------------------------------------------------------------
export const DEMO_KEY = "wardyn-demo-key";
export const DEMO_KEY_VALUE = "WARDYN-V03-KEY-CANARY-8Q4ZR7";
export const API_TOKEN = "wardyn-demo-api-token";
export const API_TOKEN_VALUE = "WARDYN-V03-APITOKEN-CANARY-5R2WX1";
export const PAT_SECRET = "wardyn-demo-pat";
export const PAT_VALUE = "WARDYN-V03-PAT-CANARY-7T9KM4";
export const SSH_SECRET = "wardyn-demo-ssh-key";
export const SSH_VALUE = "WARDYN-V03-SSHKEY-CANARY-NOT-A-REAL-PEM-3J6NQ";

// ---------------------------------------------------------------------------
// Off-camera plumbing.
// ---------------------------------------------------------------------------

/** Same bearer shape every sibling video and funnel.ts's helpers use. */
export function apiHeaders(): Record<string, string> | undefined {
  return process.env.WARDYN_DEMO_TOKEN ? { Authorization: `Bearer ${process.env.WARDYN_DEMO_TOKEN}` } : undefined;
}

/** PUT /api/v1/secrets/{name} {value} — store or overwrite. Best-effort. */
export async function putSecret(page: Page, name: string, value: string): Promise<void> {
  await page.request
    .put(`/api/v1/secrets/${encodeURIComponent(name)}`, { headers: apiHeaders(), data: { value } })
    .catch(() => {});
}

/** DELETE /api/v1/secrets/{name}. Best-effort. */
export async function deleteSecret(page: Page, name: string): Promise<void> {
  await page.request.delete(`/api/v1/secrets/${encodeURIComponent(name)}`, { headers: apiHeaders() }).catch(() => {});
}

/** /setup renders the welcome hero until this flag is set (onboarding-screen),
 *  and stage.ts hands a fresh profile — so a `?step=` deep link lands on the
 *  hero instead of the demo without it. Per-BROWSER; idempotent. */
export async function seedOnboardingSeen(page: Page): Promise<void> {
  await page.evaluate(() => {
    try {
      localStorage.setItem("wardyn-onboarding-seen", "1");
    } catch {
      /* private mode — ignore */
    }
  });
}

// ---------------------------------------------------------------------------
// Shared demo-driving primitives.
// ---------------------------------------------------------------------------

/**
 * COLD-OPEN an episode on its first demo, and prove the rig is live.
 *
 * The two-step opener, not openDemo(): a fresh take context has never seen the
 * welcome hero, and localStorage is per-ORIGIN, so seeding the flag needs a
 * document on that origin FIRST — `goto("/")`, seed, then the deep link. A cold
 * `openDemo` seeds nothing and lands on the hero instead of the demo.
 *
 * Every 03x episode calls this as its act 1 with its OWN first demo, so any of
 * them can be re-taken alone, in any order.
 */
export async function openEpisode(page: Page, id: string, title: string): Promise<Locator> {
  await page.goto("/");
  await seedOnboardingSeen(page);
  await page.goto(`/setup?step=${id}`);
  await page.bringToFront();

  // Fail here rather than minutes into a silent, caption-less take: each spec
  // file gets its OWN fresh browser, so nothing upstream proved the overlay
  // installed.
  await expect
    .poll(() => page.evaluate(() => typeof (window as unknown as Record<string, unknown>).__demo), { timeout: 15_000 })
    .toBe("object");

  await expect(page.getByRole("heading", { name: title, level: 2 })).toBeVisible({ timeout: 30_000 });
  return page.getByTestId(`demo-card-${id}`);
}

/** Open a demo by its funnel deep link and prove its card is on screen. The
 *  heading landing is ALSO the receipt for a needsSecret demo: without the
 *  secret it is dropped from stepOrder and the screen re-corrects elsewhere. */
export async function openDemo(page: Page, id: string, title: string): Promise<Locator> {
  await seedOnboardingSeen(page);
  await page.goto(`/setup?step=${id}`);
  await expect(
    page.getByRole("heading", { name: title, level: 2 }),
    `the demo step never opened — is its secret staged? (${id})`,
  ).toBeVisible({ timeout: 30_000 });
  return page.getByTestId(`demo-card-${id}`);
}

/** Start a demo's sandbox and fast-forward the boot. Returns the xterm screen
 *  locator, ready to type into. beat(200) before the span so a still-speaking
 *  caption is not compressed into the ffwd (which lands every later cue early). */
export async function startAndBoot(page: Page, card: Locator, id: string): Promise<Locator> {
  const start = card.getByTestId(`demo-start-${id}`);
  await expect(start, "the demo Start button is disabled — this stack has no ready barrier").toBeEnabled({
    timeout: 60_000,
  });
  await act(page, start, "Start it.");
  await beat(page, 200);
  await ffwdStart(page);
  try {
    await expect(card.locator(".xterm-screen").first()).toBeVisible({ timeout: SANDBOX_UP });
  } finally {
    // Real time resumes the instant the terminal is on screen — even on a
    // failed take, so the encoder gets a span this run actually spent.
    await ffwdEnd(page);
  }
  // .xterm-screen renders when AttachTerminal MOUNTS, before the PTY websocket
  // is up; typing here eats the first characters.
  await beat(page, PACE.read);
  // Cockpit framing: policy at the top, terminal + audit below, all in one frame.
  await frameRun(page, id);
  return card.locator(".xterm-screen").first();
}

/** expect.poll over innerText, NEVER toContainText — the series LAW: the xterm
 *  locator starves inside a take while the same text is demonstrably on screen.
 *  innerText is the exact signal the viewer sees. */
export async function pollScreen(screen: Locator, re: RegExp, message: string): Promise<void> {
  await expect
    .poll(async () => await screen.innerText().catch(() => "<no .xterm-screen>"), { timeout: COMMAND_ECHOES, message })
    .toMatch(re);
}

/** The exact command the operator would run, read off the demo card's own copy
 *  pill — polled so a `{grant_id}` placeholder is resolved from the live run's
 *  grants first (StepList substitutes after the run goes live). Keeps the typed
 *  command from ever drifting from the catalog, and handles per-run ids. */
export async function pillCmd(card: Locator, i: number): Promise<string> {
  const pill = card.getByTestId("demo-steps").getByRole("button", { name: "Copy command" }).nth(i);
  await expect
    .poll(async () => (await pill.innerText().catch(() => "{grant_id}")).trim(), {
      timeout: 60_000,
      message: `demo step ${i}'s command never resolved (still carries the literal {grant_id}?)`,
    })
    .not.toContain("{grant_id}");
  return (await pill.innerText()).trim();
}

/**
 * A chapter card that is NOT spoken (the series outro convention). overlay.ts's
 * chapter() always speaks what it renders; this drives the same overlay
 * primitive directly for the one card that must stay silent.
 */
export async function silentCard(page: Page, text: string): Promise<void> {
  const set = (t: string) =>
    page
      .evaluate((s: string) => {
        const d = (window as unknown as Record<string, Record<string, (...x: unknown[]) => void>>).__demo;
        d?.chapter?.(s, "");
      }, t)
      .catch(() => {
        /* overlay absent — a card is cosmetic, never fatal */
      });
  await set(text);
  await page.waitForTimeout(PACE.chapter);
  await set("");
  await page.waitForTimeout(400);
}

/** The policy card renders as YAML through tintLines, which makes EACH source
 *  line its own <div> under <pre><code> (see wardyn/code-block.tsx). So a single
 *  key/value is a targetable line: ring it to teach exactly what changed. The
 *  key is anchored at a line boundary (indent or the "- " list lead) so a nested
 *  "host:" inside a scope never collides with a top-level key. */
export function policyLine(card: Locator, id: string, key: string): Locator {
  return card
    .getByTestId(`demo-policy-${id}`)
    .locator("pre code > div")
    .filter({ hasText: new RegExp(`(^|\\s)${key}:`) });
}

/** Frame the run cockpit: scroll the policy to the top of the viewport so the
 *  policy, the terminal, and the audit panel below it are all in one frame while
 *  commands run. Relies on the demos-step layout (policy directly above the
 *  runner) and the 36vh terminal keeping the trio in view — so a later
 *  typeInTerminal, whose terminal is already visible, does not re-scroll it off. */
export async function frameRun(page: Page, id: string): Promise<void> {
  await page
    .evaluate((testid) => {
      document.querySelector(`[data-testid="${testid}"]`)?.scrollIntoView({ block: "start" });
    }, `demo-policy-${id}`)
    .catch(() => {});
  await page.waitForTimeout(500);
}

/** Ring one policy line and narrate it — the diff device: the first demo walks
 *  every line, each later demo highlights only what changed and why.
 *
 *  Since the 03 split, "the first demo" means the first demo OF EACH EPISODE: a
 *  detour that opened mid-diff would narrate a change against a policy its
 *  viewer never saw. */
export async function walkPolicyKey(
  page: Page,
  card: Locator,
  id: string,
  key: string,
  ...lines: string[]
): Promise<void> {
  const el = policyLine(card, id, key).first();
  // Assert the line EXISTS before ringing it — spotlight() silently rings
  // nothing on a missing locator, which would ship a take that narrates a key
  // with no highlight. A rename in demo-catalog.ts fails the rehearsal here.
  await expect(el, `policy key "${key}" not rendered on demo ${id} — did the catalog spec change?`).toBeVisible({
    timeout: 30_000,
  });
  await centerInFrame(el).catch(() => {});
  await spotlight(page, el);
  for (const l of lines) {
    await caption(page, l);
    await beat(page, PACE.read);
  }
  await spotlight(page, null);
}
