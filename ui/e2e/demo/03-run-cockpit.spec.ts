/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

/*
 * V03 — The run cockpit.
 *
 * The third video of the 0.5 series. It films ONE screen: /runs/:id, the
 * terminal-first cockpit. Five beats — the terminal is the run, the header is
 * identity, take-over, the widget canvas, focus mode — and the whole argument
 * is that a Wardyn run is a live session you supervise, not a log you read
 * afterwards.
 *
 * This is NOT a test. It asserts only enough to keep itself honest and to know
 * when to advance; a failure here means the recording is wrong, not that the
 * product is broken. It runs against the REAL compose stack on :8080, with a
 * real sandbox and a real attach WebSocket — nothing here works against the
 * hermetic `-runner none` backend.
 *
 * WHAT THIS VIDEO INHERITS (SV16 / SV20, the one-run ruling)
 * ---------------------------------------------------------
 * V03 does NOT launch anything. It reuses the interactive agent run V08 leaves
 * behind, which is why every take in the V8 → V2 → V3 window runs
 * `scripts/record-demo.sh --no-reset` — the harness default (DO_RESET=1) would
 * `reset-all` the stack and destroy the very run this spec opens (and churn the
 * SSH host key V10 needs). Hygiene inside the window is workspace-scoped
 * clearing only; `reset-all` belongs to video 01 alone.
 *
 * THE TWO-CLIENT STAGING (DA13) — the part a human has to do
 * ---------------------------------------------------------
 * Beat 3 is a take-over, and you cannot take over from yourself. First attach
 * wins the holder (internal/api/attach_holder.go), so an OFF-CAMERA client must
 * be attached to this run BEFORE this spec opens the cockpit; the presenter then
 * arrives as the second client and is admitted READ-ONLY, which is the state the
 * beat is about. The spec drives the presenter only. It cannot stage the holder
 * — but it does REFUSE TO ROLL without one: beat 1 asserts
 * GET /runs/{id}/attach-holder says `held: true`, so a forgotten holder fails in
 * the first ten seconds instead of at 1:15 with the narrator explaining a
 * read-only badge that is not on screen.
 *
 * The full operator checklist is in the OPERATOR PRECONDITIONS block below.
 *
 * Selectors are getByRole + accessible names, matching the rest of the suite.
 * Every literal string targeted here exists in ui/src today — the cockpit copy
 * is centralised in ui/src/app/components/wardyn/copy.ts's RUN_COCKPIT, and the
 * COPY table below mirrors the handful this file needs so a copy change breaks
 * the recording loudly and in one place rather than silently mis-narrating.
 */

import { expect, test, type Locator, type Page } from "@playwright/test";
import { act, beat, caption, PACE, spotlight, typeInTerminal } from "./overlay";
// stage.ts is the browser rig: importing it registers this file's
// beforeAll/afterAll, and each beat reads the page out of stage() inside its own
// test body rather than closing over a module-level `let`.
import { stage } from "./stage";

test.skip(!process.env.WARDYN_DEMO, "demo recording — run via `make record-demo` (exports WARDYN_DEMO=1)");

test.describe.configure({ mode: "serial" });

/*
 * OPERATOR PRECONDITIONS — set these up BEFORE the take rolls.
 *
 *  1. V08 has already shot, and its INTERACTIVE agent run is still RUNNING.
 *     Its policy must carry no auto_stop shorter than the gap between the two
 *     takes, or the cockpit opens on a corpse.
 *  2. Every V08 cockpit TAB IS CLOSED. A browser tab left open holds the PTY,
 *     and the off-camera holder in (4) would then arrive second — the exact
 *     inversion this video cannot survive.
 *  3. `clear` was typed in that session before detaching. Beat 3 says "same
 *     session, same scrollback"; V08's sentinel commands must not be what the
 *     viewer is looking at while it says so.
 *  4. An OFF-CAMERA client is attached to that run RIGHT NOW — a second browser
 *     window on /runs/<id>, or `wardyn attach <id>` over the SSH gateway. It
 *     must attach BEFORE this spec starts. It is displaced on camera at 1:15
 *     and does not come back (a taken-over client deliberately does not
 *     reconnect — see attachTakeoverReasonPrefix).
 *  5. Exactly ONE live interactive run exists on the stack. Beat 1 opens the
 *     board's single "Attach" button; two of them is an ambiguous take, and the
 *     spec fails rather than guessing.
 *  6. `scripts/record-demo.sh --video 03 --no-reset` (SV20). Never without
 *     --no-reset.
 *
 * Quota: this video makes NO model call, so it needs no quota of its own — but
 * V08 does, and V03 is downstream of it. Probe before the V08 take, not this one.
 *
 * WARDYN_DEMO_NO_AGENT (the script's fallback for a spent quota) costs this
 * video nothing: the spec picks "the newest RUNNING interactive run" and never
 * asserts an agent, so a bare Terminal run films every beat here identically —
 * it drops the framing, it does not fake anything. Only the "CC" agent badge in
 * beat 2 changes.
 */

// Timeouts are for waiting on the PRODUCT — a real container, a real WebSocket,
// a real tmux take-over. The pacing the viewer sees comes from overlay.ts, and
// the two are never the same number.
const ATTACH_CONNECTS = 120_000;
const TAKEOVER_RECONNECTS = 90_000;

// The cockpit copy this file targets, mirrored from
// ui/src/app/components/wardyn/copy.ts's RUN_COCKPIT. Verbatim: a drifted string
// here is a caption narrating something the frame does not say.
const COPY = {
  watchingReadOnly: "watching read-only — keystrokes go nowhere",
  heldHint:
    "Someone else is driving this session. You can watch it live, or take it from them — they get told, and it lands in the audit trail.",
  takeOver: "Take over",
  driving: "you are driving",
  editing: "Editing layout",
  addWidget: "Add widget",
  editLayout: "Edit layout",
  resetLayout: "Reset to default",
  saveLayoutDefault: "Save as my default",
  doneEditing: "Done",
  layoutSaved: "Saved as your default layout",
  enterFocus: "Focus",
  exitFocus: "Exit focus",
  dock: "Widget dock",
  shortcuts: "⌘\\ dock · Esc exit focus",
} as const;

// The two widgets beat 4 rearranges. Both are ALREADY PLACED by the `live`
// preset (widget-registry.ts: egress at 8,0 and files at 8,4), so beat 4 moves
// and resizes them — it never adds them. That is why the Add-widget catalog is
// opened and closed rather than used: on a live run it is a reveal, not an add
// (the script's own biggest-choreography-gap note).
const EGRESS_WIDGET = "Egress";
const FILES_WIDGET = "Files changed";

// The run this video opens, resolved off camera in beat 1 and reused by the rest.
let runId = "";
let runRepo = "";

// ---------------------------------------------------------------------------
// Off-camera helpers
// ---------------------------------------------------------------------------

function demoHeaders(): Record<string, string> | undefined {
  // The default containerized install is local-mode with no auth at all, so this
  // is inert there — same rule as funnel.ts's clearWorkspace.
  return process.env.WARDYN_DEMO_TOKEN
    ? { Authorization: `Bearer ${process.env.WARDYN_DEMO_TOKEN}` }
    : undefined;
}

async function apiJson<T>(page: Page, path: string): Promise<T | null> {
  const res = await page.request.get(path, { headers: demoHeaders() }).catch(() => null);
  if (!res?.ok()) return null;
  return (await res.json().catch(() => null)) as T | null;
}

type WireRun = {
  id: string;
  repo?: string;
  state?: string;
  interactive?: boolean;
  created_at?: string;
};

/**
 * The run this video is about: the live interactive one V08 left behind.
 *
 * Deliberately NOT a hard-coded title. V08 does not name its run in the script,
 * and an interactive run carries no task at all (the server ignores task for
 * one), so `runHeadline` can legitimately render "—" for it — matching on the
 * board's text would be matching on an em dash. The wire fields are the honest
 * key: RUNNING + interactive is exactly "the one with an Attach button".
 */
async function liveInteractiveRun(page: Page): Promise<WireRun> {
  const body = await apiJson<unknown>(page, "/api/v1/runs");
  const items: WireRun[] = Array.isArray(body)
    ? body
    : (((body as Record<string, unknown> | null)?.items as WireRun[]) ?? []);
  const live = items.filter((r) => r?.state === "RUNNING" && r?.interactive);
  // ONE, not `.first()`. Two live interactive runs means two "Attach" buttons on
  // the board and no way for this spec to know which one the holder is sitting
  // on — a take that films the wrong run, read-only badge absent, at 1:15.
  expect(
    live.length,
    "V03 needs EXACTLY ONE live interactive run (the one V08 left). Kill the others, or shoot V08 again.",
  ).toBe(1);
  return live[0];
}

/**
 * Ring a target without ever failing the take over it.
 *
 * spotlight() calls boundingBox(), which THROWS on a target that is not there —
 * and beat 2 is a seven-stop cosmetic tour of one header row. Anything this
 * video actually CLAIMS is asserted separately, immediately before the glance,
 * so tolerating a missing ring here loses a ring, never a proof.
 */
async function glance(page: Page, target: Locator, ms = 900): Promise<void> {
  await spotlight(page, target).catch(() => {});
  await page.waitForTimeout(ms);
}

/** Press-drag-release, slowly enough that the camera sees the tile travel. */
async function dragBy(page: Page, handle: Locator, dx: number, dy: number): Promise<void> {
  const box = await handle.boundingBox();
  if (!box) throw new Error("drag target has no box — the canvas is not in edit mode");
  const x = box.x + box.width / 2;
  const y = box.y + box.height / 2;
  await page.mouse.move(x, y);
  await page.mouse.down();
  // Stepped, not a single jump: react-grid-layout only paints its snap ghost on
  // intermediate moves, and the ghost tracking the tile IS the beat.
  const steps = 14;
  for (let i = 1; i <= steps; i++) {
    await page.mouse.move(x + (dx * i) / steps, y + (dy * i) / steps);
    await page.waitForTimeout(35);
  }
  await page.mouse.up();
}

/** A canvas tile, addressed by the label on its edit-mode drag handle. */
function tile(page: Page, label: string): Locator {
  return page
    .locator(".react-grid-item")
    .filter({ has: page.locator("[data-drag-handle]", { hasText: label }) });
}

// ---------------------------------------------------------------------------
// Cold open + Beat 1 — the terminal is the run  [0:00 → 0:18]
// ---------------------------------------------------------------------------

test("b1 — the terminal is the run", async () => {
  test.setTimeout(300_000);
  const page = stage();

  // ---- off camera, before a single frame ---------------------------------
  await page.goto("/runs");
  await page.bringToFront();

  const target = await liveInteractiveRun(page);
  runId = target.id;
  runRepo = target.repo ?? "";

  // DA13, as code. The holder registry names the WRITER; `held: true` here is
  // the machine-checkable form of "the off-camera client attached FIRST". Fail
  // now — before the cold open — rather than at 1:15 over a terminal that is
  // happily accepting the presenter's keystrokes.
  const holder = await apiJson<{ held?: boolean; principal?: string; source?: string }>(
    page,
    `/api/v1/runs/${encodeURIComponent(runId)}/attach-holder`,
  );
  expect(
    holder?.held,
    "nobody holds this run's PTY — the off-camera client must attach BEFORE this spec runs (DA13). " +
      "Beat 3 has nothing to take over.",
  ).toBe(true);

  // Beat 4 saves a layout to this principal's account, and a saved layout BEATS
  // the situational preset on the next take — so take two would open on the
  // arrangement take one dragged, and "Edit layout turns the cockpit into a
  // board" would be narrated over a board somebody already rearranged. `layout:
  // []` is the reset (there is no DELETE route — see lib/api/run-layout.ts).
  await page.request
    .put("/api/v1/me/run-layout", {
      headers: demoHeaders(),
      data: { preset: "live", layout: [] },
    })
    .catch(() => {
      /* 501 on a store that cannot persist layouts — nothing to clear there */
    });

  // ---- roll ---------------------------------------------------------------
  await caption(page, "This run is already live. Open it.");
  await beat(page, PACE.read);

  // The board's Attach button, not the card body: an attachable card renders one
  // (runs.tsx's RunCard), a card for anything else does not, and the dropdown's
  // twin only exists while its menu is open — so on a correctly staged stack
  // this is unique, and liveInteractiveRun() has already proved it is.
  const attach = page.getByRole("button", { name: "Attach", exact: true });
  await expect(attach).toHaveCount(1, { timeout: 60_000 });
  await act(page, attach);
  await expect(page).toHaveURL(new RegExp(`/runs/${runId.replace(/[^\w-]/g, "")}`), { timeout: 30_000 });

  await caption(page, "What loads is not a log. It is the session.");

  // The hero, and the payoff of "what loads": a LIVE xterm, connected. The title
  // bar only reads `attach — <run id>` once connState is "open"
  // (attach-terminal.tsx) — a connecting or errored socket says something else
  // entirely, so this one string proves the session is really attached rather
  // than merely rendered.
  await expect(page.getByTestId("run-terminal-pane")).toBeVisible({ timeout: ATTACH_CONNECTS });
  await expect(page.getByText(`attach — ${runId}`)).toBeVisible({ timeout: ATTACH_CONNECTS });
  await beat(page, 2000);

  await caption(page, "The terminal is the run. You are supervising a process, not reading about it.");
  await beat(page, PACE.read + 900);
});

// ---------------------------------------------------------------------------
// Beat 2 — the header is identity  [0:35]
// ---------------------------------------------------------------------------

test("b2 — the header", async () => {
  test.setTimeout(180_000);
  const page = stage();

  // The 52px command bar (run-detail-summary-header.tsx). Anchored on the page's
  // only h1 — the header is its parent — because every other landmark in that
  // row ("Runs") also exists in the shell's nav.
  const header = page.getByRole("heading", { level: 1 }).locator("xpath=..");

  // The SAY line names five facts. Assert the four that have honest accessible
  // names before pointing at any of them: this is the beat that CLAIMS the
  // header carries identity, and a narrated claim over a missing chip is exactly
  // the green-take-over-a-broken-screen failure this suite exists to prevent.
  const title = page.getByRole("heading", { level: 1 });
  // null when the run has no repo label at all — an interactive run on ephemeral
  // scratch legitimately does. getByText("") matches every empty element on the
  // page, so the ring would land somewhere arbitrary; skip the glance instead.
  const repo = runRepo ? page.getByText(runRepo, { exact: true }).first() : null;
  // The confinement chip's title is `<label> — <mechanism> · internal class CCn`
  // (primitives.tsx). Matching the wire class keeps this working on any barrier
  // tier the host can actually build — Fence here, Wall or Vault elsewhere.
  const barrier = page.getByTitle(/internal class CC\d/).first();
  const state = page.getByText("Running", { exact: true }).first();
  // RunStateBadge renders TITLE CASE ("Running", not "RUNNING") from
  // runStateMeta; matching the wire enum would wait out the whole timeout.
  const elapsed = page.getByLabel(/^Running for /);
  const interactive = page.getByText("Interactive — attachable", { exact: true });
  const kill = page.getByRole("button", { name: "Kill", exact: true });

  await expect(title).toBeVisible();
  await expect(barrier).toBeVisible();
  await expect(state).toBeVisible();
  await expect(elapsed).toBeVisible();
  await expect(kill).toBeVisible();

  await caption(page, "The header is identity: agent, workspace, barrier, state, elapsed.");

  // The tour, left to right, in the order the row renders. The agent badge has
  // no accessible name of its own (it is an initials disc — "CC" for Claude
  // Code), so it is addressed positionally inside the header: the direct-child
  // spans are [divider, AgentBadge, state chip, …] and the conditional ones all
  // come after. Cosmetic only — glance() never fails a take over a ring.
  await glance(page, header.locator("> span").nth(1), 700);
  await glance(page, title, 800);
  if (repo) await glance(page, repo, 800);
  await glance(page, state, 700);
  await glance(page, barrier, 800);
  await glance(page, interactive, 800);
  await glance(page, elapsed, 700);

  await caption(page, "Kill sits at the right. One click stops the sandbox, and it is audited.");
  // DWELL ON KILL, NEVER CLICK IT. The rest of the video needs this run alive,
  // and there is no second take of a killed sandbox.
  await glance(page, kill, PACE.read);
  await caption(page, "Naming it, not pressing it.");
  await beat(page, PACE.read);
  await expect(kill).toBeEnabled();
  await spotlight(page, null);
});

// ---------------------------------------------------------------------------
// Beat 3 — take over  [1:15]
// ---------------------------------------------------------------------------

test("b3 — take over", async () => {
  test.setTimeout(600_000);
  const page = stage();

  // The staged holder, on screen. Both halves render only when the SERVER said
  // read_only:true in the attach-mode frame (attach-terminal.tsx never infers
  // it), so these two are the honest proof that the presenter really did arrive
  // second — not that a badge happens to be in the markup.
  const readOnlyBadge = page.getByText(COPY.watchingReadOnly, { exact: true });
  const heldFooter = page.getByText(COPY.heldHint, { exact: true });
  await expect(readOnlyBadge).toBeVisible({ timeout: ATTACH_CONNECTS });
  await expect(heldFooter).toBeVisible();

  await caption(page, "Someone else already holds this session, so I am watching read-only.");
  await glance(page, readOnlyBadge, PACE.read);

  await caption(page, "Attach is exclusive on purpose. Two writers on one terminal is how mistakes happen.");
  await glance(page, heldFooter, PACE.read);

  await caption(page, "And how audits lie — nobody can say who typed what.");
  await beat(page, PACE.read + 600);
  await spotlight(page, null);

  // The footer's Take over opens a confirm — it ends another human's live
  // session, so it gets the same stop as the deny confirm in live-approvals.tsx.
  await act(page, page.getByRole("button", { name: COPY.takeOver }));
  const confirm = page.getByRole("alertdialog");
  await expect(confirm).toBeVisible({ timeout: 30_000 });
  // The dialog's body names the holder and says what take-over does:
  // "<principal> is driving this session now. Taking over disconnects them and
  // records you as the holder in the audit trail." Dwell on it — the sentence is
  // the beat.
  await caption(page, "Taking over evicts them and tells them. No silent shoulder-surfing.");
  await beat(page, PACE.read + 900);

  // Scoped to the alertdialog: "Take over" is also the dialog's TITLE and the
  // footer button still in the DOM behind it.
  await act(page, confirm.getByRole("button", { name: COPY.takeOver }));

  // THE PAYOFF. The driving chip is rendered only from a fresh attach-mode frame
  // with read_only:false on an OPEN socket — so waiting on it is waiting on the
  // whole round trip: takeover accepted, old holder displaced, our socket
  // reclaimed. Nothing may be typed before it appears, or the reconnect eats the
  // first characters and the shell reports "command not found" on camera.
  await expect(page.getByText(COPY.driving, { exact: true })).toBeVisible({ timeout: TAKEOVER_RECONNECTS });
  await expect(readOnlyBadge).toBeHidden();

  await caption(page, "Same session, same scrollback — now it says you are driving.");
  await beat(page, PACE.read);

  await typeInTerminal(page, "whoami");
  // xterm has no local echo: every character on screen came back over the PTY.
  // A read-only client's keystrokes are dropped SERVER-side, so this echo is the
  // only real proof that the take-over actually transferred the write channel —
  // the chip alone is a claim about state, this is the consequence.
  await expect(page.locator(".xterm-screen").first()).toContainText(/whoami/, { timeout: 45_000 });
  await beat(page, PACE.read + 900);
});

// ---------------------------------------------------------------------------
// Beat 4 — the widget canvas  [1:52]
// ---------------------------------------------------------------------------

test("b4 — widgets", async () => {
  test.setTimeout(300_000);
  const page = stage();

  // B-DEPENDENT (flagged in the script): the edit affordances float bottom-right
  // over the canvas rather than living in the tabs row — an open UX deferral, so
  // the labels below are the ones ui/src carries today (canvas.tsx).
  await act(page, page.getByRole("button", { name: COPY.editLayout }), "Edit layout turns the cockpit into a board — tiles drag and resize.");
  await expect(page.getByText(COPY.editing, { exact: true })).toBeVisible({ timeout: 30_000 });

  // The toolbar, named left to right. Not clicked: Reset to default would throw
  // away the arrangement the next two gestures are about to make.
  await glance(page, page.getByRole("button", { name: "layout: Live" }), 600);
  await glance(page, page.getByRole("button", { name: "layout: Finished" }), 600);
  await glance(page, page.getByRole("button", { name: COPY.resetLayout }), 600);
  await glance(page, page.getByRole("button", { name: COPY.saveLayoutDefault }), 700);
  await glance(page, page.getByRole("button", { name: COPY.doneEditing }), 600);
  await spotlight(page, null);

  // The catalog. On a live run every placeable widget is already on the board
  // (the `live` preset places all of them), so this is a REVEAL, not an add —
  // opened, read, closed.
  await act(page, page.getByRole("button", { name: COPY.addWidget }), "Add widget lists them all. Terminal is greyed out; a cockpit keeps its session.");
  const catalog = page.getByRole("dialog");
  await expect(catalog).toBeVisible({ timeout: 30_000 });
  const terminalRow = catalog.getByRole("button", { name: "Terminal", exact: true });
  // "greyed out" is a claim about the product, not about the pixels: the row is
  // disabled because the terminal is `required` in widget-registry.ts. Assert
  // the mechanism, then point at it.
  await expect(terminalRow).toBeDisabled();
  await glance(page, terminalRow, PACE.read + 600);
  await page.keyboard.press("Escape");
  await expect(catalog).toBeHidden({ timeout: 15_000 });
  await spotlight(page, null);

  // Drag Egress down the rail. Plays silent under the tail of the last line —
  // the script gives beat 4 three SAY lines and this is not one of them.
  const egress = tile(page, EGRESS_WIDGET);
  const egressBefore = await egress.boundingBox();
  await dragBy(page, page.locator("[data-drag-handle]", { hasText: EGRESS_WIDGET }), 0, 170);
  // Let react-grid-layout's transition settle before measuring.
  await beat(page, 900);
  const egressAfter = await egress.boundingBox();
  // "tiles drag" — assert the tile actually moved. A drag that silently no-ops
  // (a missed handle, a cancelled pointer) leaves the narration claiming a
  // rearrangement over a board that never budged.
  expect(egressBefore && egressAfter && egressAfter.y > egressBefore.y + 20).toBe(true);

  // Resize Files changed from its south-east corner. It sits in the last four
  // columns, so it grows DOWNWARD — there is no width to the right of it.
  const files = tile(page, FILES_WIDGET);
  const filesBefore = await files.boundingBox();
  await dragBy(page, files.locator(".react-resizable-handle-se"), 0, 120);
  await beat(page, 900);
  const filesAfter = await files.boundingBox();
  // "…and resize".
  expect(filesBefore && filesAfter && filesAfter.height > filesBefore.height + 20).toBe(true);

  await act(page, page.getByRole("button", { name: COPY.saveLayoutDefault }), "Save as my default — yours, per user, and it survives a new laptop.");
  // Three outcomes share this button (canvas.tsx's saveDefault): "ok",
  // "unsupported" (the store cannot persist layouts at all) and "failed". Only
  // the first is the one the narration just claimed, and the other two look
  // almost identical on camera — a toast in the same corner.
  await expect(page.getByText(COPY.layoutSaved, { exact: true })).toBeVisible({ timeout: 30_000 });
  // And the receipt behind the toast: "per user, survives a new laptop" is a
  // claim about a SERVER row, so read it back. The row is scoped by principal
  // alone (internal/api/ui_layout.go) — no run id anywhere in it.
  const saved = await apiJson<{ layout?: unknown[]; updated_at?: string }>(
    page,
    "/api/v1/me/run-layout?preset=live",
  );
  expect((saved?.layout ?? []).length, "the layout toast fired but the server has no row for it").toBeGreaterThan(0);
  await beat(page, PACE.read);

  await act(page, page.getByRole("button", { name: COPY.doneEditing }));
  await expect(page.getByText(COPY.editing, { exact: true })).toBeHidden({ timeout: 15_000 });
});

// ---------------------------------------------------------------------------
// Beat 5 — focus  [2:11] — and out
// ---------------------------------------------------------------------------

test("b5 — focus, and out", async () => {
  test.setTimeout(240_000);
  const page = stage();

  await act(page, page.getByRole("button", { name: COPY.enterFocus, exact: true }), "Focus gives the session the whole screen. Evidence moves to a dock beside it.");

  // Focus mode portals to document.body and takes the shell's header and sidebar
  // with it (focus-mode.tsx). All three halves of the SAY line, asserted: the
  // HUD, the dock rail, the evidence strip.
  await expect(page.getByRole("button", { name: COPY.exitFocus })).toBeVisible({ timeout: 30_000 });
  const dock = page.getByRole("group", { name: COPY.dock });
  await expect(dock).toBeVisible();
  await expect(page.getByText(COPY.shortcuts, { exact: true })).toBeVisible();
  // The session really is in there — the terminal remounts into the overlay, so
  // its socket reconnects. Safe by now: the client we displaced in beat 3 does
  // NOT reconnect (a taken-over client is contractually forbidden to), so this
  // reclaim has nobody to race.
  await expect(page.locator(".xterm-screen").first()).toBeVisible({ timeout: ATTACH_CONNECTS });
  await beat(page, PACE.read);

  // The dock opens on the first dockable widget (Egress), so open a DIFFERENT
  // one — otherwise the click is a no-op the viewer reads as a dead button.
  await act(page, dock.getByRole("button", { name: `Show ${FILES_WIDGET}` }));
  await expect(page.getByRole("region", { name: FILES_WIDGET })).toBeVisible({ timeout: 30_000 });
  await beat(page, PACE.read + 600);

  await caption(page, "Escape brings the console back.");
  await beat(page, PACE.read);
  // WCAG 2.1.2 — focus mode is not a keyboard trap. The handler is on document
  // in the CAPTURE phase, so it runs before xterm's textarea swallows the key
  // (which is also what costs a vim user their Escape while focus is on — the
  // strip says so, and it is on the script's friction list).
  await page.keyboard.press("Escape");
  await expect(page.getByRole("button", { name: COPY.exitFocus })).toBeHidden({ timeout: 30_000 });
  // …and the console is genuinely back: the canvas's own controls return.
  await expect(page.getByRole("button", { name: COPY.editLayout })).toBeVisible({ timeout: 30_000 });
  await beat(page, PACE.read);

  await caption(page, "Next: everything a run may touch — the workspace.");
  await beat(page, PACE.read);
  await caption(page, "And secrets it can use, but never hold.");
  await beat(page, PACE.chapter);
  await caption(page, "");
});
