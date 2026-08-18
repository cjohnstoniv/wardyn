/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

/*
 * V05 — Autonomous agent.
 *
 * The second video of the 0.5 series. V01 stood the stack up, walked the
 * Getting Started funnel and onboarded a workspace; nothing has yet RUN inside
 * the boundary. This film is that: one real agent run, named, confined to a
 * single host, held once at the proxy on camera, finishing with a diff of the
 * files it actually touched.
 *
 * It is an extraction of walkthrough.spec.ts's act 5 (~339-588), and where it
 * covers the same ground the code was MOVED, not rewritten — the seeded-chip
 * handling, the unnamed-workspace-combobox workaround, decide()'s
 * decide-by-host discipline and the terminal-state wait are all hard-won on
 * camera. What act 5 had that this does NOT: the `always` scope, the workspace
 * receipt and the proof run. Those are V07's, and V05 deliberately grants only
 * "This run" so a later take of V05 still gets to raise its own hold.
 *
 * This is NOT a test. It asserts only enough to keep itself honest and to know
 * when to advance; a failure here means the recording is wrong, not that the
 * product is broken. It runs against the REAL compose stack on :8080 with a
 * live runner, real sandboxes and a CONNECTED MODEL — V05 is one of the two
 * quota-bound videos (V04/V05), and there is no model-free variant here on
 * purpose: every SAY line in the script is committed to an agent writing code.
 *
 * STATE IT INHERITS, and what it re-stages for itself:
 *  - The stack is up, in LOCAL MODE, and the browser session is the operator
 *    (SV1). That is load-bearing beyond convenience: inline_policy.go:80 clamps
 *    a MEMBER's first_use_approval down to the ceiling's deny_with_review, and
 *    only an operator's `wait_for_review` survives. Beat 8's whole claim —
 *    "held at the proxy, waiting on me" — is true only on the operator path.
 *  - A model provider is connected (V01 did it). Asserted before Launch,
 *    because the rail renders "No model provider is connected…" and beat 6
 *    spotlights that rail on camera.
 *  - Per SV16/SV20 this take is shot after V04's interactive take (the quota window), so it
 *    is recorded with `scripts/record-demo.sh --video 05 --no-reset`. A reset
 *    would destroy V08's reused run, the connected model lane and the SSH host
 *    key. Hygiene inside the window is DA5's workspace-scoped clearing only —
 *    which is exactly what this file's beforeAll does.
 *
 * Selectors are getByRole + accessible names, matching ui/e2e/fixtures.ts and
 * the rest of the suite: a copy change breaks this loudly and in one place,
 * markup churn does not break it at all.
 *
 * Driven by scripts/record-demo.sh --video 05 (it globs 05-*.spec.ts and names
 * the take wardyn-05-autonomous-agent-<stamp>.mp4 — docs/README.md links that
 * exact asset name, so this FILENAME IS LOAD-BEARING).
 */

import { mkdirSync, readFileSync } from "node:fs";
import { test, expect } from "@playwright/test";
import { DEMO_TASK, HELD_HOST, MODEL_HOST, WORKSPACE_NAME, WORKSPACE_PATH } from "./task";
import { act, beat, caption, PACE, spotlight } from "./overlay";
// The browser, the recorded context and the shared page live in stage.ts:
// importing it is what registers this file's beforeAll/afterAll, and each beat
// reads the page out of stage() rather than closing over a module-level `let`.
import { stage } from "./stage";
import { clearWorkspace, decide } from "./funnel";

test.skip(!process.env.WARDYN_DEMO, "demo recording — run via `make record-demo` (exports WARDYN_DEMO=1)");

test.describe.configure({ mode: "serial" });

// Waiting on the PRODUCT, not on the viewer. A real agent run on a real
// sandbox is minutes; the pacing the viewer sees comes from overlay.ts.
const RUN_FINISHES = 900_000;

/**
 * This run's TITLE, and the key everything downstream finds it by.
 *
 * UNIQUE TO V05 on purpose (DA5). The series shoots ten videos against one
 * long-lived stack, several of them launch runs, and scripts/verify-demo-take.sh
 * picks this take's run out of `wardyn runs list` BY EXACT TITLE — a title
 * shared with the walkthrough's own run (task.ts's DEMO_TITLE) would have the
 * verifier grading the wrong run and passing a take that never happened.
 *
 * OPERATOR: export the SAME string as WARDYN_DEMO_TITLE for the verify pass,
 * or leave both at their defaults. It is read from the environment here so one
 * export drives the driver and the verifier together.
 */
const RUN_TITLE = process.env.WARDYN_DEMO_TITLE || "V05 — slugify, one held host";

/** Filled on camera in beat 1. Short: the field renders two rows. */
const RUN_DESCRIPTION = "First governed run of the series — real code, one host held at the door.";

// --- Beat 10's nouns: the borrowed-secret proof (owner fold, 2026-08-17) ---
//
// A SEPARATE keyless proof run, on its OWN throwaway workspace — deliberately
// NOT a requirement on slugify: applyRequiredSecretGrant binds any required
// secret to the run agent's LLM-provider host, so wiring one onto slugify
// would hand THIS VIDEO'S AGENT RUN the canary as its Anthropic key and 401
// the model call. The proof run makes no traffic, so the dormant binding is
// harmless there.
const PROOF_WS_NAME = "secrets-proof";
const PROOF_WS_PATH = process.env.WARDYN_DEMO_PROOF_WS || `${process.env.HOME}/wardyn-demo/secrets-proof`;
const PROOF_TITLE = "Borrowed by name — never held";
/** Video 02's secret and canary. If 02 was never shot on this stack the
 *  beforeAll stores the secret itself, so this video stands alone. */
const PROOF_SECRET = "deploy-webhook-token";
const PROOF_CANARY = "WARDYN-V02-CANARY-9K2QN";

/** Where the workspace lands inside the sandbox (add-workspace-dialog.tsx's DEFAULT_TARGET). */
const MOUNT_TARGET = "/home/agent/work";

// ---------------------------------------------------------------------------
// STAGING — the preconditions this video cannot survive without.
//
// Two of them are ENCODED BELOW. The rest are the operator's, and they are
// listed here rather than in a shoot-day note because a precondition that
// lives only in prose gets forgotten:
//
//   1. Record with `scripts/record-demo.sh --video 05 --no-reset` (SV20). The
//      harness re-materializes ~/wardyn-demo/slugify from the fixture on EVERY
//      invocation regardless of --no-reset, so the on-disk workspace is always
//      virgin; what --no-reset protects is V08's run and the model lane.
//   2. A Claude subscription with quota. This launches a real agent.
//   3. A connected model provider, from V01. Asserted before Launch anyway.
//   4. Local mode / operator session (SV1) — see the file header on why the
//      hold in beat 8 depends on it.
//   5. Export WARDYN_DEMO_TITLE to RUN_TITLE's value if you override it.
// ---------------------------------------------------------------------------

test.beforeAll(async () => {
  const page = stage();

  // (a) The fixture must not already contain the agent's work. If a previous
  // take's slugify() is sitting in src/slug.js the agent has nothing to add,
  // the diff comes back with NOTES.md alone, and beat 9 — the payoff of the
  // whole video — fails 2:36 into a quota-burning take. Catch it here, before
  // anything launches. (record-demo.sh rebuilds this from
  // examples/workspaces/demo-node every run; this fires when the spec is
  // driven by hand.)
  const slug = readFileSync(`${WORKSPACE_PATH}/src/slug.js`, "utf8");
  expect(
    /(export\s+function|const)\s+slugify/.test(slug),
    `${WORKSPACE_PATH}/src/slug.js already defines slugify() — the agent has nothing to do. ` +
      `Re-materialize it from examples/workspaces/demo-node (record-demo.sh does this for you).`,
  ).toBe(false);

  // (b) Re-onboard the workspace, off camera, WRITABLE.
  //
  // Two separate hazards, one fix. `writable` defaults to FALSE in the Add
  // workspace dialog, so a workspace onboarded without ticking "Allow writes to
  // this directory" mounts READ-ONLY: the agent runs, reports success, and its
  // edits never reach the host — beat 9 films an empty diff under narration
  // that says otherwise. And a workspace carried over from a PRIOR take may
  // already hold an `always` grant for example.com in approved_egress, which
  // unions into this run's allowlist and means the host is never held at all —
  // beat 8, the point of the video, silently does not happen.
  //
  // Delete-and-recreate settles both: writable by construction, and egress
  // lists empty by construction. Via the API because this is SETUP, not
  // choreography — V01 owns the on-camera onboarding.
  const headers = process.env.WARDYN_DEMO_TOKEN
    ? { Authorization: `Bearer ${process.env.WARDYN_DEMO_TOKEN}` }
    : undefined;
  await clearWorkspace();
  const created = await page.request.post("/api/v1/workspaces", {
    headers,
    data: {
      name: WORKSPACE_NAME,
      sources: [{ type: "local_dir", path: WORKSPACE_PATH, target: MOUNT_TARGET, writable: true }],
    },
  });
  // `name` is UNIQUE (migration 0008), so a create that lands proves the delete
  // above really removed the old row — a silently-failed delete surfaces here
  // as a duplicate-key 500 rather than as a stale workspace nobody noticed.
  expect(
    created.ok(),
    `could not onboard "${WORKSPACE_NAME}" at ${WORKSPACE_PATH} — HTTP ${created.status()}: ${await created.text()}`,
  ).toBe(true);

  const ws = await created.json();
  // Assert the two properties the beats depend on, on the row the server
  // actually stored, rather than trusting the payload we just sent.
  expect(ws.sources?.[0]?.writable, "the workspace mounted read-only — beat 9 would film an empty diff").toBe(true);
  expect(
    ws.approved_egress ?? [],
    "approved_egress is not empty — a leftover Always grant means example.com is never held, and beat 8 does not happen",
  ).toEqual([]);
  expect(ws.denied_egress ?? [], "denied_egress is not empty — a leftover deny would refuse the host outright").toEqual([]);

  // (c) Beat 10's staging: the throwaway proof workspace and its secret.
  // Delete any leftover proof workspace, make sure the secret EXISTS (video 02
  // creates it on camera; storing it here keeps this video standalone), then
  // recreate the workspace with the one requirement the beat is about.
  {
    mkdirSync(PROOF_WS_PATH, { recursive: true });
    const wsList = await page.request.get("/api/v1/workspaces", { headers });
    const items: { id?: string; name?: string }[] = wsList.ok()
      ? ((await wsList.json())?.items ?? (await wsList.json().catch(() => null)) ?? [])
      : [];
    for (const w of Array.isArray(items) ? items : []) {
      if (w?.id && w.name === PROOF_WS_NAME) await page.request.delete(`/api/v1/workspaces/${w.id}`, { headers });
    }
    const names: string[] = (await (await page.request.get("/api/v1/secrets", { headers })).json().catch(() => ({})))?.names ?? [];
    if (!names.includes(PROOF_SECRET)) {
      await page.request.put(`/api/v1/secrets/${PROOF_SECRET}`, { headers, data: { value: PROOF_CANARY } });
    }
    const mk = await page.request.post("/api/v1/workspaces", {
      headers,
      data: { name: PROOF_WS_NAME, sources: [{ type: "local_dir", path: PROOF_WS_PATH, target: MOUNT_TARGET, writable: false }] },
    });
    expect(mk.ok(), `could not create ${PROOF_WS_NAME} (${mk.status()})`).toBe(true);
    const proofWsId = (await mk.json())?.id as string;
    const reqRes = await page.request.put(`/api/v1/workspaces/${proofWsId}/requirements`, {
      headers,
      data: { requirements: { [`secret:${PROOF_SECRET}`]: { level: "required", provenance: "operator_set" } } },
    });
    expect(reqRes.ok(), `could not seed the ${PROOF_SECRET} requirement (${reqRes.status()})`).toBe(true);
  }

});

// ---------------------------------------------------------------------------
// Beats 1-6 — building the run. Everything the run is allowed to do, declared
// before it starts.
// ---------------------------------------------------------------------------

test("beats 1-6 — name it, aim it, fence it", async () => {
  test.setTimeout(600_000);
  const page = stage();

  await page.goto("/runs");
  await page.bringToFront();

  // Fail here rather than three minutes into a silent, caption-less take:
  // every narration call degrades to a no-op by design, so nothing downstream
  // would ever complain about an overlay that failed to install.
  await expect
    .poll(() => page.evaluate(() => typeof (window as unknown as Record<string, unknown>).__demo), { timeout: 15_000 })
    .toBe("object");

  // COLD OPEN. No chapter card: the script gives this video two spoken opening
  // lines and no title, and chapter() would speak words that are not in it.
  await caption(page, "Last video we stood up Wardyn, a workspace, and a model.");
  await beat(page, PACE.read);
  await caption(page, "Nothing has run inside the boundary yet. Now something does.");
  await beat(page, PACE.read + 600);

  await act(page, page.getByRole("button", { name: "New run" }));
  await expect(page).toHaveURL(/\/runs\/new/, { timeout: 30_000 });

  // --- B1 Name the run ----------------------------------------------------
  //
  // The title is required (Launch stays disabled without one) and it is the key
  // the Runs board groups by, so it is also what this run is called everywhere
  // it appears afterwards — including in the verifier.
  const titleBox = page.getByLabel("Title");
  await spotlight(page, titleBox);
  await titleBox.fill(RUN_TITLE);
  await caption(page, "The title is how you find this later — same title, same work.");
  await beat(page, PACE.read);
  await spotlight(page, null);

  const descBox = page.getByLabel("Description");
  await spotlight(page, descBox);
  await descBox.fill(RUN_DESCRIPTION);
  await spotlight(page, null);
  await beat(page, 900);

  // --- B2 What to run -----------------------------------------------------
  //
  // "Agent task" and "Claude Code" are both already the wizard's defaults
  // (wizard-types.ts initialWizardState: runType "agent", agent "claude-code").
  // Clicking the radio anyway gives the camera a ring on the choice being
  // named; the Agent select is SPOTLIT rather than opened, because opening a
  // Select to re-pick the value it already holds is two extra clicks and a
  // portal for no visible gain.
  await act(page, page.getByRole("radio", { name: "Agent task" }));
  // By id, not getByLabel("Agent"): the trigger is a <button>, and a label/for
  // pointing at a button is not the form-control association getByLabel
  // resolves. #nr-agent is the SelectTrigger's own id (new-run-screen.tsx).
  await spotlight(page, page.locator("#nr-agent"));
  await caption(page, "Agent task, run by Claude Code. A shell command is governed identically.");
  await beat(page, PACE.read);
  await spotlight(page, null);

  // ORDER IS LOAD-BEARING, twice over.
  //
  // (1) initialWizardState() defaults to mode "interactive", which launches an
  // IDLE sandbox waiting for a human to type. The agent never executes the
  // task, so nothing reaches for example.com and the held approval beats 7-8
  // are built around can never appear.
  // (2) This must be clicked BEFORE the task is filled: an interactive run has
  // no Task field at all (new-run-screen.tsx's isInteractive branch renders
  // "Start with" instead), so the box does not exist until this click.
  //
  // B-DEPENDENT (flagged in the script, and REAL as of today): the option in
  // ui/src renders "Batch — run it unattended" (new-run-screen.tsx:478) while
  // the caption below — verbatim from the script — says "Autonomous". copy.ts's
  // RUN_MODE.autonomous.label is already "Autonomous" and Track B's BB2 renames
  // the option to match; the alternation here survives that rename instead of
  // killing a shoot-day take. Until B lands, the narration and the button
  // disagree ON CAMERA — see the report.
  await act(
    page,
    page.getByRole("radio", { name: /^(Autonomous|Batch)/ }),
    "Autonomous: it works unattended, and I appear only when policy stops it.",
  );

  const taskBox = page.getByLabel("Task");
  await spotlight(page, taskBox);
  await taskBox.fill(DEMO_TASK);
  await spotlight(page, null);
  await caption(page, "In plain English: add slugify, add a test, reach two hosts.");
  await beat(page, PACE.read + 1400);

  // --- B3 Workspace -------------------------------------------------------
  //
  // Selected by its VISIBLE TEXT, not its accessible name: this trigger has no
  // accessible name at all (the Agent select beside it is labelled "Agent" via
  // Field/Label htmlFor; this one was never wired up), so
  // getByRole("combobox", {name}) can never match it. Filtering on the
  // placeholder text is the honest workaround until the control gets a label —
  // an unnamed combobox is a real a11y gap, not just a test inconvenience.
  await act(
    page,
    page.getByRole("combobox").filter({ hasText: "Ephemeral scratch" }),
    "Attach the slugify workspace — real code, mounted writable, or nothing lands.",
  );
  await act(page, page.getByRole("option", { name: new RegExp(WORKSPACE_NAME, "i") }).first());

  // --- B4 Confinement -----------------------------------------------------
  //
  // "Confined" is already the screen's default (new-run-screen.tsx:167) — the
  // click is for the camera. These are REAL radios whose accessible name is
  // title + body ("Confined Default-deny. New hosts are held at the door…"), so
  // prefix-match the title. Note this is a DIFFERENT component from the
  // Add-workspace dialog's OptionCard, which is an aria-pressed <button>. Same
  // look, two roles: check the a11y snapshot rather than assuming.
  await act(
    page,
    page.getByRole("radio", { name: /^Confined/ }),
    "Confined means default-deny: nothing reaches the network unless we allow it.",
  );

  // --- B5 Network ---------------------------------------------------------
  await act(page, page.getByRole("button", { name: /Edit hosts/ }));
  const net = page.getByRole("dialog");
  await expect(net.getByText("Network for this run")).toBeVisible();
  await beat(page, PACE.read);

  // Host chips are aria-pressed TOGGLES, and the wizard SEEDS this one on
  // (initialWizardState's allowedDomains: ["api.anthropic.com"]). Clicking it
  // therefore REMOVES it — on camera the ✓ vanishes and the header ticks to
  // "0 hosts" while the narration says it is allowed. Point at it when it is
  // already on; only click when it is genuinely off.
  const modelChip = net.getByRole("button", { name: new RegExp(MODEL_HOST.replace(/\./g, "\\.")) }).first();
  const modelLine = `Only one host is allowed: ${MODEL_HOST}. Without it, no model.`;
  if ((await modelChip.getAttribute("aria-pressed")) === "true") {
    await caption(page, modelLine);
    await spotlight(page, modelChip);
    await beat(page, PACE.read);
    await spotlight(page, null);
  } else {
    await act(page, modelChip, modelLine);
  }
  // "Only one host" is a CLAIM, and the dialog counts for us: the section
  // header reads "Hosts this run can reach · N" (network-dialog.tsx). Assert
  // the N, so a seeded preset that quietly carries more never gets narrated as
  // one.
  await expect(net.getByRole("heading", { name: /Hosts this run can reach\s*·\s*1\b/ })).toBeVisible({
    timeout: 15_000,
  });

  // The unlisted-host rules ARE radios, but each card's accessible name
  // includes its explanatory body copy — prefix-match the title.
  await act(
    page,
    net.getByRole("radio", { name: /^Hold it for approval/ }),
    "Anything unlisted is held for approval — stopped live, never refused quietly.",
  );
  await beat(page, PACE.read);
  await act(page, net.getByRole("button", { name: "Save hosts" }));
  await expect(net).toBeHidden({ timeout: 30_000 });

  // --- B6 The rail is the contract ----------------------------------------
  const rail = page.locator("aside").filter({ hasText: "What this run can do" }).first();
  // The rail is what the beat spotlights, so what it says had better be right:
  // one host, and no warning banner over the credential line. The model warning
  // in particular ("No model provider is connected. This run launches; its first
  // model call fails.") would sit in frame for the whole beat and make a liar of
  // the next line — and of the entire run, which would launch and fail its first
  // model call.
  await expect(rail).toContainText("1 host allowed", { timeout: 20_000 });
  await expect(
    page.getByText(/No model provider is connected/),
    "no model provider is connected — V01's model step did not stick, and this run cannot do its task",
  ).toHaveCount(0);

  await spotlight(page, rail);
  await caption(page, "The rail is the contract — what this run can do, before it runs.");
  await beat(page, PACE.read);
  await caption(page, "Credentials are minted at launch, injected by the proxy, never written inside.");
  await beat(page, PACE.read + 600);
  await spotlight(page, null);
});

// ---------------------------------------------------------------------------
// Beats 7-9 — the run itself: unattended, held once, finished with a diff.
// ---------------------------------------------------------------------------

test("beats 7-9 — launch, held at the boundary, files changed", async () => {
  // Real agent, real sandbox, a human decision in the middle. Minutes.
  test.setTimeout(2_700_000);
  const page = stage();

  // --- B7 Launch, let it work ---------------------------------------------
  await act(page, page.getByRole("button", { name: "Launch run" }));
  await expect(page).toHaveURL(/\/runs\/[0-9a-f-]{8,}/i, { timeout: 60_000 });

  // This run is autonomous, so run-detail renders the static "Output" notice in
  // the hero tile, NOT an attachable terminal (run-detail.tsx's TerminalPane:
  // an autonomous run execs the agent directly and has no PTY to type into).
  // Claiming a live terminal here would narrate over a motionless card — assert
  // what is actually on screen so a future cockpit change cannot silently make
  // the next line false.
  const hero = page.getByTestId("run-terminal-pane");
  await expect(hero).toContainText("Output", { timeout: 60_000 });
  await caption(page, "Launch. This run is unattended, so there's no terminal to type into.");
  await beat(page, PACE.read + 1200);
  await caption(page, "The right rail is live: sandbox metering, egress, files as they change.");
  // The script's dead-air allowance for sandbox spin-up (~12 s measured). This
  // is pacing, not a wait on the app.
  await beat(page, 12_000);

  // --- B8 Held at the boundary --------------------------------------------
  //
  // The run's own policy said "Hold it for approval" (wait_for_review), and the
  // demo stack's session is the OPERATOR, so the ceiling clamp in
  // inline_policy.go:80 does not apply and the hold is genuine: the connection
  // is parked at the proxy with the row marked "waiting", not refused and
  // logged. Assert that BEFORE deciding — the whole beat says "held at the
  // proxy, waiting on me", and a deny_with_review fast-fail (which is what a
  // MEMBER session would get) looks almost identical on camera while making
  // both of those lines false.
  const heldRow = page.getByTestId("live-approval-row").filter({ hasText: HELD_HOST }).first();
  await expect(heldRow, `${HELD_HOST} never surfaced as a held request`).toContainText("waiting", {
    timeout: RUN_FINISHES,
  });

  await caption(page, `The agent just reached for ${HELD_HOST}. It's not on the list.`);
  await beat(page, PACE.read);
  await caption(page, "So it's held at the proxy, waiting on me. Nothing left the box.");
  await beat(page, PACE.read);

  // decide() takes the host BY NAME, never .first(). A real agent run raises
  // approvals nobody asked for — Claude Code reaches for its own telemetry
  // endpoint (http-intake.logs.us5.datadoghq.com) and under "Hold it for
  // approval" that surfaces as a pending row, often ABOVE the one this beat is
  // about. Approving something you did not mean to approve is the worst
  // possible frame in a governance demo. (That telemetry row is deliberately
  // left undecided and unnarrated here: the script gives this video no line for it.)
  //
  // The BARE Approve — the split button's plain click, which is "This run".
  // Not the caret: the scope ladder is V05's video, and an `always` here would
  // write example.com onto the workspace and kill V05's own hold beat.
  // decide() proves the decision landed by waiting for the row to clear, so a
  // rejected decision can never be narrated over.
  await decide(
    page,
    "Approve",
    "Approve, scoped to this run — allowed until the run ends, then it asks again.",
    HELD_HOST,
  );
  await beat(page, PACE.read + 900);

  // --- B9 Files changed ---------------------------------------------------
  //
  // Terminal state first, whichever it is, so a failed run fails HERE with the
  // badge on screen instead of timing out fifteen minutes later against a
  // "Completed"-only matcher. RunStateBadge renders TITLE CASE labels from
  // runStateMeta ("Completed", not "COMPLETED").
  const finished = page.getByText(/^(Completed|Failed|Stopped|Killed)$/).first();
  await expect(finished).toBeVisible({ timeout: RUN_FINISHES });
  await expect(finished, "the run did not complete — 'Done.' would be narrated over a failure badge").toHaveText(
    "Completed",
  );

  // The canvas swaps to the "finished" preset on its own the moment the run
  // terminates (canvas.tsx: `ctx.finished ? "finished" : "live"`), promoting
  // Files changed to the top-right. DO NOT RELOAD to get there: the widget
  // execs `git` INSIDE the sandbox (GET /runs/{id}/files), and a fresh mount
  // against a torn-down sandbox answers 409 and renders "This run has finished
  // — its sandbox is gone." Left alone, polling has already stopped (usePoll is
  // paused once live=false) and the last-good rows stay on screen.
  // The widget's own <section> (WidgetCard), reached from its heading rather
  // than by picking a section off the page — an ancestor-scoped hop cannot
  // accidentally resolve to a wrapper that happens to sort first.
  const files = page.getByRole("heading", { name: "Files changed" }).locator("xpath=ancestor::section[1]");
  await spotlight(page, files);
  await caption(page, "Done. Files changed lists exactly what it touched: source, test, and notes.");

  // ASSERT THE PAYOFF. The narration names three files; if the workspace mounted
  // read-only, or the agent stopped after the curl, the widget shows "No files
  // changed yet." and this line is a lie told over an empty box. All three are
  // guaranteed by DEMO_TASK (steps 1 and 4) against the demo-node fixture, which
  // ships src/slug.js and test/slug.test.js for the agent to MODIFY.
  await expect(files).toContainText("src/slug.js", { timeout: 60_000 });
  await expect(files).toContainText("test/slug.test.js");
  await expect(files).toContainText("NOTES.md");
  // …and the diffstat itself: "+N" proves real counted edits rather than a row
  // that rendered with absent counts.
  await expect(files).toContainText(/\+\d+/);
  await beat(page, PACE.read + 900);
  await spotlight(page, null);

  // "every boundary crossing left a receipt" — the Egress widget is where that
  // receipt is read, and example.com is the specific crossing this video just
  // decided on camera.
  const egress = page.getByRole("heading", { name: "Egress" }).locator("xpath=ancestor::section[1]");
  await spotlight(page, egress);
  await caption(page, "Real edits on my disk — and every boundary crossing left a receipt.");

  // ASSERTED FROM THE TRAIL, NOT FROM THE TILE. EgressWidget renders only the 8
  // NEWEST decisions (widgets/egress.tsx's MAX_ROWS) and a real Claude Code run
  // makes a fresh api.anthropic.com CONNECT on every turn — by the time step 4
  // finishes writing NOTES.md, the example.com rows have almost certainly rolled
  // out of that window. Asserting the widget's text would burn a twenty-minute
  // quota take on a cosmetic cap. The claim the line makes is that the crossing
  // left a receipt; /audit is where the receipt actually lives.
  const runId = page.url().split("/runs/")[1]?.split(/[?#]/)[0] ?? "";
  const trail = await page.request.get(
    `/api/v1/audit?run_id=${encodeURIComponent(runId)}&action=egress.allow`,
    { headers: process.env.WARDYN_DEMO_TOKEN ? { Authorization: `Bearer ${process.env.WARDYN_DEMO_TOKEN}` } : undefined },
  );
  expect(
    (await trail.text()).includes(HELD_HOST),
    `no egress.allow receipt for ${HELD_HOST} on run ${runId} — the approval on camera never reached the trail`,
  ).toBe(true);
  await beat(page, PACE.read + 900);
  await spotlight(page, null);

});

// ---------------------------------------------------------------------------
// Beat 10 — borrowed by name, never held (owner fold, 2026-08-17)
//
// Closes the loop video 02 opened: a secret a run BORROWS but never holds.
// The carrier is a small keyless background run, launched off camera against
// its own throwaway workspace whose one requirement names video 02's token —
// see the nouns block for why this must not ride the agent run above.
// ---------------------------------------------------------------------------

test("V05 beat 10 — borrowed, never held", async () => {
  test.setTimeout(300_000);
  const page = stage();
  const headers = process.env.WARDYN_DEMO_TOKEN
    ? { Authorization: `Bearer ${process.env.WARDYN_DEMO_TOKEN}` }
    : undefined;

  await caption(page, "One more claim from video two to close out: a secret a run borrows, but never holds.");
  await beat(page, PACE.read);

  // Launched via the API — the form was this series' videos three and five;
  // what this beat teaches is the WIRING, not the clicks.
  const mk = await page.request.post("/api/v1/runs", {
    headers,
    data: {
      agent: "claude-code",
      title: PROOF_TITLE,
      task: "echo wired by name, minted by the broker, never handed over",
      workspace_id: (await (async () => {
        const body = await (await page.request.get("/api/v1/workspaces", { headers })).json();
        const items: { id?: string; name?: string }[] = Array.isArray(body) ? body : (body?.items ?? []);
        return items.find((w) => w.name === PROOF_WS_NAME)?.id ?? "";
      })()),
      inline_policy: {
        allowed_domains: [],
        denied_domains: [],
        allow_all_egress: false,
        first_use_approval: "always_deny",
        allowed_methods: [],
        min_confinement_class: "CC1",
        eligible_grants: [],
      },
    },
  });
  expect(mk.ok(), `proof-run create failed (${mk.status()}): ${await mk.text().catch(() => "")}`).toBe(true);
  const proofRunId = ((await mk.json())?.id as string) ?? "";
  expect(proofRunId.length > 0, "proof-run create returned no id").toBe(true);

  await page.goto(`/runs/${proofRunId}`);
  await caption(page, "A background run, wired off camera to that token — by name, through its workspace.");
  await beat(page, PACE.read);
  await expect(page.getByText(/Running|Completed/).first()).toBeVisible({ timeout: 180_000 });

  // The Credentials widget: minted at sandbox startup, never on traffic — so
  // even this instantly-finished run earns its count.
  const credHeading = page.getByRole("heading", { name: "Credentials" }).first();
  await credHeading.scrollIntoViewIfNeeded().catch(() => {});
  await spotlight(page, credHeading);
  await expect(page.getByText(/1 eligible · 1 minted/).first()).toBeVisible({ timeout: 120_000 });
  await caption(page, "One credential eligible, one minted — a short-lived stand-in, made by the broker at start.");
  await beat(page, PACE.read);
  await spotlight(page, page.getByText(/never sees your real keys/).first());
  await caption(page, "The widget says it plainly: the agent never sees your real keys.");
  await beat(page, PACE.read + 400);
  await spotlight(page, null);

  // Both halves on the record (the retired workspaces-and-secrets spec's own
  // verified event pair): the workspace granted it, Wardyn read it.
  const reqEvents = await (
    await page.request.get(`/api/v1/audit?run_id=${encodeURIComponent(proofRunId)}&action=run.workspace.requirement.secret`, { headers })
  ).text();
  expect(
    reqEvents.includes(PROOF_SECRET),
    `no run.workspace.requirement.secret event for ${PROOF_SECRET} on ${proofRunId}`,
  ).toBe(true);
  await caption(page, "Both halves are on the record: the workspace granted it, and Wardyn read it —");
  await beat(page, PACE.read);
  await caption(page, "read by the run's identity, at start. The value crossed no screen and no shell.");
  await beat(page, PACE.read);

  // The deterministic negative: video two's canary value — the secret's actual
  // content — appears NOWHERE. Not on this page, not in the run's audit trail.
  await expect(page.locator("body")).not.toContainText(PROOF_CANARY);
  const fullTrail = await (
    await page.request.get(`/api/v1/audit?run_id=${encodeURIComponent(proofRunId)}`, { headers })
  ).text();
  expect(fullTrail.includes(PROOF_CANARY), "the canary VALUE leaked into the audit trail").toBe(false);
  await caption(page, "And the value itself — video two's canary — appears nowhere. That is the whole design.");
  await beat(page, PACE.read + 600);

  // --- OUTRO --------------------------------------------------------------
  await caption(page, "That's a first agent run: confined, held once, finished with a diff — and secrets only ever borrowed.");
  await beat(page, PACE.read + 600);
  await caption(page, "Next: stop writing the policy at all — record a run, and let it write itself.");
  await beat(page, PACE.chapter);
  await caption(page, "");
});
