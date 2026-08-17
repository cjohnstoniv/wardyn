/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

/*
 * Driving the Getting Started funnel, and deciding an egress approval.
 *
 * Lifted out of walkthrough.spec.ts unchanged. These are the two things the 0.5
 * series does over and over — nearly every video walks some part of the funnel,
 * and every governance video decides an approval — so they live here rather
 * than in whichever spec happened to need them first.
 *
 * Each helper reads the current page out of stage() instead of taking it as an
 * argument, which is exactly what it did when all of this was one file with one
 * module-level `page`.
 */

import { expect, type Locator, type Page } from "@playwright/test";
import { WORKSPACE_NAME } from "./task";
import { act, beat, caption, PACE, spotlight } from "./overlay";
import { stage } from "./stage";

// A held request waits on a HUMAN, so this is minutes, not seconds. It is a
// ceiling for waiting on the PRODUCT; the pacing the viewer sees comes from
// overlay.ts. Exported because the acts wait on approval rows themselves too.
export const APPROVAL_APPEARS = 300_000;

/** The funnel's footer "Next: <step>" button. */
export function nextButton(): Locator {
  return stage().getByRole("button", { name: /^Next:/ });
}

/** Which step the funnel is on, from the layout's "Step N of M" counter. */
export async function stepIndex(): Promise<number> {
  const t = await stage()
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
export async function advance(text?: string): Promise<void> {
  const page = stage();
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
export async function clearWorkspace(): Promise<void> {
  const page = stage();
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
export const SCOPE_MENU_LABEL: Record<"once" | "always", string> = { once: "Once", always: "Always" };

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
export async function decide(
  scope: Page | Locator,
  choice: "Approve" | "Deny",
  text: string,
  host?: string,
  decisionScope: "run" | "once" | "always" = "run",
): Promise<void> {
  const page = stage();
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
  // Give the viewer the ROW before the verdict. The approval often appears
  // below the terminal, and the old shape scrolled to it as a side effect of
  // the click itself — the row flashed into frame and was decided in the same
  // second, which on camera read as "something happened, apparently". Scroll
  // it into view first, park the ring on it, say the line, and hold a breath;
  // only then decide. The extra ~2s comes out of the 30s decision window,
  // which has room for it.
  await row.scrollIntoViewIfNeeded().catch(() => {});
  await spotlight(page, row);
  await caption(page, text);
  await beat(page, PACE.read + 900);
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
