/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

/*
 * Episode 04 of the series — "Add a workspace".
 *
 * WHAT THIS FILMS. Episode 02 ended on a governed host with nothing to work
 * on. This episode hands it something: what a workspace IS (the one directory
 * a run may touch — the blast radius), onboarding the real slugify fixture
 * through the Add-workspace dialog with every choice explained (source, path,
 * auto-derived name, the Advanced drawer, and the read-only default that gets
 * deliberately overridden on camera), what the new workspace remembers (its
 * own allowed/denied-hosts ledger, empty for now and honest about it), and
 * write-only secrets — values a run can borrow by name but that no one,
 * including the operator, can ever read back. No run is launched; running
 * against this workspace is episode 04, and this episode ending with the
 * workspace ready is the seam between them.
 *
 * THE DIALOG IS THE OWNER'S, VERBATIM — local/episodes-03-12-scripts-current.md
 * ("# Episode 03 — Add a workspace"). Every blank-line stanza there is exactly
 * one caption here; SAY-ON-CLICK lines are act() captions spoken on the click
 * they name. Wording changes go through that file, never through this one.
 * Short stanzas ride BEAT_SHORT. The choreography (rings, typing, asserts) is
 * this file's job; the words are not.
 *
 * WHY THE SECRET IS NOT A PROVIDER NAME. A stored anthropic-api-key
 * participates in model-credential resolution on this stack, so a canary under
 * that name can be picked up as a real API key by episode 06/07's agent run
 * and fail it with a 401 — a broken take episodes later, traceable to a
 * teaching beat here. The beat teaches the same write-only property under a
 * neutral name no resolver will ever claim.
 *
 * STATE IT INHERITS. Episode 02's finished stack: model connected, funnel
 * demos done, NO workspaces and NO secrets. record-demo.sh --video 04 does
 * not reset, and re-materializes the slugify fixture on disk every take.
 * resetFixtures() below deletes whatever a PRIOR take of THIS episode left
 * (the slugify workspace row, the demo secret) so the take always films a
 * real creation.
 *
 * OWNERSHIP OF NOUNS. This episode owns the workspace `slugify` (onboarded on
 * camera, writable — later episodes run against it and need the diff to land)
 * and the secret `deploy-webhook-token`. It touches nothing else the series
 * uses.
 *
 * THE SENTINEL. Beat 4 pastes WARDYN-V02-CANARY-9K2QN into the Add-secret
 * dialog's Value field, which MASKS at entry (secrets.tsx) — its glyphs are
 * never on screen. At the paste the DOM value necessarily holds the
 * plaintext (asserted MASKED, not absent); from the save onward this file
 * asserts at every beat that could leak it that the sentinel appears
 * NOWHERE in the page.
 *
 * This is NOT a test. It asserts only enough to keep itself honest and to know
 * when to advance; a failure here means the recording is wrong, not that the
 * product is broken. It runs against the REAL compose stack on :8080.
 *
 * Driven by `scripts/record-demo.sh --video 04`, which globs this exact
 * filename and names the take wardyn-04-add-a-workspace-<stamp>.mp4. It
 * self-skips without WARDYN_DEMO=1 so a bare `pnpm e2e` can never point a
 * headed browser at a developer's live stack and start deleting workspaces.
 */

import { test, expect, type Page } from "@playwright/test";
import { WORKSPACE_NAME, WORKSPACE_PATH } from "./task";
import { act, beat, caption, centerInFrame, chapter, PACE, spotlight } from "./overlay";
import { sweepStaleState } from "./sweep";
// stage.ts is the rig: importing it registers this file's beforeAll/afterAll
// (one browser, one context, one recorded page), and every beat reads the page
// out of stage() inside a test body rather than closing over a module binding.
import { stage } from "./stage";

test.skip(!process.env.WARDYN_DEMO, "demo recording — run via `make record-demo` (exports WARDYN_DEMO=1)");

test.describe.configure({ mode: "serial" });

// ---------------------------------------------------------------------------
// This episode's nouns. WORKSPACE_NAME/WORKSPACE_PATH come from task.ts — they
// are the series-wide slugify fixture record-demo.sh materializes, and later
// episodes run against the row this episode creates. The secret is local:
// nothing else in the series touches it, and its name is deliberately NOT a
// provider name (see the header).
// ---------------------------------------------------------------------------

/** The secret Beat 4 adds. A neutral name no credential resolver claims. */
const SECRET_NAME = "deploy-webhook-token";

/**
 * The canary. Never a real credential — its only job is to be a string this
 * file can grep the whole page for. Video-scoped in the name so it cannot
 * collide with anything another take leaves behind.
 */
const SENTINEL = "WARDYN-V02-CANARY-9K2QN";

/** The owner's staccato lines read fast; PACE.read after one is dead air. */
const BEAT_SHORT = 1400;

/** Same bearer shape funnel.ts's clearWorkspace() and every sibling video use. */
function apiHeaders(): Record<string, string> | undefined {
  return process.env.WARDYN_DEMO_TOKEN ? { Authorization: `Bearer ${process.env.WARDYN_DEMO_TOKEN}` } : undefined;
}

/**
 * Per-take hygiene, off camera: delete the slugify workspace row and the demo
 * secret a PRIOR take of this video left behind, so Beat 2 always films a real
 * creation (onboarding a duplicate name is rejected and silently leaves the
 * dialog open — the failure mode walkthrough.spec.ts documented first).
 * Deliberately does NOT create anything: creation is the video.
 */
async function resetFixtures(page: Page): Promise<void> {
  const headers = apiHeaders();
  const wsRes = await page.request.get("/api/v1/workspaces", { headers });
  expect(wsRes.ok(), `GET /api/v1/workspaces failed (${wsRes.status()}) — is the stack up on :8080?`).toBe(true);
  const wsBody = await wsRes.json();
  const wsItems: { id?: string; name?: string }[] = Array.isArray(wsBody)
    ? wsBody
    : (wsBody?.items ?? wsBody?.workspaces ?? []);
  // ALL rows, not just slugify: Beat 1's entire premise is the EMPTY list
  // ("Right now, that list is empty."), and later episodes' workspaces
  // (record-demo, egress-lab) survive their own takes — a take of THIS
  // episode after theirs found two leftover rows and no empty-state card
  // (2026-08-23). Deleting them here is safe: those episodes recreate their
  // own nouns in their own beforeAll every take.
  for (const w of wsItems) {
    if (w?.id) {
      await page.request.delete(`/api/v1/workspaces/${w.id}`, { headers });
    }
  }
  const secRes = await page.request.get("/api/v1/secrets", { headers });
  expect(secRes.ok(), `GET /api/v1/secrets failed (${secRes.status()})`).toBe(true);
  const names: string[] = (await secRes.json())?.names ?? [];
  if (names.includes(SECRET_NAME)) {
    await page.request.delete(`/api/v1/secrets/${encodeURIComponent(SECRET_NAME)}`, { headers });
  }
}

/**
 * The load-bearing negative assertion of the secrets beat: called at every
 * point after the paste where the sentinel could plausibly leak, so a take
 * that narrates "we can't read it back" over a screen that prints the value
 * fails here instead of shipping.
 */
async function assertSentinelAbsent(page: Page, where: string): Promise<void> {
  await expect(page.locator("body"), `the sentinel secret rendered on screen at: ${where}`).not.toContainText(
    SENTINEL,
  );
}

test.beforeAll(async () => {
  if (!process.env.WARDYN_DEMO) return;
  // S6: deny stale pending approvals / kill stale runs first — the Approvals
  // badge otherwise carries a prior take's number through the whole video.
  await sweepStaleState([WORKSPACE_NAME]);
  await resetFixtures(stage());
});

// ---------------------------------------------------------------------------
// Cold open + Beat 1 — what a workspace is, and the list as the blast radius
// ---------------------------------------------------------------------------

test("V04 beat 1 — the blast radius", async () => {
  test.setTimeout(120_000);
  const page = stage();
  await page.goto("/workspaces");
  await page.bringToFront();

  // Fail here rather than minutes into a silent, caption-less take: every
  // narration call degrades to a no-op by design, so nothing downstream would
  // ever complain about an overlay that failed to install.
  await expect
    .poll(() => page.evaluate(() => typeof (window as unknown as Record<string, unknown>).__demo), { timeout: 15_000 })
    .toBe("object");

  const heading = page.getByRole("heading", { name: "Workspaces", level: 1 });
  await expect(heading).toBeVisible({ timeout: 30_000 });

  await chapter(page, "Add a workspace", "The one directory a run may touch");
  await caption(page, "In episode two, we turned this machine into a governed one.");
  await beat(page, PACE.read);
  await caption(page, "But there was still nothing for an agent to work on.");
  await beat(page, PACE.read);
  await caption(page, "That's what a workspace is.");
  await beat(page, BEAT_SHORT);
  await caption(page, "It gives a run access to one specific directory or repository — and nothing else.");
  await beat(page, PACE.read);

  // SCREEN: empty workspace list — S3: ring the empty-state card, not the H1
  // sitting above nothing, for the whole empty-list run of stanzas.
  await spotlight(page, page.getByText("No workspaces yet").locator(".."));
  await caption(page, "Right now, that list is empty.");
  await beat(page, BEAT_SHORT);
  await caption(page, "Which means the blast radius is empty too.");
  await beat(page, BEAT_SHORT);
  await caption(page, "No workspace attached?");
  await beat(page, BEAT_SHORT);
  await caption(page, "The run gets no project.");
  await beat(page, BEAT_SHORT);
  await caption(page, "No project means there's nothing here for it to touch.");
  await beat(page, PACE.read);
  await caption(page, "That's our first boundary.");
  await beat(page, BEAT_SHORT + 400);
  await spotlight(page, null);
});

// ---------------------------------------------------------------------------
// Beat 2 — the Add-workspace dialog, every choice explained
// ---------------------------------------------------------------------------

test("V04 beat 2 — onboard the project", async () => {
  test.setTimeout(150_000);
  const page = stage();

  // Empty install says "Add your first workspace"; once one exists it becomes
  // "Add workspace". resetFixtures() makes the first form the norm, matching
  // both keeps a --no-reset iteration working.
  await act(
    page,
    page.getByRole("button", { name: /Add your first workspace|Add workspace/ }).first(),
    "There are three ways to bring a project in.",
  );
  const dlg = page.getByRole("dialog");
  // By ROLE, not text: "Add workspace" is both the dialog title and its submit
  // button, so a bare getByText is a strict-mode violation.
  await expect(dlg.getByRole("heading", { name: "Add workspace" })).toBeVisible();

  // Owner claim, verified against source (runs_scm.go's in-sandbox clone via
  // the broker route + confineGitBrokerEgress's name-keyed GitHub denies): on
  // the GitHub App lane the sandbox's git remote points at the proxy, the
  // proxy does the fetch, and the token never leaves proxy memory. Scoped to
  // the App lane on purpose: PAT and SSH clones DO dial the git host from
  // inside, as the git card's footer admits.
  // S3: ring the Repository card while these three stanzas describe it.
  await spotlight(page, dlg.getByRole("button", { name: "Repository" }));
  await caption(page, "A repository can be cloned using the Git credential we configured during setup.");
  await beat(page, PACE.read);
  await caption(page, "And with the GitHub App, the sandbox doesn't need to talk to GitHub directly.");
  await beat(page, PACE.read);
  await caption(page, "The proxy handles that connection and keeps the credential outside the sandbox.");
  await beat(page, PACE.read);

  // Source cards are OptionCards — aria-pressed buttons whose accessible name
  // carries the hint text too, so match on a prefix. The ring flies from
  // Repository to Local directory as it's clicked.
  await act(page, dlg.getByRole("button", { name: /Local directory/ }), "Or use an existing directory on the machine.");
  await caption(page, "Today, though, we'll point at a project that's already on this machine.");
  await beat(page, BEAT_SHORT);

  const pathField = dlg.getByLabel("Path on this host");
  await spotlight(page, pathField);
  // S5: the path is the teaching — type it visibly rather than filling silently.
  await pathField.click();
  await page.keyboard.type(WORKSPACE_PATH, { delay: 30 });
  await caption(page, "The path is the boundary.");
  await beat(page, BEAT_SHORT);
  await caption(page, "This directory is available to the run.");
  await beat(page, BEAT_SHORT);
  await caption(page, "Everything above it isn't.");
  await beat(page, BEAT_SHORT + 400);
  await spotlight(page, null);

  // Name auto-derives from the path's basename (add-workspace-dialog.tsx's
  // deriveName) — never typed here, so this is proof the claim is true, not
  // narration over a field the driver quietly filled itself. The owner marked
  // this SAY-ON-CLICK, but there is nothing to click — the field fills itself;
  // the ring flying to it is the on-screen event the line rides.
  const nameField = dlg.getByLabel("Name", { exact: true });
  await expect(nameField).toHaveValue(WORKSPACE_NAME, { timeout: 10_000 });
  await spotlight(page, nameField);
  await caption(page, "The name comes from the folder.");
  await beat(page, BEAT_SHORT);
  await caption(page, "That's the name runs will use when they ask for it.");
  await beat(page, PACE.read);
  await spotlight(page, null);

  const advanced = dlg.getByText("Advanced", { exact: true });
  await act(page, advanced, "Open Advanced.");
  await caption(page, "This is where we get into the details.");
  await beat(page, BEAT_SHORT);

  // S3: one ring per card, moved as each is named — not one ring parked on
  // the whole group while three cards are walked in turn (persona round 1).
  await spotlight(page, dlg.getByRole("button", { name: /Standard sandbox image/ }));
  await caption(page, "Which image should the sandbox use?");
  await beat(page, BEAT_SHORT);
  await caption(page, "The standard image works for most projects.");
  await beat(page, BEAT_SHORT);
  // "Bring its own environment" is the card's own promise (add-workspace-
  // dialog.tsx) — BuildDevcontainer consumes the repo's .devcontainer/
  // devcontainer.json unmodified (docs/ENVBUILD.md).
  await spotlight(page, dlg.getByRole("button", { name: /devcontainer\.json/ }));
  await caption(page, "A project with a standard devcontainer can bring its own environment.");
  await beat(page, PACE.read);
  await spotlight(page, dlg.getByRole("button", { name: /Pinned image ref/ }));
  await caption(page, "Or you can pin an exact image if you need something specific.");
  await beat(page, PACE.read);
  await spotlight(page, dlg.getByLabel("Mount path"));
  await caption(page, "And this controls where the workspace appears inside the sandbox.");
  await beat(page, PACE.read);
  await spotlight(page, null);

  // BOTH halves of the writability lesson, in order. First the default — the
  // checkbox must genuinely start unchecked or the claim is false — then the
  // deliberate grant, because a later episode runs an agent against this
  // workspace and its edits have to reach the host.
  const writable = dlg.getByRole("checkbox", { name: /Allow writes/ });
  await expect(
    writable,
    "the checkbox must start UNCHECKED — the default is half the lesson",
  ).not.toBeChecked();
  await spotlight(page, writable);
  await caption(page, "Notice that writes are off by default.");
  await beat(page, BEAT_SHORT);
  await caption(page, "A workspace starts read-only.");
  await beat(page, BEAT_SHORT);
  await act(page, writable, "For this project, we're going to allow writes.");
  await expect(writable).toBeChecked();
  await caption(page, "That's a deliberate decision.");
  await beat(page, BEAT_SHORT);
  await caption(page, "The run gets write access because we gave it that access — not because it asked for it.");
  await beat(page, PACE.read);
  await spotlight(page, null);

  await act(page, dlg.getByRole("button", { name: "Add workspace" }), "Create.");
  await expect(dlg).toBeHidden({ timeout: 60_000 });

  // onCreated navigates straight to /workspaces/{id} (workspaces.tsx) — the
  // id in the URL IS the row the dialog just wrote.
  await expect(page).toHaveURL(/\/workspaces\/[0-9a-f-]{8,}/i, { timeout: 30_000 });
  await caption(page, "And that's it.");
  await beat(page, BEAT_SHORT);
  await caption(page, "No waiting for a scan or another setup process.");
  await beat(page, PACE.read);
  await caption(page, "The workspace is ready.");
  await beat(page, BEAT_SHORT + 400);
});

// ---------------------------------------------------------------------------
// Beat 3 — what the workspace remembers
// ---------------------------------------------------------------------------

test("V04 beat 3 — what it remembers", async () => {
  test.setTimeout(90_000);
  const page = stage();

  await expect(page.getByRole("heading", { name: WORKSPACE_NAME, level: 1 })).toBeVisible({ timeout: 30_000 });
  await caption(page, "Every workspace gets its own record.");
  await beat(page, BEAT_SHORT);
  await caption(page, "So the workspace isn't just a directory.");
  await beat(page, BEAT_SHORT);
  await caption(page, "It remembers how it's governed.");
  await beat(page, BEAT_SHORT);

  // Persona round 1: this banner owned the page unnamed — name it, move on.
  // By testid, not text: f6bf20c5 made the banner tier-derived, so its wording
  // differs across tiers while the testid holds.
  await spotlight(page, page.getByTestId("record-open-egress-banner"));
  await caption(page, "This warning is showing us what Record Mode looks like.");
  await beat(page, PACE.read);
  await caption(page, "We'll spend an entire episode on that.");
  await beat(page, BEAT_SHORT);
  await caption(page, "For now, the important part is that nothing is being recorded yet.");
  await beat(page, PACE.read);
  await spotlight(page, null);

  // The ledger is EMPTY today and the narration says so — that is the
  // teaching. Hosts land here two ways the series shows later: an approval
  // saved for good (episode 10) and a recording that proves what a task
  // really needs (episode 09).
  const allowedHeading = page.getByRole("heading", { name: /^Allowed hosts ·/, level: 2 });
  await expect(allowedHeading).toBeVisible({ timeout: 30_000 });
  await spotlight(page, allowedHeading);
  await caption(page, "The network starts with an empty memory too.");
  await beat(page, BEAT_SHORT);
  await caption(page, "Nothing has been earned yet.");
  await beat(page, BEAT_SHORT);
  await caption(page, "When a host is approved, or proven through a recording, that decision can become part of this workspace.");
  await beat(page, PACE.read);
  await spotlight(page, null);

  const deniedHeading = page.getByRole("heading", { name: /^Denied hosts ·/, level: 2 });
  // S2: center before speaking — the caption bar covered this heading
  // (persona round 1). The ring lands as "the other side" is named.
  await centerInFrame(deniedHeading);
  await spotlight(page, deniedHeading);
  await caption(page, "And the other side matters just as much.");
  await beat(page, BEAT_SHORT);
  await caption(page, "Denied hosts stay denied.");
  await beat(page, BEAT_SHORT);
  await caption(page, "A deny beats an allow.");
  await beat(page, BEAT_SHORT + 400);
  await spotlight(page, null);
});

// ---------------------------------------------------------------------------
// Beat 4 — write-only secrets
// ---------------------------------------------------------------------------

test("V04 beat 4 — write-only secrets", async () => {
  test.setTimeout(120_000);
  const page = stage();

  await caption(page, "A run may also need a secret.");
  await beat(page, BEAT_SHORT);

  await page.goto("/secrets");
  await expect(page.getByRole("heading", { name: "Secrets", level: 1 })).toBeVisible({ timeout: 30_000 });

  // Verified against internal/api/secrets.go (S1): the store hides only
  // wardyn-signing-key/wardyn-session-key, so what is on screen today is the
  // one secret setup minted — exactly what the line says.
  await caption(page, "The host already has one here — the SSH host key Wardyn created during setup.");
  await beat(page, PACE.read);
  await caption(page, "The important rule is simple:");
  await beat(page, BEAT_SHORT);
  await caption(page, "Secrets can go in.");
  await beat(page, BEAT_SHORT);
  await caption(page, "They don't come back out.");
  await beat(page, BEAT_SHORT + 400);

  await act(page, page.getByRole("button", { name: "Add secret", exact: true }), "Add secret.");
  const dlg = page.getByRole("dialog");
  await expect(dlg.getByRole("heading", { name: "Add secret" })).toBeVisible();

  const nameBox = dlg.getByLabel("Name");
  await spotlight(page, nameBox);
  await nameBox.fill(SECRET_NAME);
  await caption(page, "The name is the handle.");
  await beat(page, BEAT_SHORT);
  await caption(page, "Runs ask for a secret by name, never by its value.");
  await beat(page, PACE.read);

  // The Value field masks at entry (secrets.tsx's -webkit-text-security +
  // reveal toggle, added 2026-08-23 after this take showed the plaintext) —
  // so the sentinel's literal characters are NEVER legitimately on screen,
  // and the whole-page grep below can run from the paste itself.
  const valueBox = dlg.getByLabel("Value", { exact: true });
  await spotlight(page, valueBox);
  await valueBox.fill(SENTINEL);
  await beat(page, BEAT_SHORT);
  await spotlight(page, null);
  // NOT assertSentinelAbsent here: the DOM value necessarily holds the
  // plaintext until save (the field must submit it), and Playwright's page
  // text includes control values — the take that tried it failed on its own
  // paste. The visible claim at this moment is the MASK; assert exactly that.
  // The whole-page greps resume right after save, when the field clears.
  await expect
    .poll(
      () =>
        valueBox.evaluate(
          (el) => (getComputedStyle(el) as unknown as Record<string, string>).webkitTextSecurity ?? "",
        ),
      { timeout: 10_000 },
    )
    .toBe("disc");

  await act(page, dlg.getByRole("button", { name: "Save secret" }), "Save secret.");
  await expect(dlg).toBeHidden({ timeout: 30_000 });
  // From here on the sentinel is checked at every beat that could leak it.
  await assertSentinelAbsent(page, "Secrets screen, right after Save secret");
  await caption(page, "And once we save it, we can't read it back.");
  await beat(page, PACE.read);

  // Open the row menu (Rotate/Delete), then Escape without acting — nothing
  // here is meant to be clicked; the menu itself is the receipt.
  const row = page.getByRole("row", { name: new RegExp(SECRET_NAME) });
  await spotlight(page, row);
  await act(page, row.getByRole("button", { name: "Secret actions" }), "Open Secret Actions.");
  const menu = page.getByRole("menu");
  await expect(menu.getByRole("menuitem", { name: /Rotate/ })).toBeVisible();
  await expect(menu.getByRole("menuitem", { name: /Delete/ })).toBeVisible();
  await caption(page, "We can rotate it.");
  await beat(page, BEAT_SHORT);
  await caption(page, "We can delete it.");
  await beat(page, BEAT_SHORT);
  // The negative IS the claim — assert it before the line lands.
  await expect(menu.getByRole("menuitem", { name: /reveal|show|copy|view/i })).toHaveCount(0);
  await caption(page, "But we can't reveal it.");
  await beat(page, BEAT_SHORT);
  await caption(page, "The value was never even displayed — masked from the moment we typed it.");
  await beat(page, PACE.read);
  await caption(page, "From here on, even the person who created it can't ask Wardyn to show it again.");
  await beat(page, PACE.read + 400);
  await page.keyboard.press("Escape");
  await spotlight(page, null);
  await assertSentinelAbsent(page, "Secrets screen, row menu open");
});

// ---------------------------------------------------------------------------
// Conclusion — same shape episode 01 locked: say what you saw, tease the next.
// ---------------------------------------------------------------------------

test("V04 conclusion", async () => {
  test.setTimeout(60_000);
  const page = stage();

  await chapter(page, "What you just saw", "A workspace on the record, and a secret nobody can read");
  // S1: the recap describes the workspace — return to the list instead of
  // leaving the Secrets table underneath the words (persona round 1).
  await page.goto("/workspaces");
  await expect(page.getByRole("heading", { name: "Workspaces", level: 1 })).toBeVisible();
  await caption(page, "A workspace is the part of the machine a run is allowed to touch.");
  await beat(page, PACE.read);
  await caption(page, "This one points at a real project, and it's writable because we explicitly allowed it.");
  await beat(page, PACE.read);
  await caption(page, "The workspace also remembers its network decisions.");
  await beat(page, BEAT_SHORT);
  await caption(page, "And secrets become write-only the moment they're saved.");
  await beat(page, BEAT_SHORT);
  await caption(page, "Now we have somewhere for a run to work.");
  await beat(page, PACE.read);
  await caption(page, "And something real for it to work on.");
  await beat(page, BEAT_SHORT);
  // DIALOG-NEW-BEAT (dialog review, A19): 04 ends on what we built and never
  // says what it is FOR — every sibling conclusion hands off to the next
  // episode by name. Drafted; see local/light-episodes-dialog-flags.md.
  await caption(page, "Next, we spend it. Our first run.");
  await beat(page, BEAT_SHORT + 400);
  await caption(page, "");
  await silentCard(page, "Next — 05: Your first run");
});

/**
 * A chapter card that is NOT spoken — the outro card convention episode 01
 * set. overlay.ts's chapter() always speaks what it renders; this drives the
 * same overlay primitive directly for the one card that must stay silent.
 */
async function silentCard(page: Page, text: string): Promise<void> {
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
}
