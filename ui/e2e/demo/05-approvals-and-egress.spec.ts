/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

/*
 * Video 5 of the 0.5 series — Approvals & egress scopes.
 *
 * WHAT THIS FILMS. One question, answered four ways: an agent reaches for a
 * host nobody allow-listed — who decides, and for how long? The take opens on
 * a throwaway demo sandbox (the "Held at the door" card on /demos), where a
 * curl HANGS at the proxy while a human decides; a plain Approve lets that
 * same in-flight request finish with no retry. Then a second, unwanted host
 * shows the whole scope ladder — Once / This run / Until… / Always — and gets
 * denied. Then the payoff the demo sandbox structurally CANNOT show: the same
 * refusal on a REAL workspace, decided with `always`, landing as a permanent
 * row on that workspace's own Allowed hosts list, and a brand-new run that
 * reaches the host with nothing to click.
 *
 * WHY /demos AND NOT THE FUNNEL. The same five demos are also embedded in the
 * Getting Started funnel one-per-step, and video 1 drives them there. This
 * video drives the CATALOG page instead: it is the surface that does not move
 * when the funnel's step structure changes, it renders every card at once (so
 * `demo-card-<id>` is a real scope), and it needs no wizard state to reach.
 *
 * STATE IT INHERITS. Whatever the earlier videos left — this is a keyless
 * video and it resets nothing (record-demo.sh only resets for video 01). What
 * it does NOT tolerate is its OWN leftovers: a previous take's `always`
 * decision sitting in egress-lab's approved_egress would mean beat 6's run is
 * never refused at all, and the entire back half of the video films a host
 * that was already allowed while the narrator says "refused". So beforeAll
 * deletes and re-onboards the workspace, off camera, every take (DA5).
 *
 * OWNERSHIP OF NOUNS. This video owns the workspace `egress-lab` and the two
 * hosts crates.io and ingest.sentry.io, and touches nothing else. Video 2 owns
 * `slugify` and example.com; sharing either would let one video's permanent
 * grant silently disarm the other's hold beat. Nothing here types
 * example.com — not even the example.com pill the demo card offers.
 *
 * This is NOT a test. It asserts only enough to keep itself honest and to know
 * when to advance; a failure here means the recording is wrong, not that the
 * product is broken. It runs against the REAL compose stack on :8080 with real
 * sandboxes — the hermetic `-runner none` e2e backend cannot start one at all.
 *
 * Selectors are getByRole + accessible name, matching the rest of the suite:
 * every literal below exists in ui/src today.
 */

import { mkdirSync } from "node:fs";
import { test, expect } from "@playwright/test";
import { act, beat, caption, PACE, spotlight, typeInTerminal } from "./overlay";
// stage.ts is the rig: importing it registers this file's beforeAll/afterAll
// (one browser, one context, one recorded page), and every beat reads the page
// out of stage() inside a test body rather than closing over a module binding.
import { stage } from "./stage";
import { APPROVAL_APPEARS, decide } from "./funnel";

test.skip(!process.env.WARDYN_DEMO, "demo recording — run via `make record-demo` (exports WARDYN_DEMO=1)");

test.describe.configure({ mode: "serial" });

// ---------------------------------------------------------------------------
// This video's nouns. Deliberately LOCAL, not in task.ts: nothing else in the
// series uses egress-lab or these two hosts, and a shared constant is how two
// videos end up fighting over one workspace's permanent grants.
// ---------------------------------------------------------------------------

/** The workspace beats 6-8 attach. Ours alone; recreated every take. */
const WORKSPACE = "egress-lab";

/**
 * Its host directory. record-demo.sh materializes only the slugify fixture, so
 * this one is created here — an empty throwaway dir is all beats 6-8 need (they
 * run curl, not code), and creating it in-process removes a staging step that
 * would otherwise be prose in a script nobody reads on shoot day.
 */
const WORKSPACE_PATH =
  process.env.WARDYN_DEMO_EGRESS_WORKSPACE || `${process.env.HOME}/wardyn-demo/egress-lab`;

/** The host the whole video is about: held, approved, then permanently granted. */
const HELD_HOST = "crates.io";

/** The host nobody asked for — an agent's own telemetry endpoint. Denied. */
const TELEMETRY_HOST = "ingest.sentry.io";

// --max-time 60 has to outlast the proxy's ~30s wait_for_review hold
// (defaultHoldTimeout, internal/egress/proxy/approvals.go) so the on-camera
// approval lands while the request is still parked and the SAME curl completes.
// Beats 6 and 8 reuse the exact string on purpose — "same command" is a line
// the narrator says out loud.
const REACH_HELD = `curl -sSI --max-time 60 https://${HELD_HOST}`;
const REACH_TELEMETRY = `curl -sSI --max-time 60 https://${TELEMETRY_HOST}`;

/** Beat 6's run title. Beat 8's is separate so the board shows two rows. */
const ALWAYS_RUN_TITLE = "Reach one new host";
/** Beat 8's run title — not spoken; the Title field is simply required. */
const PROOF_RUN_TITLE = "Same host, no approval";

// Real containers, so this is minutes. A ceiling for waiting on the PRODUCT —
// the pacing the viewer sees comes from overlay.ts, never from here.
const SANDBOX_UP = 180_000;

/**
 * A response actually came back, rather than the proxy refusing the tunnel.
 * NOT a bare /HTTP/ match: the proxy's own refusal renders as `HTTP/1.1 403`
 * in curl -sSI output, which is precisely the frame this assertion exists to
 * refuse to narrate over. 2xx/3xx only.
 */
const RESPONDED = /HTTP\/[\d.]+ [23]\d\d/;

// ---------------------------------------------------------------------------
// Staging — the part that must not be prose.
//
// OFF-CAMERA PRECONDITIONS THE OPERATOR STILL OWNS (nothing below can encode
// these; check them before the take rolls):
//   1. The compose stack is up on :8080 and a barrier is READY — the demo
//      cards' Start button is disabled without one, and this spec fails loudly
//      on that rather than clicking a dead button for 45s.
//   2. The stack has NOT been reset since the earlier videos (record-demo.sh
//      --video 05 already defaults DO_RESET=0; never pass --reset here).
//   3. Both hosts are reachable from this machine's egress path — crates.io
//      must answer a HEAD with a 2xx/3xx, or beat 2's payoff assertion fails.
//   4. No model needed. This video is keyless end to end.
// ---------------------------------------------------------------------------

function apiHeaders(): Record<string, string> | undefined {
  // Local mode (the series' posture, SV1) has no auth at all; the token lane
  // exists for a stack booted with one. Same shape funnel.ts's clearWorkspace
  // uses.
  return process.env.WARDYN_DEMO_TOKEN
    ? { Authorization: `Bearer ${process.env.WARDYN_DEMO_TOKEN}` }
    : undefined;
}

/**
 * Delete and re-onboard `egress-lab`, off camera, before the first beat.
 *
 * DELETE, not "clear the two lists": a delete drops approved_egress AND
 * denied_egress AND the requirements contract AND any record_results in one
 * call, so beat 7's "Allowed hosts · 1" is a count this take earned rather
 * than one inherited from the last one. The workspace has no active-run
 * interlock (handleDeleteWorkspace is unconditional), so this is safe to run
 * against a stack that still has an old egress-lab run on it.
 *
 * It is also why this video can be re-taken immediately: the second take is
 * byte-identical to the first.
 */
async function restageWorkspace(): Promise<void> {
  const page = stage();
  const headers = apiHeaders();

  mkdirSync(WORKSPACE_PATH, { recursive: true });

  const res = await page.request.get("/api/v1/workspaces", { headers });
  expect(res.ok(), `GET /api/v1/workspaces failed (${res.status()}) — is the stack up on :8080?`).toBe(
    true,
  );
  const body = await res.json();
  const items: { id?: string; name?: string }[] = Array.isArray(body)
    ? body
    : (body?.items ?? body?.workspaces ?? []);
  for (const w of items) {
    if (w?.id && w.name === WORKSPACE) {
      await page.request.delete(`/api/v1/workspaces/${w.id}`, { headers });
    }
  }

  // Legacy scalar shape (kind/source), folded server-side into one local_dir
  // source — the smallest body that onboards a directory. Read-only: beats 6-8
  // run curl, and a demo that grants write access it never uses is a demo
  // teaching the wrong habit.
  const created = await page.request.post("/api/v1/workspaces", {
    headers,
    data: { name: WORKSPACE, kind: "local_dir", source: WORKSPACE_PATH },
  });
  expect(
    created.ok(),
    `could not onboard ${WORKSPACE} at ${WORKSPACE_PATH} (${created.status()}: ${await created.text()})`,
  ).toBe(true);

  // The precondition the whole back half rests on, stated as an assertion
  // rather than a hope: a fresh workspace vouches for NOTHING, so beat 6's
  // curl is genuinely refused.
  const fresh = await created.json();
  expect(fresh.approved_egress ?? [], "a fresh egress-lab must allow no hosts").toEqual([]);
  expect(fresh.denied_egress ?? [], "a fresh egress-lab must deny no hosts").toEqual([]);
}

// Registered AFTER stage.ts's own beforeAll (import order), so stage() is
// assigned by the time this runs. Re-guards on WARDYN_DEMO because this hook
// DELETES a workspace: a file-level test.skip must never be the only thing
// standing between a developer's stack and that.
test.beforeAll(async () => {
  if (!process.env.WARDYN_DEMO) return;
  await restageWorkspace();
});

// ---------------------------------------------------------------------------
// Beats 0-5 — the demo sandbox: held, approved, the ladder, denied
// ---------------------------------------------------------------------------

test("beats 0-5 — held at the door, and the scope ladder", async () => {
  test.setTimeout(900_000);
  const page = stage();
  await page.goto("/demos");
  await page.bringToFront();

  // Fail here rather than several minutes into a silent, caption-less take:
  // every narration call degrades to a no-op by design, so nothing downstream
  // would ever complain about an overlay that failed to install.
  await expect
    .poll(() => page.evaluate(() => typeof (window as unknown as Record<string, unknown>).__demo), {
      timeout: 15_000,
    })
    .toBe("object");

  // ---- B0 · Start ---------------------------------------------------------
  // The catalog stacks every demo on one page, so `demo-card-<id>` is a real
  // scope here (it is NOT on the funnel path, where each step renders the bare
  // DemoRunControls — see funnel.ts's note). Everything below is scoped to
  // this card: the page holds six other terminals' worth of controls.
  const card = page.getByTestId("demo-card-held-at-the-door");
  await expect(card.getByRole("heading", { name: "Held at the door" })).toBeVisible({
    timeout: 60_000,
  });

  await spotlight(page, card);
  await caption(page, "An agent reaches for a host nobody allow-listed. Who decides, and for how long?");
  await beat(page, PACE.read);
  await spotlight(page, null);

  const startDemo = card.getByTestId("demo-start-held-at-the-door");
  // The barrier gate, named. Without a ready barrier this button is disabled
  // and act() would simply park on it for the full 45s action timeout, which
  // on camera is indistinguishable from the app hanging.
  await expect(startDemo, "the demo Start button is disabled — this stack has no ready barrier").toBeEnabled(
    { timeout: 60_000 },
  );
  await act(page, startDemo, "Wardyn stops it at the proxy and asks. Every answer carries a scope.");

  // Sandbox spin-up is ~12s of real dead air; the cold open above plays over it.
  await expect(card.locator(".xterm-screen").first()).toBeVisible({ timeout: SANDBOX_UP });
  // .xterm-screen renders when AttachTerminal MOUNTS, before the PTY websocket
  // is up — typing here eats the first characters and the shell reports
  // "command not found" on camera.
  await beat(page, PACE.read);

  // ---- B1 · Held ----------------------------------------------------------
  await typeInTerminal(page, REACH_HELD, card);
  await caption(page, "The command is hanging. Nothing failed — the connection is parked at the proxy.");

  // The strip's header is the wait_for_review flavour (isHeld → anyHeld), which
  // is the entire difference between this demo and "Fail, then approve". If it
  // says "Approval needed — off-policy egress" instead, the sandbox is NOT
  // holding anything and the narration above is a lie.
  await expect(card.getByText("Sandbox is waiting — approve to let it through")).toBeVisible({
    timeout: APPROVAL_APPEARS,
  });
  const heldRow = card.getByTestId("live-approval-row").filter({ hasText: HELD_HOST });
  await expect(heldRow).toBeVisible({ timeout: APPROVAL_APPEARS });
  await expect(heldRow).toContainText("waiting");
  // LET THE LINE FINISH. caption() only SCHEDULES narration (narrator.ts lays
  // each cue on the timeline at the moment it was spoken); it is beat()/act()
  // that hold for it. Without this the next caption starts ~2s in and the mux
  // plays both lines over each other. The floor is subsumed by the residual
  // speech, so the cost against the proxy's ~30s hold is ~3s, not 2.2+speech.
  await beat(page, PACE.read);
  await spotlight(page, heldRow);
  await caption(page, "Nothing left the box — it is held, waiting on a human.");
  await beat(page, PACE.read);
  await spotlight(page, null);

  // ---- B2 · Default scope -------------------------------------------------
  // The bare split-button click — "This run", today's default scope. decide()
  // decides a NAMED host, never .first(): approving something you did not mean
  // to approve is the worst possible frame in a governance video.
  await decide(card, "Approve", "Plain Approve is one scope: This run.", HELD_HOST);

  // THE PAYOFF. The narration claims the same in-flight request finishes with
  // no retry; without this assertion a take stays green while narrating success
  // over a terminal showing a 403 and a fresh pending row.
  await expect(card.locator(".xterm-screen").first()).toContainText(RESPONDED, { timeout: 60_000 });
  await caption(page, "The same in-flight command completes. No retry — it was never refused.");
  await beat(page, PACE.read + 600);

  // ---- B3 · Unwanted host -------------------------------------------------
  await typeInTerminal(page, REACH_TELEMETRY, card);
  await caption(page, "A second host: the agent's own telemetry. Nobody asked for it.");
  const telemetryRow = card.getByTestId("live-approval-row").filter({ hasText: TELEMETRY_HOST });
  await expect(telemetryRow).toBeVisible({ timeout: APPROVAL_APPEARS });
  await beat(page, PACE.read);

  // ---- B4 · The scope ladder ----------------------------------------------
  // A pure LOOK — nothing is decided here, the row stays pending for beat 5.
  //
  // Both carets on the row carry the same aria-label ("More options") by
  // design: ScopeMenu's trigger may never say approve/deny, or an unanchored
  // /approve/i query elsewhere in the suite would match two buttons. DOM order
  // is Approve, Approve-caret, Deny, Deny-caret — .first() is Approve's, the
  // same disambiguation e2e/approvals.spec.ts and funnel.ts's decide() use.
  await act(
    page,
    telemetryRow.getByRole("button", { name: "More options" }).first(),
    "The caret opens the other three.",
  );
  // Radix portals the menu content, so it is NOT a descendant of the row — and
  // scoping to the open menu (rather than the page) keeps /^Once/ from also
  // matching the "Once, or for good" card sitting further down this catalog.
  const menu = page.getByRole("menu");
  await expect(menu).toBeVisible();

  await spotlight(page, menu.getByRole("button", { name: /^Once\b/ }));
  await caption(page, "Once: this one connection. The next attempt asks you again.");
  await beat(page, PACE.read);

  await spotlight(page, menu.getByRole("button", { name: /^This run\b/ }));
  await caption(page, "This run: every attempt until the run ends. That is the default.");
  await beat(page, PACE.read);

  // "Until…" swaps the menu for its preset list (untilMode) rather than
  // deciding — the four presets and a datetime picker, with "← Back" out.
  await act(
    page,
    menu.getByRole("button", { name: /^Until…/ }),
    "Until is time-boxed — fifteen minutes, an hour, or a time you pick.",
  );
  await expect(menu.getByRole("button", { name: "15 minutes" })).toBeVisible();
  await beat(page, PACE.read + 600);
  await act(page, menu.getByRole("button", { name: "← Back" }));

  const alwaysOption = menu.getByRole("button", { name: /^Always\b/ });
  await spotlight(page, alwaysOption);
  await caption(page, "Always saves it to the workspace, so future runs start with it.");
  await beat(page, PACE.read);
  await caption(page, "Greyed out here — a demo sandbox has no workspace.");
  // THE PAYOFF of this beat, and the setup for beat 6's contrast. A REAL
  // disabled attribute, not aria-disabled (ScopeMenu renders plain <button>s
  // precisely so this is true) — and the hint that replaces the normal one is
  // the sentence the narrator just spoke.
  await expect(alwaysOption).toBeDisabled();
  await expect(menu.getByText("Always needs a workspace — this run isn't attached to one.")).toBeVisible();
  await beat(page, PACE.read);
  await spotlight(page, null);

  // Close without picking: this approval has to survive into beat 5.
  await page.keyboard.press("Escape");
  await expect(page.getByRole("menu")).toHaveCount(0);

  // ---- B5 · Deny ----------------------------------------------------------
  // The ~30s hold may well have expired during the ladder walk — the request
  // 403s and the row STAYS pending and decidable, which is why this beat is
  // written as "deny a pending row", not "deny a held one".
  //
  // decide() clicks the bare Deny, confirms in the alertdialog (Deny always
  // confirms, whatever the scope), and asserts the row is GONE afterwards.
  await decide(card, "Deny", "Telemetry gets nothing. Deny always confirms first.", TELEMETRY_HOST);

  await caption(page, "Denied. Who decided, and at what scope, lands in the audit trail.");
  const auditPanel = card.getByTestId("demo-audit-panel");
  await spotlight(page, auditPanel);
  // The claim, asserted: a deny row for THIS host, not merely the pending row
  // that was already there. The panel polls every 2s.
  await expect(
    auditPanel
      .getByTestId("demo-audit-rows")
      .locator("li")
      .filter({ hasText: TELEMETRY_HOST })
      .filter({ hasText: "deny" })
      .first(),
  ).toBeVisible({ timeout: 60_000 });
  await beat(page, PACE.read + 600);
  await spotlight(page, null);

  // End the sandbox before moving on — a demo killed mid-flight can lose its
  // last decision on the way to the audit log.
  await act(page, card.getByRole("button", { name: "End demo" }));
  await expect(card.getByTestId("demo-terminated")).toBeVisible({ timeout: 60_000 });
});

// ---------------------------------------------------------------------------
// Beats 6-7 — Always, on a real workspace, and the receipt
// ---------------------------------------------------------------------------

test("beats 6-7 — Always, and the workspace's own Allowed hosts", async () => {
  test.setTimeout(900_000);
  const page = stage();

  // ---- B6 · Always, real workspace ---------------------------------------
  // "New run" lives in the app shell's top bar, so it is reachable from /demos
  // without a detour through the Runs board.
  await act(page, page.getByRole("button", { name: "New run" }));
  await expect(page).toHaveURL(/\/runs\/new/, { timeout: 30_000 });

  const titleBox = page.getByLabel("Title");
  await spotlight(page, titleBox);
  await titleBox.fill(ALWAYS_RUN_TITLE);
  await spotlight(page, null);

  // Terminal, not the agent: this video is keyless, and a shell is all a curl
  // needs. The wizard's own default is an INTERACTIVE agent run, so the "Start
  // with" segment is already on screen — this click only swaps which of its two
  // options is chosen. (Real radios: role=radio buttons inside a radiogroup,
  // accessible name = the full option label, so prefix-match it.)
  //
  // Deliberately UNNARRATED, along with the launch and the boot below: this
  // beat's two spoken lines both describe the refusal, and the script's silence
  // ledger pays for the form-filling. Speaking "Refused" over a form that has
  // not been submitted yet is the exact dishonesty this driver exists to avoid.
  await act(page, page.getByRole("radio", { name: /^Terminal/ }));

  // The workspace trigger has NO accessible name (the Agent select beside it is
  // labelled, this one was never wired up), so it is addressed by its
  // placeholder text — an honest workaround for a real a11y gap, not a test
  // convenience.
  await act(page, page.getByRole("combobox").filter({ hasText: "Ephemeral scratch" }));
  await act(page, page.getByRole("option", { name: new RegExp(WORKSPACE, "i") }).first());

  // EVERYTHING ELSE IS LEFT ALONE, and that is the beat: Confined, the model
  // host allow-listed, unlisted hosts on "Deny, but ask" (deny_with_review) —
  // the shipped defaults. So crates.io fails FAST here rather than hanging,
  // which is a visible difference from the demo sandbox above and exactly what
  // the narration says ("Refused — and it raised an approval").
  await act(page, page.getByRole("button", { name: "Launch run" }));
  await expect(page).toHaveURL(/\/runs\/[0-9a-f-]{8,}/i, { timeout: 60_000 });

  await expect(page.locator(".xterm-screen").first()).toBeVisible({ timeout: SANDBOX_UP });
  await beat(page, PACE.read);
  await typeInTerminal(page, REACH_HELD);

  // The passive-pending header, NOT the held one — the run's own policy says
  // deny-but-ask, so nothing is parked waiting. Asserting the exact header is
  // what makes the narration's "Refused" honest: the held flavour would mean
  // the request is still open, which is a different sentence entirely.
  await expect(page.getByText("Approval needed — off-policy egress")).toBeVisible({
    timeout: APPROVAL_APPEARS,
  });
  await caption(page, "Real workspace now, and the default ask policy. Refused — and it raised an approval.");
  await beat(page, PACE.read);

  // `always` is the whole point of the video. It is only clickable because THIS
  // run resolves to a workspace (run-detail passes hasWorkspace=runHasWorkspace
  // (run)); were it still disabled, this click would time out loudly rather
  // than narrate over a greyed option. decide() then asserts the row cleared —
  // a 400 from the scope rules would otherwise leave it pending while the
  // narrator says "Always".
  await decide(
    page,
    "Approve",
    "This time, Always. And where allow and deny collide, deny wins.",
    HELD_HOST,
    "always",
  );
  await beat(page, PACE.read + 600);

  // ---- B7 · Receipt -------------------------------------------------------
  await act(page, page.getByRole("link", { name: "Workspaces" }));
  await act(page, page.getByRole("row", { name: new RegExp(WORKSPACE, "i") }));
  await expect(page.getByRole("heading", { name: WORKSPACE, level: 1 })).toBeVisible({ timeout: 30_000 });

  // THE PAYOFF, and the reason beforeAll deletes this workspace: the count is
  // exactly one, the host is the one that was held, and its provenance names
  // where it came from. A stale grant from the last take would read "· 2" here
  // and the receipt would be someone else's.
  const allowed = page.getByText("Allowed hosts · 1");
  await expect(allowed).toBeVisible({ timeout: 30_000 });
  await spotlight(page, allowed);
  await caption(page, "The receipt: crates.io is on the workspace's own Allowed hosts list now.");
  await expect(page.getByText(HELD_HOST, { exact: true }).first()).toBeVisible();
  await expect(page.getByText("approved for this workspace")).toBeVisible();
  await beat(page, PACE.read + 900);
  await spotlight(page, null);
});

// ---------------------------------------------------------------------------
// Beat 8 — the run that never has to ask
// ---------------------------------------------------------------------------

test("beat 8 — a new run, and nothing to click", async () => {
  test.setTimeout(600_000);
  const page = stage();

  // Launched from the workspace page we are standing on — same top-bar button.
  await act(page, page.getByRole("button", { name: "New run" }));
  await expect(page).toHaveURL(/\/runs\/new/, { timeout: 30_000 });

  const titleBox = page.getByLabel("Title");
  await spotlight(page, titleBox);
  await titleBox.fill(PROOF_RUN_TITLE);
  await spotlight(page, null);

  await act(page, page.getByRole("radio", { name: /^Terminal/ }));
  await act(page, page.getByRole("combobox").filter({ hasText: "Ephemeral scratch" }));
  await act(page, page.getByRole("option", { name: new RegExp(WORKSPACE, "i") }).first());

  // Network is DELIBERATELY untouched — that omission IS the beat. The
  // workspace's approved_egress is unioned into this run's allowlist
  // server-side (unionWorkspaceEgress), so there is nothing to configure.
  await act(page, page.getByRole("button", { name: "Launch run" }));
  await expect(page).toHaveURL(/\/runs\/[0-9a-f-]{8,}/i, { timeout: 60_000 });

  await expect(page.locator(".xterm-screen").first()).toBeVisible({ timeout: SANDBOX_UP });
  await beat(page, PACE.read);
  await typeInTerminal(page, REACH_HELD);
  await caption(page, "New run, same workspace, same command. Nothing to click.");

  // BOTH halves of the claim, asserted. The response proves it got through;
  // the idle hint proves nothing was raised to decide — a pending row here
  // would mean the permanent grant never reached this run's policy, and the
  // outro's "the decision outlived the run that raised it" would be false.
  await expect(page.locator(".xterm-screen").first()).toContainText(RESPONDED, { timeout: 60_000 });
  await expect(page.getByTestId("live-approvals-idle")).toBeVisible();
  await beat(page, PACE.read + 600);
  await caption(page, "Governance that remembers: the decision outlived the run that raised it.");
  await beat(page, PACE.read + 900);

  // ---- Outro --------------------------------------------------------------
  await caption(page, "Once, this run, until, always. You choose the blast radius.");
  await beat(page, PACE.read + 600);
  await caption(page, "Next: policies — deciding all of this before the agent ever starts.");
  await beat(page, PACE.chapter);
  await caption(page, "");
});
