/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

/*
 * Video 03 of the series — "Add a workspace".
 *
 * WHAT THIS FILMS. Video one ended on a governed host with nothing to work on.
 * This video hands it something: it teaches what a workspace IS (the one
 * directory a run may touch — the blast radius), onboards the real slugify
 * fixture through the Add-workspace dialog with every choice explained (source,
 * path, auto-derived name, the Advanced drawer, and the read-only default that
 * gets deliberately overridden on camera), looks at what the new workspace
 * remembers (its own allowed-hosts ledger, empty for now and honest about it),
 * and closes on write-only secrets — values a run can borrow by name but that
 * no one, including the operator, can ever read back. No run is launched;
 * running against this workspace is video 03, and this video ending with the
 * workspace ready is the seam between them.
 *
 * WHERE THE CODE CAME FROM. The onboarding choreography is walkthrough.spec.ts
 * act 4 (proven on camera) and the blast-radius/secrets beats are lifted from
 * the retired 04-workspaces-and-secrets spec — the series restructure
 * (2026-08-17) moved "add a workspace" up to slot 02 and that spec's
 * run-launch beats forward into video 03's territory. The narration is new,
 * in the register video 01 locked: teach what a thing is and why it exists
 * before touching it, transitions between chapters, no slogans.
 *
 * WHY THE SECRET IS NOT A PROVIDER NAME. The retired spec's secret beat used
 * `anthropic-api-key` with a canary value. On THIS stack that name is live
 * plumbing: a stored anthropic-api-key participates in model-credential
 * resolution, so a canary under that name can be picked up as a real API key
 * by video 03's agent run and fail it with a 401 — a broken take two videos
 * later, traceable to a teaching beat here. The beat teaches the same
 * write-only property under a neutral name no resolver will ever claim.
 *
 * STATE IT INHERITS. Video 01's finished stack: model connected, funnel demos
 * done, NO workspaces and NO secrets (video 01 no longer onboards anything).
 * record-demo.sh --video 03 does not reset, and re-materializes the slugify
 * fixture on disk every take. resetFixtures() below deletes whatever a PRIOR
 * take of THIS video left (the slugify workspace row, the demo secret) so the
 * take always films a real creation.
 *
 * OWNERSHIP OF NOUNS. This video owns the workspace `slugify` (onboarded on
 * camera, writable — video 03 runs against it and needs the diff to land) and
 * the secret `deploy-webhook-token`. It touches nothing else the series uses.
 *
 * THE SENTINEL. Beat 4 pastes WARDYN-V02-CANARY-9K2QN into the Add-secret
 * dialog's Value field — the one moment its literal characters are legitimately
 * on screen. From then on this file asserts, at every beat that could leak it,
 * that the sentinel appears NOWHERE in the page.
 *
 * This is NOT a test. It asserts only enough to keep itself honest and to know
 * when to advance; a failure here means the recording is wrong, not that the
 * product is broken. It runs against the REAL compose stack on :8080.
 *
 * Driven by `scripts/record-demo.sh --video 03`, which globs this exact
 * filename and names the take wardyn-02-add-a-workspace-<stamp>.mp4
 * (docs/README.md links that asset name — do not rename this file). It
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
// This video's nouns. WORKSPACE_NAME/WORKSPACE_PATH come from task.ts — they
// are the series-wide slugify fixture record-demo.sh materializes, and video
// 03 runs against the row this video creates. The secret is local: nothing
// else in the series touches it, and its name is deliberately NOT a provider
// name (see the header).
// ---------------------------------------------------------------------------

/** The secret Beat 4 adds. A neutral name no credential resolver claims. */
const SECRET_NAME = "deploy-webhook-token";

/**
 * The canary. Never a real credential — its only job is to be a string this
 * file can grep the whole page for. Video-scoped in the name so it cannot
 * collide with anything another take leaves behind.
 */
const SENTINEL = "WARDYN-V02-CANARY-9K2QN";

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
  for (const w of wsItems) {
    if (w?.id && w.name === WORKSPACE_NAME) {
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
 * that narrates "never read back" over a screen that prints the value fails
 * here instead of shipping.
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

test("V02 beat 1 — the blast radius", async () => {
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
  await caption(page, "Last video this host got its guardrails, and we watched five sandboxes obey them.");
  await beat(page, PACE.read);
  await caption(page, "But every one of those sandboxes was empty — nothing of yours was inside.");
  await beat(page, PACE.read);
  await caption(page, "A workspace is how your actual code gets in: one directory or repo, mounted on purpose.");
  await beat(page, PACE.read);

  // S3: ring the empty-state card, not the H1 sitting above nothing.
  await spotlight(page, page.getByText("No workspaces yet").locator(".."));
  await caption(page, "This list is the whole blast radius.");
  await beat(page, PACE.read);
  await caption(page, "Empty list, empty blast radius — right now a run could reach nothing at all.");
  await beat(page, PACE.read);
  await caption(page, "A run attaches a workspace, or it gets nothing — the rest of this machine does not exist to it.");
  await beat(page, PACE.read + 400);
  await spotlight(page, null);
});

// ---------------------------------------------------------------------------
// Beat 2 — the Add-workspace dialog, every choice explained
// ---------------------------------------------------------------------------

test("V02 beat 2 — onboard the project", async () => {
  test.setTimeout(120_000);
  const page = stage();

  // Empty install says "Add your first workspace"; once one exists it becomes
  // "Add workspace". resetFixtures() makes the first form the norm, matching
  // both keeps a --no-reset iteration working.
  await act(page, page.getByRole("button", { name: /Add your first workspace|Add workspace/ }).first());
  const dlg = page.getByRole("dialog");
  // By ROLE, not text: "Add workspace" is both the dialog title and its submit
  // button, so a bare getByText is a strict-mode violation.
  await expect(dlg.getByRole("heading", { name: "Add workspace" })).toBeVisible();

  await caption(page, "Three sources. A repository clones with the git credential you stored during setup.");
  await beat(page, PACE.read);
  // Owner note, verified against source (runs_scm.go's in-sandbox clone via
  // the broker route + confineGitBrokerEgress's name-keyed GitHub denies):
  // there is NO host-side pre-clone — the clone runs in the sandbox — but on
  // the GitHub App lane the sandbox still cannot reach GitHub itself. Its
  // remote points at the proxy, the proxy does the fetch, and the token never
  // leaves proxy memory. Scoped to the App lane on purpose: PAT and SSH
  // clones DO dial the git host from inside, as the git card's footer admits.
  // S3: ring the Repository card while these two captions describe it.
  await spotlight(page, dlg.getByRole("button", { name: "Repository" }));
  await caption(page, "With the GitHub App, the sandbox never talks to GitHub at all.");
  await beat(page, PACE.read);
  await caption(page, "Its git remote points at the proxy — the proxy fetches the repo, and it keeps the token.");
  await beat(page, PACE.read);

  // S3: the ring flies to the Empty card, then to Local directory as it's
  // clicked — one named thing at a time instead of a 20s frozen dialog.
  await spotlight(page, dlg.getByRole("button", { name: "Empty" }));
  // Source cards are OptionCards — aria-pressed buttons whose accessible name
  // carries the hint text too, so match on a prefix.
  await act(page, dlg.getByRole("button", { name: /Local directory/ }), "An empty workspace is a scratchpad. Today: a directory already on this machine.");

  const pathField = dlg.getByLabel("Path on this host");
  await spotlight(page, pathField);
  // S5: the path is the teaching — type it visibly rather than filling silently.
  await pathField.click();
  await page.keyboard.type(WORKSPACE_PATH, { delay: 30 });
  await caption(page, "The path is the boundary: this directory, and nothing above it, is reachable from inside.");
  await beat(page, PACE.read);
  await spotlight(page, null);

  // Name auto-derives from the path's basename (add-workspace-dialog.tsx's
  // deriveName) — never typed here, so this is proof the claim is true, not
  // narration over a field the driver quietly filled itself.
  const nameField = dlg.getByLabel("Name", { exact: true });
  await expect(nameField).toHaveValue(WORKSPACE_NAME, { timeout: 10_000 });
  await spotlight(page, nameField);
  await caption(page, "The name comes off the folder — it is how runs will ask for this workspace.");
  await beat(page, PACE.read);
  await spotlight(page, null);

  const advanced = dlg.getByText("Advanced", { exact: true });
  await act(page, advanced, "Advanced is where the sandbox-side details live.");
  await beat(page, PACE.read);

  // S3: one ring per card, moved as each is named — not one ring parked on
  // the whole group while three cards are walked in turn (persona round 1).
  await spotlight(page, dlg.getByRole("button", { name: /Standard sandbox image/ }));
  await caption(page, "Which container image the sandbox boots — the standard image suits most projects.");
  await beat(page, PACE.read);
  // "Built exactly as written" is the card's own hint (add-workspace-dialog.tsx)
  // — the narration matches it rather than promising more: BuildDevcontainer
  // consumes the repo's own .devcontainer/devcontainer.json unmodified
  // (docs/ENVBUILD.md).
  await spotlight(page, dlg.getByRole("button", { name: /devcontainer\.json/ }));
  await caption(page, "A repo that ships a standard devcontainer file just works — built exactly as written.");
  await beat(page, PACE.read);
  await spotlight(page, dlg.getByRole("button", { name: /Pinned image ref/ }));
  await caption(page, "Or pin an exact image ref, and Wardyn pulls it as given.");
  await beat(page, PACE.read);
  await spotlight(page, dlg.getByLabel("Mount path"));
  await caption(page, "And where this directory lands inside the box.");
  await beat(page, PACE.read);
  await spotlight(page, null);

  // BOTH halves of the writability lesson, in order. First the default — the
  // checkbox must genuinely start unchecked or the claim is false — then the
  // deliberate grant, because video 03 runs an agent against this workspace
  // and its edits have to reach the host. Teaching the default and then
  // overriding it on camera is the honest version of both beats.
  const writable = dlg.getByRole("checkbox", { name: /Allow writes/ });
  await expect(
    writable,
    "the checkbox must start UNCHECKED — the default is half the lesson",
  ).not.toBeChecked();
  await spotlight(page, writable);
  await caption(page, "Allow writes is off by default: a workspace mounts read-only until you say otherwise.");
  await beat(page, PACE.read);
  await act(page, writable, "This one is here for real work, so the write is granted — deliberately, on the record.");
  await expect(writable).toBeChecked();
  await beat(page, PACE.read);
  await spotlight(page, null);

  await act(page, dlg.getByRole("button", { name: "Add workspace" }), "One call. No scan queue, no build — usable immediately.");
  await expect(dlg).toBeHidden({ timeout: 60_000 });

  // onCreated navigates straight to /workspaces/{id} (workspaces.tsx) — the
  // id in the URL IS the row the dialog just wrote.
  await expect(page).toHaveURL(/\/workspaces\/[0-9a-f-]{8,}/i, { timeout: 30_000 });
});

// ---------------------------------------------------------------------------
// Beat 3 — what the workspace remembers
// ---------------------------------------------------------------------------

test("V02 beat 3 — what it remembers", async () => {
  test.setTimeout(60_000);
  const page = stage();

  await expect(page.getByRole("heading", { name: WORKSPACE_NAME, level: 1 })).toBeVisible({ timeout: 30_000 });
  await caption(page, "Every workspace gets a page like this — its own standing record.");
  await beat(page, PACE.read);

  // Persona round 1: this red banner owned the page for 25-90s here, unnamed
  // — every persona flagged it. Name it once, then move on to the ledger.
  // By testid, not the "Open recording on Fence" text the adjudication quoted:
  // f6bf20c5 made the banner tier-derived — the weakest-barrier wording only
  // renders under real CC1, and this host builds all three tiers. The caption
  // stays true either way (the banner states the open-egress worst case on
  // every tier; it is danger-red only on Fence).
  await spotlight(page, page.getByTestId("record-open-egress-banner"));
  await caption(page, "That warning panel is Record Mode printing its own worst case — episode nine's subject.");
  await beat(page, PACE.read);
  await caption(page, "Nothing records until you start it.");
  await beat(page, PACE.read);
  await spotlight(page, null);

  // The ledger is EMPTY today and the narration says so — that is the teaching.
  // Hosts land here in two ways the series shows later: an approval saved with
  // the Always scope (video on approvals) and a recording that proves what a
  // task really needs (video on Record Mode). Narrating a full card the viewer
  // can see is empty would be the exact overclaim this series keeps cutting.
  const allowedHeading = page.getByRole("heading", { name: /^Allowed hosts ·/, level: 2 });
  await expect(allowedHeading).toBeVisible({ timeout: 30_000 });
  await spotlight(page, allowedHeading);
  await caption(page, "Allowed hosts is the workspace's own network memory — empty, because it has earned nothing yet.");
  await beat(page, PACE.read);
  await caption(page, "When you approve a host for good, or a recording proves one, it lands here — with its reason.");
  await beat(page, PACE.read + 400);
  await spotlight(page, null);

  const deniedHeading = page.getByRole("heading", { name: /^Denied hosts ·/, level: 2 });
  // S2: center before speaking — the caption bar covered this heading (persona round 1).
  await centerInFrame(deniedHeading);
  await spotlight(page, deniedHeading);
  await caption(page, "Denied hosts is the other half, and a deny always beats an allow.");
  await beat(page, PACE.read);
  await spotlight(page, null);
});

// ---------------------------------------------------------------------------
// Beat 4 — write-only secrets
// ---------------------------------------------------------------------------

test("V02 beat 4 — write-only secrets", async () => {
  test.setTimeout(60_000);
  const page = stage();

  await caption(page, "One more thing a run may need handed to it: a secret.");
  await beat(page, PACE.read);

  await page.goto("/secrets");
  await expect(page.getByRole("heading", { name: "Secrets", level: 1 })).toBeVisible({ timeout: 30_000 });

  // Verified against internal/api/secrets.go:172-203 (S1): the store hides
  // only wardyn-signing-key/wardyn-session-key, so the model subscription
  // does not live here, and no git PAT existed on the take's stack — say
  // what is actually on screen instead.
  await caption(page, "Setup fills this same store — today it holds one secret, the SSH host key Wardyn minted.");
  await beat(page, PACE.read);
  await spotlight(page, page.getByText(/Exception:/));
  await caption(page, "The rule: values go in and never come out — one printed exception, the git token a clone borrows.");
  await beat(page, PACE.read);
  await spotlight(page, null);

  await act(page, page.getByRole("button", { name: "Add secret", exact: true }));
  const dlg = page.getByRole("dialog");
  await expect(dlg.getByRole("heading", { name: "Add secret" })).toBeVisible();

  const nameBox = dlg.getByLabel("Name");
  await spotlight(page, nameBox);
  await nameBox.fill(SECRET_NAME);
  await caption(page, "A name is the whole handle — runs are wired to a secret by name, never by value.");
  await beat(page, PACE.read);

  // The one moment the sentinel's literal characters are legitimately on
  // screen: an operator pasting a credential into a write-only field.
  const valueBox = dlg.getByLabel("Value");
  await spotlight(page, valueBox);
  await valueBox.fill(SENTINEL);
  await spotlight(page, null);
  await caption(page, "Paste the value once. After this click, no screen in Wardyn can show it again.");
  await beat(page, PACE.read + 400);

  await act(page, dlg.getByRole("button", { name: "Save secret" }));
  await expect(dlg).toBeHidden({ timeout: 30_000 });
  // From here on the sentinel is checked at every beat that could leak it.
  await assertSentinelAbsent(page, "Secrets screen, right after Save secret");

  // Open the row menu (Rotate/Delete), then Escape without acting — the
  // Radix menu can open above the row and outside the viewport for a
  // positional click; nothing here is meant to be clicked anyway.
  const row = page.getByRole("row", { name: new RegExp(SECRET_NAME) });
  await spotlight(page, row);
  await act(page, row.getByRole("button", { name: "Secret actions" }));
  const menu = page.getByRole("menu");
  await expect(menu.getByRole("menuitem", { name: /Rotate/ })).toBeVisible();
  await expect(menu.getByRole("menuitem", { name: /Delete/ })).toBeVisible();
  // S1: the proof was spoken before the menu opened, then played mute — move
  // the claim onto the receipt (persona round 1: "the … menu never opens").
  await caption(page, "Stored, and already unreadable. I can rotate it or delete it — never read it back.");
  await beat(page, PACE.read);
  await caption(page, "Rotate it, delete it — and no reveal. It never comes back.");
  await beat(page, PACE.read + 600);
  await expect(menu.getByRole("menuitem", { name: /reveal|show|copy|view/i })).toHaveCount(0);
  await page.keyboard.press("Escape");
  await spotlight(page, null);
  await assertSentinelAbsent(page, "Secrets screen, row menu open");

  await caption(page, "You just watched its only display — from here on, nobody sees it again, including you.");
  await beat(page, PACE.read + 400);
});

// ---------------------------------------------------------------------------
// Conclusion — same shape video 01 locked: say what you saw, tease the next.
// ---------------------------------------------------------------------------

test("V02 conclusion", async () => {
  test.setTimeout(60_000);
  const page = stage();

  await chapter(page, "What you just saw", "A workspace on the record, and a secret nobody can read");
  // S1: the recap describes the workspace page — return to it instead of
  // leaving the Secrets table underneath the words (persona round 1).
  await page.goto("/workspaces");
  await expect(page.getByRole("heading", { name: "Workspaces", level: 1 })).toBeVisible();
  await caption(page, "A workspace is the one piece of this machine a run is allowed to touch.");
  await beat(page, PACE.read);
  await caption(page, "This one mounts a real project, writable because you granted it — not because it asked.");
  await beat(page, PACE.read);
  await caption(page, "It keeps its own ledger of allowed and denied hosts, each with a reason.");
  await beat(page, PACE.read);
  await caption(page, "And the secrets a run may borrow are write-only from the moment they are saved.");
  await beat(page, PACE.read + 400);
  await caption(page, "Next: a run, in this workspace, doing real work under those rules.");
  await beat(page, PACE.read);
  await caption(page, "");
  await silentCard(page, "Next — 04: Your first run");
});

/**
 * A chapter card that is NOT spoken — the outro card convention video 01 set.
 * overlay.ts's chapter() always speaks what it renders; this drives the same
 * overlay primitive directly for the one card that must stay silent.
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
