/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

/*
 * Episode 00 — "Meet Wardyn". The front door.
 *
 *     WARDYN_DEMO_SKIP_MODEL=1 scripts/record-demo.sh --video 00
 *
 * WHAT THIS FILMS. The first video most people will watch, knowing one thing:
 * they want to sandbox an AI coding agent. In one keyless run it answers the
 * three questions a plain sandbox cannot — what a run may REACH, what it may
 * HOLD, and WHO DECIDED — each with its receipt on screen, and then it tears
 * down its own nouns on camera.
 *
 * THE SCRIPT IS THE OWNER'S, VERBATIM — local/review-0.7/dialog/00-script.md.
 * Every caption below is one line of its §3 transcript, in order, unedited;
 * the C-numbers in the comments are that transcript's. The choreography
 * (rings, typing, races, asserts) is this file's job; the words are not.
 * Wording changes go through the script, never through this file.
 *
 * OWNERSHIP OF NOUNS — the state law (plan H7). sweepStaleState() completes
 * onboarding, denies pending approvals and kills live runs; it deletes NO
 * workspace, secret, allowlist or policy. So this episode owns everything it
 * touches: the workspace `meet-wardyn` (created ON CAMERA), the secret
 * `metrics-push-token` (created ON CAMERA), and the `always` grant act 2 writes
 * onto that workspace. It never uses episode 04's `slugify` workspace nor
 * episode 10's `egress-lab` — both of those assert counts THEIR takes earned,
 * and a shared workspace is how two videos end up fighting over one permanent
 * grant. The close DELETES both nouns on camera; afterAll deletes them again,
 * best-effort, so a take that died mid-episode still leaves the stack clean for
 * the next one.
 *
 * WHAT IT LEAVES BEHIND. Finished runs. `meet-wardyn`'s three runs (the
 * terminal run, the recording, the confined replay) stay on the Runs board for
 * the rest of batch B — runs have no delete API and only episode 01's reset
 * wipes the board. No later batch-B episode asserts an empty board today; the
 * verifier arm for 00 pins that this is all it leaves.
 *
 * THE EMPTY DIRECTORY IS LOAD-BEARING. Onboarding SCANS the directory and
 * seeds an `egress:<host>` requirement row per detected ecosystem registry,
 * and AllowedHostsCard counts those alongside approved_egress. One stray
 * package.json in there and beat 1.10's "Allowed hosts · 0" is "· 1" with a
 * host nobody decided on camera — and beat 2.18's "· 1" receipt is somebody
 * else's. resetFixtures() recreates the directory empty every take.
 *
 * THE SENTINEL. Beat 3.4 types a canary into the Add-secret dialog's Value
 * field, which MASKS at entry (secrets.tsx) — its glyphs are never on screen.
 * At the paste the DOM value necessarily holds the plaintext (asserted MASKED,
 * not absent); from the save onward every beat that could leak it asserts the
 * sentinel appears NOWHERE on the page.
 *
 * This is NOT a test. It asserts only enough to keep itself honest and to know
 * when to advance; a failure here means the recording is wrong, not that the
 * product is broken. It runs against the REAL compose stack on :8080, in batch
 * B immediately after episode 02, and it self-skips without WARDYN_DEMO=1 so a
 * bare `pnpm e2e` can never point a headed browser at a developer's live stack
 * and start deleting workspaces.
 */

import { mkdirSync, rmSync } from "node:fs";

import { test, expect, type Page } from "@playwright/test";
import { POSITIONING } from "../../src/app/lib/governance-copy";
import {
  act,
  beat,
  caption,
  centerInFrame,
  chapter,
  ffwdEnd,
  ffwdStart,
  PACE,
  spotlight,
  typeInTerminal,
} from "./overlay";
import { apiHeaders, BEAT_SHORT, pollScreen, silentCard } from "./demos";
import { APPROVAL_APPEARS, decide } from "./funnel";
import { bootRun, REPLAY_OVER, RUN_BOOTS, RUN_OVER, waitUnlessGone } from "./runs";
import { sweepStaleState } from "./sweep";
// stage.ts is the rig: importing it registers this file's beforeAll/afterAll
// (one browser, one context, one recorded page), and every beat reads the page
// out of stage() inside a test body rather than closing over a module binding.
import { stage } from "./stage";

test.skip(!process.env.WARDYN_DEMO, "demo recording — run via `make record-demo` (exports WARDYN_DEMO=1)");

test.describe.configure({ mode: "serial" });

// ---------------------------------------------------------------------------
// This episode's nouns. Deliberately LOCAL, not in task.ts — see the header.
// ---------------------------------------------------------------------------

/** The workspace act 1 creates on camera, and the close deletes on camera. */
const WORKSPACE = "meet-wardyn";

/** Its host directory: EMPTY, under the workspaces root, recreated every take. */
const WORKSPACE_PATH =
  process.env.WARDYN_DEMO_MEET_WORKSPACE || `${process.env.HOME}/wardyn-demo/meet-wardyn`;

/** Act 2's run. Title is the only required field on the Terminal lane. */
const RUN_TITLE = "Meet Wardyn";

/** Act 3's secret. Neutral on purpose: a provider-shaped name participates in
 *  model-credential resolution on this stack, and a canary under one can be
 *  picked up as a real API key by a later episode's agent run. */
const SECRET_NAME = "metrics-push-token";

/** The canary. Never a real credential — its only job is to be a string this
 *  file can grep the whole page for. Video-scoped so it cannot collide with
 *  anything another take leaves behind. */
const SENTINEL = "WARDYN-V00-CANARY-4M7XD2";

/** Act 2's host: refused, then approved ALWAYS onto this workspace. IANA's
 *  sibling of episode 10's example.org — distinct host, zero shared state. */
const REACH_HOST = "example.net";

/** The host the RECORDING discovers. Deliberately NOT example.net: the promote
 *  diff would bucket that as already-approved and the "Approve 1 observed host"
 *  button would never render (record-pane.tsx's egressPromotionDiff). */
const RECORDED_HOST = "www.iana.org";

/** The host that is plainly not the job. Never dialled — held at CONNECT and
 *  denied on camera. */
const VILLAIN_HOST = "pastebin.com";

/** The recording session. sessionKeyOf("first job") — session-helpers.ts. */
const SESSION_NAME = "first job";
const SESSION_KEY = "first-job";

// The commands, exactly as the script's beat table writes them.
//
// REACH is typed TWICE and "Same command" is a line the narrator says out loud,
// so the string is one constant. It carries no --max-time: under the shipped
// default (deny_with_review) the request is refused at the CONNECT and curl
// returns immediately — there is nothing to time out.
const REACH = `curl -sSI https://${REACH_HOST}`;
// A readable receipt of ABSENCE: grep -c prints the count, and the count is 0.
const ENV_PROBE = `printenv | grep -ci ${SECRET_NAME}`;
// `--noproxy '*'` is required or curl routes the proxy's own URL through the
// proxy (03a:555). WARDYN_PROXY_URL is injected into every run's env
// (runs_dispatch_mounts.go).
const READBACK = `curl -sS -i --noproxy '*' "$WARDYN_PROXY_URL/wardyn/v1/secrets/${SECRET_NAME}"`;
const JOB = `curl -sSI https://${RECORDED_HOST}/`;
// --max-time 60 outlasts the proxy's ~30s wait_for_review hold so the on-camera
// Deny lands while the request is still parked.
const VILLAIN = `curl -sSI --max-time 60 https://${VILLAIN_HOST}`;

/** A response actually came back, rather than the proxy refusing the tunnel. */
const RESPONDED = /HTTP\/[\d.]+ 200/;

/**
 * The refusal, as the SHELL prints it. The script flags the exact text as
 * unverified — task.ts's note says the proxy answers 403 at the CONNECT and
 * "curl reports 000" — so this takes episode 09's own widened shape (09:1053)
 * rather than pinning curl's exit number. REHEARSAL: narrow it to whatever this
 * stack actually prints before the take.
 */
const REFUSED = /403|curl: \(\d+\)/;

// A capture is reconciled server-side after the run terminates, so this is
// minutes. A ceiling for waiting on the PRODUCT, never pacing.
const CAPTURE_SETTLES = 300_000;

// Captured on camera and reused: act 3 comes back to the run, act 4 comes back
// to the workspace.
let runUrl = "";
let workspaceUrl = "";

// ---------------------------------------------------------------------------
// Staging — off camera, and the part that must not be prose.
// ---------------------------------------------------------------------------

/**
 * Delete every workspace row and this episode's secret, and recreate the
 * workspace directory EMPTY.
 *
 * ALL workspace rows, not just ours: beat 0.1's entire premise is the empty
 * list ("No workspaces yet."), and a leftover row from another episode's take
 * means the cold open films a list instead of an empty state. Deleting them is
 * safe — every episode that owns a workspace recreates it in its own beforeAll.
 *
 * Deliberately creates NOTHING: the creation is the video.
 */
async function resetFixtures(page: Page): Promise<void> {
  const headers = apiHeaders();
  rmSync(WORKSPACE_PATH, { recursive: true, force: true });
  mkdirSync(WORKSPACE_PATH, { recursive: true });

  const wsRes = await page.request.get("/api/v1/workspaces", { headers });
  expect(wsRes.ok(), `GET /api/v1/workspaces failed (${wsRes.status()}) — is the stack up on :8080?`).toBe(true);
  const wsBody = await wsRes.json();
  const items: { id?: string; name?: string }[] = Array.isArray(wsBody)
    ? wsBody
    : (wsBody?.items ?? wsBody?.workspaces ?? []);
  for (const w of items) {
    if (w?.id) await page.request.delete(`/api/v1/workspaces/${w.id}`, { headers });
  }

  const secRes = await page.request.get("/api/v1/secrets", { headers });
  expect(secRes.ok(), `GET /api/v1/secrets failed (${secRes.status()})`).toBe(true);
  const names: string[] = (await secRes.json())?.names ?? [];
  if (names.includes(SECRET_NAME)) {
    await page.request.delete(`/api/v1/secrets/${encodeURIComponent(SECRET_NAME)}`, { headers });
  }
}

/**
 * The teardown the close already performs on camera, run again as a net.
 *
 * A take that dies in act 3 leaves a workspace row carrying an `always` grant
 * and a secret nobody deleted, and the NEXT take of this episode would then
 * open on a list instead of the empty state. Deleting the workspace row drops
 * its approved_egress, its denied_egress and its requirements contract in one
 * call, so the re-take is byte-identical to this one. Best-effort throughout:
 * a teardown that throws would fail a take that has already finished filming.
 */
async function teardown(): Promise<void> {
  // PLAIN fetch, not page.request, and sweep.ts's shape: afterAll hooks run in
  // DECLARATION order, and stage.ts's — declared first, because importing it is
  // how a spec gets the rig — is the one that closes the context. By the time
  // this runs the page may already be gone, so the teardown must not need one.
  const api = `${process.env.WARDYN_DEMO_BASE_URL || "http://localhost:8080"}/api/v1`;
  const headers = { "content-type": "application/json", ...(apiHeaders() ?? {}) };
  const res = await fetch(`${api}/workspaces`, { headers }).catch(() => null);
  if (res?.ok) {
    const body = (await res.json().catch(() => null)) as unknown;
    const items = (
      Array.isArray(body) ? body : ((body as { items?: unknown })?.items ?? [])
    ) as { id?: string; name?: string }[];
    for (const w of Array.isArray(items) ? items : []) {
      if (w?.id && w.name === WORKSPACE) {
        await fetch(`${api}/workspaces/${w.id}`, { method: "DELETE", headers }).catch(() => {});
      }
    }
  }
  await fetch(`${api}/secrets/${encodeURIComponent(SECRET_NAME)}`, { method: "DELETE", headers }).catch(() => {});
}

/**
 * The load-bearing negative of the secrets act: called at every point after the
 * paste where the sentinel could plausibly leak, so a take that narrates "no
 * screen can read that value back" over a screen printing it fails here
 * instead of shipping.
 */
async function assertSentinelAbsent(page: Page, where: string): Promise<void> {
  await expect(page.locator("body"), `the sentinel secret rendered on screen at: ${where}`).not.toContainText(
    SENTINEL,
  );
}

// Registered AFTER stage.ts's own beforeAll (import order), so stage() is
// assigned by the time this runs. Re-guarded on WARDYN_DEMO because this hook
// DELETES workspaces: a file-level test.skip must never be the only thing
// standing between a developer's stack and that.
test.beforeAll(async () => {
  if (!process.env.WARDYN_DEMO) return;
  // S6: deny stale pending approvals / kill stale runs first — the Approvals
  // badge otherwise carries a prior take's number through the whole video, and
  // the tick from 0 to 1 in act 2 is this episode's receipt.
  await sweepStaleState([WORKSPACE]);
  await resetFixtures(stage());
});

test.afterAll(async () => {
  if (!process.env.WARDYN_DEMO) return;
  await teardown().catch(() => {});
});

// ---------------------------------------------------------------------------
// Cold open — the three questions a sandbox alone cannot answer (C1-C6)
// ---------------------------------------------------------------------------

test("V00 cold open — three questions", async () => {
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

  await expect(page.getByRole("heading", { name: "Workspaces", level: 1 })).toBeVisible({ timeout: 30_000 });

  // S3: ring the empty-state CARD, not the h1 sitting above nothing. Its own
  // premise is asserted before it is narrated — a leftover row from another
  // take would make every line of this cold open false.
  const emptyCard = page.getByText("No workspaces yet.").locator("..");
  await expect(emptyCard, "a workspace row survived beforeAll — the cold open's empty list is not real").toBeVisible({
    timeout: 30_000,
  });
  await spotlight(page, emptyCard);

  await caption(page, "You want to sandbox an AI coding agent."); // C1
  await beat(page, BEAT_SHORT);
  await caption(page, "Good. A sandbox keeps it off your machine."); // C2
  await beat(page, BEAT_SHORT);
  await caption(page, "But a sandbox alone can't answer three questions."); // C3
  await beat(page, BEAT_SHORT);
  await caption(page, "What is it allowed to reach?"); // C4
  await beat(page, BEAT_SHORT);
  await caption(page, "What is it allowed to hold?"); // C5
  await beat(page, BEAT_SHORT);
  await caption(page, "And who decided?"); // C6
  await beat(page, BEAT_SHORT);
  await spotlight(page, null);

  await chapter(page, "Meet Wardyn", "Three answers, one run, no keys");
});

// ---------------------------------------------------------------------------
// Act 1 — a workspace (C7-C17)
// ---------------------------------------------------------------------------

test("V00 act 1 — a workspace", async () => {
  test.setTimeout(180_000);
  const page = stage();

  await spotlight(page, page.getByText("No workspaces yet.").locator(".."));
  await caption(page, "A run — one job inside a sandbox — touches nothing until you hand it a workspace."); // C7
  await beat(page, PACE.read);
  await spotlight(page, null);

  // Empty install says "Add your first workspace"; once one exists it becomes
  // "Add workspace". resetFixtures() makes the first form the norm; matching
  // both keeps a --no-reset iteration working.
  await act(page, page.getByRole("button", { name: /Add your first workspace|Add workspace/ }).first(), "Add one."); // C8

  const dlg = page.getByRole("dialog");
  // By ROLE, not text: "Add workspace" is both the dialog title and its submit
  // button, so a bare getByText is a strict-mode violation.
  await expect(dlg.getByRole("heading", { name: "Add workspace" })).toBeVisible();

  // Source cards are OptionCards — aria-pressed buttons whose accessible name
  // carries the hint text too, so match on a prefix.
  await act(page, dlg.getByRole("button", { name: /Local directory/ }), "A directory that's already on this machine."); // C9

  const pathField = dlg.getByLabel("Path on this host");
  await spotlight(page, pathField);
  // S5: the path is the teaching — type it visibly rather than filling silently.
  await pathField.click();
  await page.keyboard.type(WORKSPACE_PATH, { delay: 30 });
  await caption(page, "The path is the boundary."); // C10
  await beat(page, BEAT_SHORT);
  await caption(page, "Nothing above it exists to the run."); // C11
  await beat(page, PACE.read);
  await spotlight(page, null);

  // Name auto-derives from the path's basename (add-workspace-dialog.tsx's
  // deriveName) — never typed here, so this is proof the claim is true rather
  // than narration over a field the driver quietly filled itself.
  const nameField = dlg.getByLabel("Name", { exact: true });
  await expect(nameField).toHaveValue(WORKSPACE, { timeout: 10_000 });
  await spotlight(page, nameField);
  await caption(page, "The name comes from the folder."); // C12
  await beat(page, PACE.read);
  await spotlight(page, null);

  await act(page, dlg.getByText("Advanced", { exact: true }));
  const writable = dlg.getByRole("checkbox", { name: /Allow writes/ });
  // The checkbox must genuinely start unchecked or the claim is false.
  await expect(writable, "the checkbox must start UNCHECKED — the default IS the lesson").not.toBeChecked();
  await spotlight(page, writable);
  await caption(page, "Writes are off by default."); // C13
  await beat(page, BEAT_SHORT);
  await caption(page, "We'll leave it that way."); // C14
  await beat(page, BEAT_SHORT);
  // NOT toggled, deliberately: this episode never writes to the host, and a
  // demo that grants access it does not use teaches the wrong habit.
  await expect(writable).not.toBeChecked();
  await spotlight(page, null);

  await act(page, dlg.getByRole("button", { name: "Add workspace" }), "Create."); // C15
  await expect(dlg).toBeHidden({ timeout: 60_000 });

  // onCreated navigates straight to /workspaces/{id} — the id in the URL IS the
  // row the dialog just wrote.
  await expect(page).toHaveURL(/\/workspaces\/[0-9a-f-]{8,}/i, { timeout: 30_000 });
  workspaceUrl = page.url();
  await expect(page.getByRole("heading", { name: WORKSPACE, level: 1 })).toBeVisible({ timeout: 30_000 });

  // THE PRECONDITION THE WHOLE BACK HALF RESTS ON, stated as an assertion
  // rather than a hope: a fresh workspace vouches for NOTHING, so act 2's curl
  // is genuinely refused and act 2's "· 1" is a count this take earned. A
  // regex, not the literal: the heading is composed (`Allowed hosts · ${n}`).
  const allowedHeading = page.getByRole("heading", { name: /^Allowed hosts · 0$/, level: 2 });
  await expect(
    allowedHeading,
    `${WORKSPACE} starts with hosts already allowed — ${WORKSPACE_PATH} is not empty (a scan seeded egress rows)`,
  ).toBeVisible({ timeout: 30_000 });
  await centerInFrame(allowedHeading);
  await spotlight(page, allowedHeading);
  await expect(page.getByText("Nothing approved yet.")).toBeVisible();
  await caption(page, "The workspace keeps its own list of hosts a run may reach."); // C16
  await beat(page, PACE.read);
  await caption(page, "Right now: nothing. Nothing has been earned yet."); // C17
  await beat(page, PACE.read);
  await spotlight(page, null);
});

// ---------------------------------------------------------------------------
// Act 2 — a run, and a knock at the door (C18-C34)
// ---------------------------------------------------------------------------

test("V00 act 2 — a run, and a knock at the door", async () => {
  test.setTimeout(900_000);
  const page = stage();

  await act(page, page.getByRole("button", { name: "New run" }), "Start a run against it."); // C18
  await expect(page).toHaveURL(/\/runs\/new/, { timeout: 30_000 });

  // Fill BEFORE ringing: a ring that lands before the fill sits on an empty box
  // for its whole visible span — .fill() is instant, so "ring then fill" never
  // actually shows content inside it (S3: point at content).
  const titleBox = page.getByLabel("Title");
  await titleBox.fill(RUN_TITLE);
  await spotlight(page, titleBox);
  await caption(page, "A title."); // C19
  await beat(page, BEAT_SHORT);
  await spotlight(page, null);

  // Terminal, not the agent: this episode is keyless, and a shell is all a curl
  // needs. Seg renders role=radio buttons whose accessible name is the FULL
  // option label, so prefix-match it. The startup field it reveals is left
  // BLANK — the run comes up idle with a shell, which is what act 2 and act 3
  // type into, and it is why Title is the only required field on this lane.
  await act(page, page.getByRole("radio", { name: /^Terminal/ }), "A plain command window. No AI agent, and no key to any AI service."); // C20
  await caption(page, "The rules don't care who's typing — a person or an agent gets the same answer."); // C21
  await beat(page, PACE.read);

  // The workspace trigger has NO accessible name (the Agent select beside it is
  // labelled, this one was never wired up), so it is addressed by its
  // placeholder text — an honest workaround for a real a11y gap.
  await act(page, page.getByRole("combobox").filter({ hasText: "Ephemeral scratch" }), "Our workspace."); // C22
  await act(page, page.getByRole("option", { name: new RegExp(WORKSPACE, "i") }).first());

  // EVERYTHING ELSE IS LEFT ALONE, and that is the beat: the shipped defaults.
  // Asserted straight off the JSON textarea rather than a radio's aria-checked,
  // because a changed default would make the approval below never fire while
  // the take still went green.
  const spec = page.getByLabel("Spec (JSON)");
  await expect(spec, "the default first_use_approval is no longer deny_with_review").toHaveValue(
    /"first_use_approval": "deny_with_review"/,
  );
  await centerInFrame(spec);
  await spotlight(page, spec);
  await caption(page, "The default rules for a run: any host nobody listed is refused."); // C23a
  await beat(page, PACE.read);
  await caption(page, "And every refusal is raised for a person to decide."); // C23b
  await beat(page, PACE.read);
  await spotlight(page, null);

  // SILENT launch, and silent through the boot: nothing is spoken until there
  // is something to decide (and nothing may be spoken inside an ffwd span).
  await act(page, page.getByRole("button", { name: "Launch run" }));
  runUrl = await bootRun(page, "the run never came up as an attached terminal");

  await typeInTerminal(page, REACH);
  await pollScreen(
    page.locator(".xterm-screen").first(),
    REFUSED,
    `${REACH_HOST} was not refused — the run's policy is not default-deny`,
  );
  await caption(page, "Now dial a host nobody listed. Refused."); // C24
  await beat(page, BEAT_SHORT);

  // The PASSIVE-PENDING header, NOT the held one — the run's own policy says
  // deny-but-ask, so nothing is parked waiting. Asserting the exact header is
  // what makes "Refused" honest: the held flavour would mean the request is
  // still open, which is a different sentence entirely.
  //
  // RACED against the run's own terminal state: the cockpit mounts the strip
  // only while RUNNING, so a sandbox that dies here takes the header, the row
  // and the whole `always` beat with it.
  await waitUnlessGone(
    expect(page.getByText("Approval needed — off-policy egress")).toBeVisible({ timeout: APPROVAL_APPEARS }),
    page.getByText(RUN_OVER).first(),
    APPROVAL_APPEARS,
    `${REACH_HOST} never raised an approval`,
  );
  await spotlight(page, page.getByText("Approval needed — off-policy egress"));
  await caption(page, "And it asks."); // C25
  await beat(page, BEAT_SHORT);
  await spotlight(page, null);

  // ALWAYS A NAMED ROW, never "whatever is first in the queue" — approving
  // something you did not mean to approve is the worst possible frame in a
  // governance demo, and it has shipped once. The row is given to the viewer
  // BEFORE the verdict.
  const reachRow = page.getByTestId("live-approval-row").filter({ hasText: REACH_HOST }).first();
  await expect(reachRow).toBeVisible({ timeout: APPROVAL_APPEARS });
  await centerInFrame(reachRow);
  await spotlight(page, reachRow);
  await caption(page, "Refused at the proxy — nothing left the sandbox. A person decides."); // C26
  await beat(page, PACE.read);
  await spotlight(page, null);

  // decide() re-rings the same named row, speaks C27 over it, holds a breath,
  // then opens the scope menu and speaks C28 while it is open. `always` is only
  // clickable because THIS run resolves to a workspace; were it still disabled
  // the click would time out loudly rather than narrate over a greyed option,
  // and decide() then asserts the row cleared — a 400 from the scope rules
  // would otherwise leave it pending while the narrator says "Always".
  await decide(
    page,
    "Approve",
    "Approve it — and say how far the yes reaches.", // C27
    REACH_HOST,
    "always",
    "Once. This run. Until a time. Or always.", // C28 (spoken over the open menu)
  );
  await caption(page, "Always. It's saved to the workspace."); // C29
  await beat(page, PACE.read);

  // THE PAYOFF, and a combination no spec has filmed before: the retry
  // succeeds in the SAME run. The proxy serves apApproved from the decided
  // approval and an `always` scope is never consumed (internal/egress/proxy/
  // approvals.go). REHEARSAL: if the retry is still refused, the fallback is
  // episode 10's shape — cut this beat and let act 4's confined replay carry
  // the payoff (the script's open question 2).
  await typeInTerminal(page, REACH);
  await pollScreen(
    page.locator(".xterm-screen").first(),
    RESPONDED,
    `the approved retry to ${REACH_HOST} never answered 200 in the raising run`,
  );
  await caption(page, "Same command. This time the page comes back."); // C30
  await beat(page, PACE.read);

  // ---- The receipt, twice: the decision, then the workspace ---------------
  await act(page, page.getByRole("link", { name: "Approvals" }), "Every decision leaves a receipt."); // C31
  await expect(page.getByRole("heading", { name: "Approvals", level: 1 })).toBeVisible({ timeout: 30_000 });
  await act(page, page.getByRole("tab", { name: "Decided" }));

  // deriveTitle(egress_domain) renders `Reach <host>` (approvals.tsx). A
  // template literal, not a quoted composite: the row's text is assembled at
  // render time and there is no such literal in ui/src to pin it against.
  const decidedRow = page.getByText(`Reach ${REACH_HOST}`).first().locator("..");
  await expect(decidedRow).toBeVisible({ timeout: 30_000 });
  // The SCOPE is why this receipt and not the audit trail's: approvalScopeBadge
  // lowercases APPROVAL_SCOPE_LABEL, so the chip beside who/when reads "always".
  await expect(decidedRow, "the Decided row carries no `always` scope chip — the decision was not saved").toContainText(
    "always",
  );
  await centerInFrame(decidedRow);
  await spotlight(page, decidedRow);
  await caption(page, "Which decision, when — and how far it reaches. On a team install, the person's name sits here too."); // C32
  await beat(page, PACE.read);
  await spotlight(page, null);

  await act(page, page.getByRole("link", { name: "Workspaces" }), "And the workspace itself remembers."); // C33
  await act(page, page.getByRole("row", { name: new RegExp(WORKSPACE, "i") }).first());
  await expect(page.getByRole("heading", { name: WORKSPACE, level: 1 })).toBeVisible({ timeout: 30_000 });

  // Exactly one, the host that was held, with its provenance. A stale grant
  // from an earlier take would read "· 2" and the receipt would be someone
  // else's — which is what resetFixtures() exists to make impossible.
  await expect(page.getByRole("heading", { name: /^Allowed hosts · 1$/, level: 2 })).toBeVisible({ timeout: 30_000 });
  const receiptRow = page.getByRole("listitem").filter({ hasText: REACH_HOST });
  await expect(receiptRow).toBeVisible();
  await expect(receiptRow).toContainText("approved for this workspace");
  await centerInFrame(receiptRow);
  await spotlight(page, receiptRow);
  await caption(page, "One host, approved for this workspace — until you remove it."); // C34
  await beat(page, PACE.read);
  await spotlight(page, null);
});

// ---------------------------------------------------------------------------
// Act 3 — secrets stay secret (C35-C46)
// ---------------------------------------------------------------------------

test("V00 act 3 — secrets stay secret", async () => {
  test.setTimeout(300_000);
  const page = stage();

  await caption(page, "Reach is one half of the boundary. The other half is what a run may hold."); // C35
  await beat(page, PACE.read);

  await page.goto("/secrets");
  await expect(page.getByRole("heading", { name: "Secrets", level: 1 })).toBeVisible({ timeout: 30_000 });
  await act(
    page,
    page.getByRole("button", { name: "Add secret", exact: true }),
    "Runs sometimes need a key that's yours. Store it here — then we'll check exactly what the run can see of it.", // C36
  );

  const dlg = page.getByRole("dialog");
  await expect(dlg.getByRole("heading", { name: "Add secret" })).toBeVisible();
  const nameBox = dlg.getByLabel("Name");
  await spotlight(page, nameBox);
  await nameBox.fill(SECRET_NAME);
  await caption(page, "The name is the handle."); // C37
  await beat(page, PACE.read);

  // The Value field masks at ENTRY (secrets.tsx's -webkit-text-security), so
  // the sentinel's literal characters are never legitimately on screen.
  //
  // NOT assertSentinelAbsent here: the DOM value necessarily holds the
  // plaintext until save (the field must submit it), and Playwright's page text
  // includes control values — a take that tried it failed on its own paste. The
  // visible claim at this moment is the MASK; assert exactly that. The
  // whole-page greps resume right after save, when the field clears.
  const valueBox = dlg.getByLabel("Value", { exact: true });
  await spotlight(page, valueBox);
  await valueBox.fill(SENTINEL);
  await expect
    .poll(
      () =>
        valueBox.evaluate((el) => (getComputedStyle(el) as unknown as Record<string, string>).webkitTextSecurity ?? ""),
      { timeout: 10_000 },
    )
    .toBe("disc");
  await caption(page, "Masked as it's typed."); // C38
  await beat(page, PACE.read);
  await spotlight(page, null);

  await act(page, dlg.getByRole("button", { name: "Save secret" }), "Saved. No screen, and no run, can read that value back."); // C39
  await expect(dlg).toBeHidden({ timeout: 30_000 });
  // From here on the sentinel is checked at every beat that could leak it.
  await assertSentinelAbsent(page, "Secrets screen, right after Save secret");

  // The row's chip carries the product's one honest sentence for write-only
  // (secrets.tsx's WRITE_ONLY_TOOLTIP). Located BY that sentence, so a reworded
  // tooltip fails the take here instead of outliving the product's own words.
  const row = page.getByRole("row", { name: new RegExp(SECRET_NAME) });
  const writeOnlyChip = row.getByTitle(/never read back — not even by you/);
  await centerInFrame(writeOnlyChip);
  await spotlight(page, writeOnlyChip);
  await caption(page, "Write-only — the store says so."); // C40
  await beat(page, PACE.read);
  await spotlight(page, null);

  await act(page, row.getByRole("button", { name: "Secret actions" }));
  const menu = page.getByRole("menu");
  await expect(menu.getByRole("menuitem", { name: /Rotate/ })).toBeVisible();
  await expect(menu.getByRole("menuitem", { name: /Delete/ })).toBeVisible();
  // The negative IS the claim — assert it before the line lands.
  await expect(menu.getByRole("menuitem", { name: /reveal|show|copy|view/i })).toHaveCount(0);
  await caption(page, "Replace it. Delete it. Never read it."); // C41
  await beat(page, PACE.read);
  await page.keyboard.press("Escape");
  await assertSentinelAbsent(page, "Secrets screen, row menu open");

  // ---- and now from inside the box ---------------------------------------
  // The run act 2 left RUNNING: an idle shell, no agent, no model key.
  await page.goto(runUrl);
  const screen = page.locator(".xterm-screen").first();
  await waitUnlessGone(
    expect(screen).toBeVisible({ timeout: RUN_BOOTS }),
    page.getByText(RUN_OVER).first(),
    RUN_BOOTS,
    "the act-2 run is no longer attachable — act 3 has no sandbox to ask",
  );
  await beat(page, PACE.read);
  await caption(
    page,
    "Now ask the sandbox: does it hold that key? Count how many times its name appears in the run's environment.", // C42
  );
  await beat(page, BEAT_SHORT);

  await typeInTerminal(page, ENV_PROBE);
  // grep -c prints the count on its own line, and the count is 0. The screen
  // NECESSARILY carries the secret's NAME — the probe types it, and the shell
  // echoes what was typed — so unlike 03a's demo-card probe this cannot assert
  // the name is absent. The zero IS that assertion, from the shell's own mouth;
  // what stays checkable here is the VALUE, which must appear nowhere.
  await pollScreen(screen, /^0$/m, "the environment probe never printed a count");
  await assertSentinelAbsent(page, "the run's terminal, after the environment probe");
  await caption(page, "Zero. The run never received it."); // C43a
  await beat(page, BEAT_SHORT);
  await caption(page, "The default policy hands it none."); // C43b
  await beat(page, PACE.read);

  await typeInTerminal(page, READBACK);
  await pollScreen(
    screen,
    /404|unknown brokered route/,
    "the read-back probe never answered — expected a 404 from the proxy's brokered routes",
  );
  await assertSentinelAbsent(page, "the run's terminal, after the read-back probe");
  await caption(page, "Four-oh-four — not found."); // C44
  await beat(page, BEAT_SHORT);
  await spotlight(page, screen);
  await caption(
    page,
    "Not forbidden — there is simply no way to ask for it back. Not for the run; and this console has none either.", // C45
  );
  await beat(page, PACE.read);
  await spotlight(page, null);

  // End the run on camera. The dialog's own body says what the kill costs
  // ("tears down the sandbox, and revokes any brokered credentials"), so the
  // softer verb in C46 is not a euphemism the viewer cannot check.
  await act(page, page.getByRole("button", { name: "Kill", exact: true }), "End the run. The sandbox goes with it."); // C46
  const kill = page.getByRole("alertdialog");
  await expect(kill).toBeVisible({ timeout: 15_000 });
  await expect(kill).toContainText("tears down the sandbox");
  await act(page, kill.getByRole("button", { name: "Kill run" }));
  await expect(page.getByText(RUN_OVER).first()).toBeVisible({ timeout: 60_000 });
});

// ---------------------------------------------------------------------------
// Act 4 — record a policy, promote, run confined (C47-C64)
// ---------------------------------------------------------------------------

test("V00 act 4 — record a policy", async () => {
  test.setTimeout(1_800_000);
  const page = stage();

  await page.goto(workspaceUrl);
  // The ring goes on the card HEADER (h2 + its subtitle), not the whole card —
  // the subtitle is the half that says a policy gets learned here.
  const cardHeader = page.getByRole("heading", { name: "Recorded sessions", level: 2 }).locator("xpath=..");
  await centerInFrame(cardHeader);
  await spotlight(page, cardHeader);
  await caption(page, "When you don't know what a job needs — don't guess. Record it."); // C47
  await beat(page, PACE.read);
  await spotlight(page, null);

  // NewSessionForm pre-fills "build & test" only when the workspace has no
  // sessions; type anyway, because a re-take against a workspace that somehow
  // kept one finds the box EMPTY and a blank name is a 400 with the form still
  // standing.
  const nameBox = page.getByLabel("Session name");
  await spotlight(page, nameBox);
  await nameBox.fill(SESSION_NAME);
  await caption(page, "Name the session."); // C48
  await beat(page, BEAT_SHORT);
  await spotlight(page, null);

  // A DISABLED button is indistinguishable from a hung app on camera: act()
  // would park the ring on it for the full action timeout. NO MODEL IS NEEDED —
  // modelReady gates only RecordPane's warning note, never this button — which
  // is what makes a keyless recording legal at all.
  const start = page.getByRole("button", { name: "Start recording" });
  await expect(
    start,
    "Start recording is disabled — this stack has no runner, or another session for this workspace is still running",
  ).toBeEnabled({ timeout: 30_000 });
  await act(page, start, "Start recording."); // C49

  // FAST-FORWARD. POST /workspaces/{id}/record dispatches SYNCHRONOUSLY, so the
  // session card does not render at all until the container is provisioned.
  await beat(page, 200);
  const card = page.getByTestId(`session-${SESSION_KEY}`);
  await ffwdStart(page);
  try {
    await expect(card).toBeVisible({ timeout: RUN_BOOTS });
    await expect(card.getByText("Recording…")).toBeVisible({ timeout: RUN_BOOTS });
    await expect(card.locator(".xterm-screen").first()).toBeVisible({ timeout: RUN_BOOTS });
  } finally {
    await ffwdEnd(page);
  }

  // The open-egress banner is the honest half of "nothing is denied while
  // recording". By testid, not text: the banner is tier-derived, so its wording
  // differs across tiers while the testid holds. ASSERTED, not merely rung:
  // spotlight() silently rings nothing on a missing locator, and C50b says
  // "Look — the screen says so" — a claim about a thing that has to be there.
  const banner = page.getByTestId("record-open-egress-banner");
  await expect(banner, "no open-recording banner — C50b points at a box that is not on screen").toBeVisible({
    timeout: 30_000,
  });
  await spotlight(page, banner);
  await caption(page, "Recording runs wide open, on purpose — everything but the walls nobody can approve."); // C50a
  await beat(page, PACE.read);
  await caption(page, "Look — the screen says so."); // C50b
  await beat(page, PACE.read);
  await spotlight(page, null);
  await caption(page, "Only record work you trust."); // C51
  await beat(page, BEAT_SHORT);

  const screen = card.locator(".xterm-screen").first();
  await typeInTerminal(page, JOB, card);
  await pollScreen(screen, RESPONDED, `the recorded job never reached ${RECORDED_HOST}`);
  await caption(page, "One job. One host it reaches."); // C52
  await beat(page, PACE.read);

  await act(page, card.getByRole("button", { name: "Done recording" }));
  await caption(
    page,
    "Done. Wardyn builds the session from its own log of what the run actually did — not from a guess.", // C53
  );
  await beat(page, PACE.read);

  // FAST-FORWARD. The run has to die, and only THEN does the server reconcile
  // its capture out of the audit trail.
  //
  // AN EMPTY CAPTURE IS A FAILED TAKE, and it looks exactly like a good one
  // until someone reads the card: RecordReviewCard swaps the whole review for
  // the reachability warning, every later beat degrades, and the narration
  // keeps claiming a policy was learned. The two are mutually-exclusive
  // branches of one component, so racing them IS the verdict.
  await beat(page, 200);
  await ffwdStart(page);
  try {
    await waitUnlessGone(
      card.getByTestId("record-review").waitFor({ state: "visible", timeout: CAPTURE_SETTLES }),
      card.getByTestId("record-empty-capture"),
      CAPTURE_SETTLES,
      "the recording captured NO egress — on WSL2 host-mode NAT the sandbox cannot call the control plane back " +
        "and every capture lands empty; shoot this video in CONTAINERIZED mode",
    );
  } finally {
    await ffwdEnd(page);
  }
  await expect(card.getByText("Recorded", { exact: true })).toBeVisible({ timeout: 30_000 });

  // EXACTLY ONE HOST, asserted by NAME rather than by the button's count,
  // because the count alone cannot say which of the two causes it is: a
  // workspace directory that was not marker-free (a scan folded a registry into
  // the profile), or the CONTROL-PLANE HOST leaking into the approvable bucket
  // (the client excludes it by comparing against window.location.hostname,
  // "localhost", while the sandbox reaches it at WARDYN_CONTROL_PLANE_URL's
  // host, "wardynd" on compose — when those disagree this list grows a row
  // nobody can honestly approve on camera).
  const newHosts = card.getByTestId("record-new-hosts");
  await expect(newHosts).toContainText(RECORDED_HOST);
  expect(
    (await newHosts.locator("li").allTextContents()).map((s) => s.trim()),
    "the approvable set is not exactly the one recorded host — see the two causes above",
  ).toEqual([RECORDED_HOST]);
  await centerInFrame(newHosts);
  await spotlight(page, newHosts);
  await caption(page, "Here is what it actually reached."); // C54
  await beat(page, PACE.read);
  await spotlight(page, null);

  await act(page, card.getByRole("button", { name: /^Approve 1 observed host$/ }), "Seeing a host isn't allowing it."); // C55

  // Host names came from a session's observed traffic, so promotion routes
  // through the shared untrusted-content confirm. SINGLE host, so there are no
  // checkboxes and the action reads "Approve host" (confirm-egress-dialog.tsx:
  // checkboxes are bulk-only).
  const confirm = page.getByRole("alertdialog");
  await expect(confirm).toBeVisible({ timeout: 30_000 });
  await expect(confirm).toContainText(RECORDED_HOST);
  await act(page, confirm.getByRole("button", { name: "Approve host" }), "A person says so."); // C56

  // The no-remainder chip: egress_promoted is a boolean the server flips on any
  // promoted>0, and the card counts what is STILL approvable — with one host
  // promoted and none left, it reads the bare "Promoted" and not the partial
  // form. That string IS this beat's proof.
  const promoted = card.getByText("Promoted", { exact: true });
  await expect(promoted, "the promote did not land — the single observed host never reached the workspace").toBeVisible({
    timeout: 60_000,
  });
  await spotlight(page, promoted);
  await caption(page, "Approved — now it's a rule for this workspace, for every run from here on."); // C57
  await beat(page, PACE.read);
  await spotlight(page, null);

  // launchRecordRun's replay does NOT read a saved policy: it builds a fresh
  // spec straight off the WORKSPACE — AllowedDomains = confinedEgressDomains
  // (clone hosts ∪ profile/approved egress ∪ required-egress rows) with
  // FirstUseApproval hardcoded to wait_for_review. That union is exactly why
  // beat 4.14's example.net flows here: act 2 approved it `always` onto this
  // same workspace.
  await act(page, card.getByRole("button", { name: "Replay confined" }));
  await beat(page, 200);
  await ffwdStart(page);
  try {
    await waitUnlessGone(
      card.locator(".xterm-screen").first().waitFor({ state: "visible", timeout: RUN_BOOTS }),
      card.getByText(REPLAY_OVER),
      RUN_BOOTS,
      "the confined replay never came up as a live session — it settled before act 4 could type into it",
    );
  } finally {
    await ffwdEnd(page);
  }
  await beat(page, PACE.read);
  await caption(page, "Same job. Now every host it didn't record is refused unless a person says yes."); // C58
  await beat(page, PACE.read);

  const replayScreen = card.locator(".xterm-screen").first();
  await typeInTerminal(page, JOB, card);
  await pollScreen(replayScreen, RESPONDED, `${RECORDED_HOST} did not flow in the confined replay`);
  await expect(card.getByTestId("live-approvals-idle")).toBeVisible({ timeout: 30_000 });
  await caption(page, "The recorded host flows. Nothing to click."); // C59
  await beat(page, PACE.read);

  // THE IDLE STRIP IS THIS BEAT'S REAL PROOF, not the 200: the terminal still
  // holds the previous command's response, so a stale `HTTP/… 200` would
  // satisfy RESPONDED either way. "No prompt is raised" is the claim, and an
  // idle LiveApprovals strip is what makes it checkable on camera — the grader
  // re-checks it off the trail (exactly one approval.decide for this host, the
  // one act 2 made).
  await typeInTerminal(page, REACH, card);
  await pollScreen(replayScreen, RESPONDED, `${REACH_HOST}'s always-grant did not reach the confined replay`);
  await expect(card.getByTestId("live-approvals-idle")).toBeVisible({ timeout: 30_000 });
  await caption(page, "The host we approved 'always' in the first run — it flows here too, and no prompt is raised."); // C60
  await beat(page, PACE.read);

  // The villain LAST, while the shell is free: a confined replay runs
  // wait_for_review by construction, so this one is HELD at the door rather
  // than refused — and the hold is what a person decides on camera. Raced
  // against the replay ending, because the strip lives inside SessionCard's
  // `replaying` branch only.
  await typeInTerminal(page, VILLAIN, card);
  await waitUnlessGone(
    card
      .getByText("Sandbox is waiting — approve to let it through")
      .waitFor({ state: "visible", timeout: APPROVAL_APPEARS }),
    card.getByText(REPLAY_OVER),
    APPROVAL_APPEARS,
    `${VILLAIN_HOST} never surfaced as a HELD request while the replay was still live`,
  );
  const heldRow = card.getByTestId("live-approval-row").filter({ hasText: VILLAIN_HOST }).first();
  await expect(heldRow).toContainText("waiting");
  await spotlight(page, heldRow);
  await caption(
    page,
    "Something the job never needed. A confined replay holds it at the door instead of refusing — nothing has left, and a person decides.", // C61
  );
  await beat(page, PACE.read);
  await spotlight(page, null);

  // Decide it QUICKLY: the hold expires after ~30s (defaultHoldTimeout), and an
  // expired hold films as a timeout nobody decided.
  await decide(card, "Deny", "No.", VILLAIN_HOST); // C62
  await pollScreen(replayScreen, /403|curl: \(\d+\)|timed out/, "the denied command never failed on screen");
  await spotlight(page, replayScreen);
  await caption(page, "Refused — and on the record."); // C63
  await beat(page, PACE.read);
  await spotlight(page, null);

  await act(page, card.getByRole("button", { name: "Done", exact: true }));
  await beat(page, 200);
  await ffwdStart(page);
  try {
    await waitUnlessGone(
      card.getByTestId("verify-session-review").waitFor({ state: "visible", timeout: CAPTURE_SETTLES }),
      card.getByTestId("verify-session-failed"),
      CAPTURE_SETTLES,
      "the confined replay captured no egress decisions — the containment proof C64 names does not exist",
    );
  } finally {
    await ffwdEnd(page);
  }

  // A LITERAL 1, never a \d+: this episode denies exactly one villain and
  // abandons no hold, so 2+ means the replay reached something nobody scripted
  // and 0 means the deny never landed.
  const caught = card.getByTestId("verify-session-blocked");
  await expect(caught).toContainText(VILLAIN_HOST);
  await expect(caught).toContainText("blocked");
  await centerInFrame(caught);
  await spotlight(page, caught);
  await expect(
    card.getByText(/^Replayed — caught 1$/),
    "the confined replay did not settle at exactly one catch",
  ).toBeVisible({ timeout: 30_000 });
  await caption(page, "Caught one. Exactly the one that wasn't the job."); // C64
  await beat(page, PACE.read);
  await spotlight(page, null);
});

// ---------------------------------------------------------------------------
// Close — tear down our own nouns, then the four words (C65-C71)
// ---------------------------------------------------------------------------

test("V00 close", async () => {
  test.setTimeout(300_000);
  const page = stage();

  await page.goto(workspaceUrl);
  await expect(page.getByRole("heading", { name: WORKSPACE, level: 1 })).toBeVisible({ timeout: 30_000 });
  await caption(page, "Nothing here outlives your say-so."); // C65a
  await beat(page, PACE.read);

  await act(page, page.getByRole("button", { name: "Delete this workspace" }), "Delete the workspace."); // C65b
  const wsConfirm = page.getByRole("alertdialog");
  await expect(wsConfirm).toBeVisible({ timeout: 15_000 });
  await expect(wsConfirm).toContainText(WORKSPACE);
  await act(page, wsConfirm.getByRole("button", { name: /^Delete workspace$/ }));
  await expect(page).toHaveURL(/\/workspaces$/, { timeout: 30_000 });

  await page.goto("/secrets");
  const row = page.getByRole("row", { name: new RegExp(SECRET_NAME) });
  await act(page, row.getByRole("button", { name: "Secret actions" }), "Delete the secret."); // C66
  await act(page, page.getByRole("menu").getByRole("menuitem", { name: /Delete/ }));
  const secConfirm = page.getByRole("alertdialog");
  await expect(secConfirm).toBeVisible({ timeout: 15_000 });
  await expect(secConfirm).toContainText(SECRET_NAME);
  await act(page, secConfirm.getByRole("button", { name: /^Delete secret$/ }));
  await expect(page.getByRole("row", { name: new RegExp(SECRET_NAME) })).toHaveCount(0, { timeout: 30_000 });
  await assertSentinelAbsent(page, "Secrets screen, after the secret is deleted");

  // The frozen positioning string, imported rather than retyped
  // (governance-copy.ts §7.8). It renders as the welcome h1 on an UN-onboarded
  // install only, and every non-02 take marks the install onboarded
  // (sweep.ts) — so a chapter card carrying the same words is the only way this
  // episode can close on them.
  await chapter(page, POSITIONING.HERO_SLOGAN, "");
  await caption(page, "Sandboxed — a run touches one directory and nothing else."); // C67
  await beat(page, PACE.read);
  await caption(page, "Governed — every host this run reached and every key it used is a decision on the record."); // C68
  await beat(page, PACE.read);
  await caption(page, "Self-hosted — everything you just watched ran on one machine. Yours."); // C69
  await beat(page, PACE.read);
  await caption(page, "Free — open source, Apache-2.0, and the demos ship in the repo you installed from."); // C70
  await beat(page, PACE.read);
  await caption(
    page,
    "Start with episode one — why govern agents. Episode two is the install. Then episode three, part A — what it stops.", // C71
  );
  await beat(page, PACE.read);
  await caption(page, "");
  await silentCard(page, "Next — 01: Why govern agents");
});
