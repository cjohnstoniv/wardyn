/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

/*
 * Video 10 of the series — Approvals & egress scopes.
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
 * hosts example.org and ingest.sentry.io, and touches nothing else. Video 2 owns
 * `slugify` and example.com; sharing either would let one video's permanent
 * grant silently disarm the other's hold beat. Nothing here types
 * example.com — not even the example.com pill the demo card offers.
 *
 * PACING. Three stretches of this video are nothing but waiting on a container:
 * the demo sandbox's spin-up and the two real-workspace runs' launches. Each is
 * wrapped in overlay.ts's ffwdStart/ffwdEnd and squeezed 12x by
 * scripts/demo-ffwd.py after assembly. NOTHING MAY SPEAK inside a span — a line
 * spoken over frames the encoder throws away lands 12x early and drags every
 * later cue with it — and each ffwdStart is preceded by a beat(200) that DRAINS
 * the previous line's audio, because caption() only SCHEDULES speech. Each
 * ffwdEnd fires the instant the awaited state lands (in a `finally`, so a failed
 * take still closes its span), BEFORE the next caption. Nothing else is spanned:
 * a hold is raised in about a second, and a span under ~1s is dropped by the
 * encoder anyway.
 *
 * WHAT CAN STOP EXISTING MID-WAIT. Every decision surface in this video renders
 * only while its run is RUNNING — see WAIT_UNLESS_GONE below. Waits are raced
 * against that, always.
 *
 * This is NOT a test. It asserts only enough to keep itself honest and to know
 * when to advance; a failure here means the recording is wrong, not that the
 * product is broken. It runs against the REAL compose stack on :8080 with real
 * sandboxes — the hermetic `-runner none` e2e backend cannot start one at all.
 *
 * Driven by `scripts/record-demo.sh --video 10`, which globs this exact filename
 * and names the take wardyn-10-approvals-and-egress-<stamp>.mp4 — so this
 * FILENAME IS LOAD-BEARING. It self-skips without WARDYN_DEMO=1 so a bare
 * `pnpm e2e` can never point a browser at a developer's live stack and start
 * deleting workspaces.
 *
 * The demo project records HEADLESS (playwright.config.ts): the browser records
 * ITSELF, there is no OS window during a take, and nothing here may depend on
 * window geometry. Nothing does — every locator is a role/testid on the page.
 *
 * Selectors are getByRole + accessible name, matching the rest of the suite:
 * every literal below exists in ui/src today (re-read 2026-08-18 against
 * demo-screen.tsx, live-approvals.tsx + copy.ts, new-run-screen.tsx and
 * allowed-hosts-card.tsx).
 */

import { mkdirSync, rmSync } from "node:fs";
import { test, expect, type Locator, type Page } from "@playwright/test";
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
// stage.ts is the rig: importing it registers this file's beforeAll/afterAll
// (one browser, one context, one recorded page), and every beat reads the page
// out of stage() inside a test body rather than closing over a module binding.
import { stage } from "./stage";
import { APPROVAL_APPEARS, decide } from "./funnel";
import { sweepStaleState } from "./sweep";

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
const HELD_HOST = "example.org";

/** The host nobody asked for — an agent's own telemetry endpoint. Denied. */
const TELEMETRY_HOST = "ingest.sentry.io";

// --max-time 60 has to outlast the proxy's ~30s wait_for_review hold
// (defaultHoldTimeout, internal/egress/proxy/approvals.go) so the on-camera
// approval lands while the request is still parked and the SAME curl completes.
// Beats 6 and 8 reuse the exact string on purpose — "same command" is a line
// the narrator says out loud.
//
// The host is example.org (IANA), deliberately boring: crates.io answered
// this same beat's OPENED tunnel with its own 403 twice on 2026-08-21
// (Cloudflare/UA caprice), which on camera is indistinguishable from the
// proxy refusing — the beat's truth cannot depend on a third party's bot
// policy. Not example.com: the Always beat writes HELD_HOST permanently onto
// the real workspace, and example.com is the demo cards' printed noun —
// distinct host, zero cross-episode state.
//
// `-o /dev/null -w '%{http_code}\n'` prints just the one number that matters.
// The `\n` here is TWO characters (backslash, n), not a JS newline escape —
// curl's own -w parser is what turns it into a line break; typing an actual
// newline mid-command would submit the line early.
const REACH_HELD = `curl -sS --max-time 60 -o /dev/null -w '%{http_code}\\n' https://${HELD_HOST}/`;
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
 * `-w '%{http_code}\n'` writes ONLY the status code (the body is discarded to
 * /dev/null), so the bare digits `200` are unambiguous here — a proxy refusal
 * would print `403`, which is precisely the frame this assertion exists to
 * refuse to narrate over.
 */
const RESPONDED = /\b200\b/;

/** The owner's staccato lines read fast; PACE.read after one is dead air. */
const BEAT_SHORT = 1400;

// ---------------------------------------------------------------------------
// WAIT_UNLESS_GONE — the one shape every wait in this video is written in.
//
// THIS VIDEO IS ALL APPROVALS, and every approval surface it uses renders only
// while its run is RUNNING:
//   - the /demos card's terminal, LiveApprovals strip and audit panel are the
//     `running` branch of DemoRunControls (demo-screen.tsx); the moment the demo
//     run goes terminal the whole branch UNMOUNTS and demo-terminated (or
//     demo-failed) takes its place;
//   - the cockpit's strip is gated on `run.state === "RUNNING"`
//     (run-detail.tsx's terminalPane).
// So a sandbox that dies — a failed start, an auto-stop, a stray kill — takes
// the row, the terminal and the panel with it, and a bare wait then polls for
// minutes against a node that can no longer appear. V05 lost a take to exactly
// that shape. Racing the two mutually-exclusive outcomes turns it into an
// immediate, NAMED failure instead.
// ---------------------------------------------------------------------------

/** The demo card's two "this run is over" branches (demo-screen.tsx). */
const demoOver = (card: Locator): Locator =>
  card.getByTestId("demo-terminated").or(card.getByTestId("demo-failed"));

/** RunStateBadge's terminal labels — TITLE CASE, from runStateMeta (primitives.tsx). */
const RUN_OVER = /^(Completed|Failed|Stopped|Killed)$/;

/**
 * Await `want`, unless `gone` lands first — then fail loudly, saying which.
 *
 * `want` is any Playwright wait already in flight (an `expect(…)` assertion or a
 * `locator.waitFor`), so this covers both toBeVisible and toContainText without
 * a second helper. The losing promise keeps polling in the background until its
 * own timeout; both branches carry a rejection handler, so that is a dangling
 * poll and never an unhandled rejection (same shape 05/06 inline).
 */
async function waitUnlessGone(
  want: Promise<unknown>,
  gone: Locator,
  timeout: number,
  why: string,
): Promise<void> {
  const outcome = await Promise.race([
    want.then(
      () => "ok" as const,
      () => "failed" as const,
    ),
    gone.waitFor({ state: "visible", timeout }).then(
      () => "gone" as const,
      () => "timeout" as const,
    ),
  ]);
  expect(
    outcome,
    outcome === "gone"
      ? `${why} — the run ENDED before this beat could land.`
      : `${why} — the wait timed out (${timeout / 1000}s) with the run still live.`,
  ).toBe("ok");
}

// ---------------------------------------------------------------------------
// Staging — the part that must not be prose.
//
// OFF-CAMERA PRECONDITIONS THE OPERATOR STILL OWNS (nothing below can encode
// these; check them before the take rolls):
//   1. The compose stack is up on :8080 and a barrier is READY — the demo
//      cards' Start button is disabled without one, and this spec fails loudly
//      on that rather than clicking a dead button for 45s.
//   2. The stack has NOT been reset since the earlier videos (record-demo.sh
//      --video 10 already defaults DO_RESET=0; never pass --reset here).
//   3. Both hosts are reachable from this machine's egress path — example.org
//      must answer 200, or beat 2's payoff assertion fails.
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

  // EMPTY, not merely present. Onboarding scans the directory and seeds an
  // `egress:<host>` requirement row per detected ecosystem registry
  // (source_scan.go's scan_seeded rows) — and AllowedHostsCard counts those
  // alongside approved_egress. One stray package.json in here and beat 7's
  // "Allowed hosts · 1" is "· 2", with the receipt naming a host nobody
  // decided on camera. Same reset video 09 does for its own workspace dir.
  rmSync(WORKSPACE_PATH, { recursive: true, force: true });
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
  // The third lane, and the one the rm above exists for: a scan-seeded
  // `egress:<host>` requirement counts on the Allowed hosts card exactly like
  // an approved host does. Caught HERE, off camera, rather than at beat 7's
  // "· 1" three minutes into the take.
  const seeded = Object.keys(fresh.effective_requirements ?? fresh.requirements ?? {}).filter((k: string) =>
    k.startsWith("egress:"),
  );
  expect(seeded, `a fresh egress-lab must require no hosts — ${WORKSPACE_PATH} is not empty`).toEqual([]);
}

// Registered AFTER stage.ts's own beforeAll (import order), so stage() is
// assigned by the time this runs. Re-guards on WARDYN_DEMO because this hook
// DELETES a workspace: a file-level test.skip must never be the only thing
// standing between a developer's stack and that.
test.beforeAll(async () => {
  if (!process.env.WARDYN_DEMO) return;
  // S6: deny stale pending approvals / kill stale runs first — the Approvals
  // badge otherwise carries a prior take's number (as high as 10) through the
  // whole video, and the on-camera tick from 0 to 1 in beat 1 is the receipt.
  await sweepStaleState([WORKSPACE]);
  await restageWorkspace();
});

// ---------------------------------------------------------------------------
// Beats 0-5 — the demo sandbox: held, approved, the ladder, denied
// ---------------------------------------------------------------------------

test("beats 0-5 — the wait-for-review hold, and the scope ladder", async () => {
  test.setTimeout(900_000);
  const page = stage();
  // /demos redirects to the held-at-the-door step directly (App.tsx) — go
  // straight there. See the file header's "WHY /demos AND NOT THE FUNNEL" for
  // the now-stale premise this retarget doesn't resolve (episode re-anchoring
  // is a separate phase); the assertions below hold either way, since
  // `demo-card-held-at-the-door` (H4) is the only demo card on this step.
  // A fresh take context has never seen the welcome hero — /setup renders
  // OnboardingScreen until wardyn-onboarding-seen is set, and the ?step= deep
  // link lands on the hero instead of the demo (the live take-killer episode
  // 04's beat 5 documents). Seed it the way that beat does: this host's
  // operator walked the welcome in episode 02.
  await page.goto("/");
  await page.evaluate(() => {
    try {
      localStorage.setItem("wardyn-onboarding-seen", "1");
    } catch {
      /* private mode — ignore */
    }
  });
  await page.goto("/setup?step=held-at-the-door");
  await page.bringToFront();

  // Fail here rather than several minutes into a silent, caption-less take:
  // every narration call degrades to a no-op by design, so nothing downstream
  // would ever complain about an overlay that failed to install.
  await expect
    .poll(() => page.evaluate(() => typeof (window as unknown as Record<string, unknown>).__demo), {
      timeout: 15_000,
    })
    .toBe("object");

  await chapter(page, "Approvals and egress", "How far should one yes reach?");

  // ---- B0 · Start ---------------------------------------------------------
  // The catalog stacks every demo on one page, so `demo-card-<id>` is a real
  // scope here (it is NOT on the funnel path, where each step renders the bare
  // DemoRunControls — see funnel.ts's note). Everything below is scoped to
  // this card: the page holds six other terminals' worth of controls.
  //
  // S7 (unnarrated, mechanics only): the card's own printed steps still say
  // example.com/wikipedia.org, but the commands typed below reach example.org —
  // a host this workspace wants for good. No caption owns the swap; it is not
  // in the owner's dialog.
  const card = page.getByTestId("demo-card-held-at-the-door");
  // The step TITLE renders above the card wrapper (DemoDetail's testid wraps
  // the body, not the h2 — rehearsal-proven), so the heading is asserted at
  // page level, episode 03's idiom; everything interactive stays card-scoped.
  await expect(page.getByRole("heading", { name: "Held at the door", level: 2 })).toBeVisible({
    timeout: 60_000,
  });

  await spotlight(page, card);
  await caption(page, "A run reaches for a host that isn't on the list of hosts it may reach.");
  await beat(page, PACE.read);
  await caption(page, "We know what happens next.");
  await beat(page, PACE.read);
  await caption(page, "Wardyn stops it.");
  await beat(page, BEAT_SHORT);
  await caption(page, "But now comes the interesting question:");
  await beat(page, PACE.read);
  await caption(page, "What exactly does our approval mean?");
  await beat(page, PACE.read);
  await spotlight(page, null);

  const startDemo = card.getByTestId("demo-start-held-at-the-door");
  // The barrier gate, named. Without a ready barrier this button is disabled
  // and act() would simply park on it for the full 45s action timeout, which
  // on camera is indistinguishable from the app hanging.
  await expect(startDemo, "the demo Start button is disabled — this stack has no ready barrier").toBeEnabled(
    { timeout: 60_000 },
  );
  // SPRINT: nothing is spoken between the click and the wait, because
  // everything from here IS the wait.
  await act(page, startDemo, "Trigger the request.");

  // FAST-FORWARD. Sandbox spin-up is 30s-3min of a spinner nobody needs to sit
  // through (a cold first take also pulls the image). act() above already held
  // for its own line; the beat(200) drains any residue, because opening a span
  // over a still-speaking caption puts that speech inside compressed footage
  // and lands every later cue early.
  await beat(page, 200);
  await ffwdStart(page);
  try {
    await waitUnlessGone(
      expect(card.locator(".xterm-screen").first()).toBeVisible({ timeout: SANDBOX_UP }),
      demoOver(card),
      SANDBOX_UP,
      "the demo sandbox never came up as a live terminal",
    );
  } finally {
    // Real time resumes the instant the terminal is on screen — and even on a
    // failed take, so the span the encoder gets is one this run actually spent.
    await ffwdEnd(page);
  }
  // .xterm-screen renders when AttachTerminal MOUNTS, before the PTY websocket
  // is up — typing here eats the first characters and the shell reports
  // "command not found" on camera. Deliberately OUTSIDE the span: this pause is
  // for the product, and the viewer never sees it either way.
  await beat(page, PACE.read);

  // ---- B1 · Held ----------------------------------------------------------
  await typeInTerminal(page, REACH_HELD, card);

  // The strip's header is the wait_for_review flavour (isHeld → anyHeld), which
  // is the entire difference between this demo and "Fail, then approve". If it
  // says "Approval needed — off-policy egress" instead, the sandbox is NOT
  // holding anything and the narration above is a lie.
  //
  // RACED (see waitUnlessGone): the strip lives inside the card's `running`
  // branch, so a demo that dies here takes the header with it.
  //
  // STARTED, NOT AWAITED: the proxy holds this connection for only ~30s
  // (defaultHoldTimeout, no production knob), and a serial header-wait here
  // plus the narration blew the window once (08-21 rehearsal: the curl had
  // 403'd before the Approve landed). Let the poll run UNDER the callback
  // caption and collect it after; the failure still surfaces at the await.
  // P11b shortened this stanza, which only widens the margin.
  const headerUp = waitUnlessGone(
    expect(card.getByText("Sandbox is waiting — approve to let it through")).toBeVisible({
      timeout: APPROVAL_APPEARS,
    }),
    demoOver(card),
    APPROVAL_APPEARS,
    `${HELD_HOST} never surfaced as a HELD request`,
  ).catch((e: unknown) => e);
  // Ring only once the terminal has something to show — the command now
  // hanging — never the idle pane before it (S3: point at content, not
  // emptiness).
  await spotlight(page, card.locator(".xterm-screen").first());
  // P11b (dialog review, owner-ratified 2026-08-23): this is the THIRD full
  // explanation of held-at-the-boundary (03 walks it, 07 repeats it), and it
  // costs momentum in the longest episode. One callback replaces "The command
  // is waiting." / "It hasn't failed." / (below) "It's sitting at the
  // boundary, waiting for a decision." — the callback's own words are
  // "waiting, not failing", so keeping the two it paraphrases would stutter.
  // "Nothing has left the sandbox." stays: it is the only line here that says
  // something the callback does not, and it rides the held row's own ring.
  await caption(page, "Same as in 'What it stops' — it's waiting, not failing.");
  await beat(page, PACE.read);
  {
    const r = await headerUp;
    if (r instanceof Error) throw r;
  }
  // No race below: the header only renders when the strip has a pending row
  // (LiveApprovals returns the idle hint otherwise), so this resolves at once.
  const heldRow = card.getByTestId("live-approval-row").filter({ hasText: HELD_HOST });
  await expect(heldRow).toBeVisible({ timeout: APPROVAL_APPEARS });
  await expect(heldRow).toContainText("waiting");
  // LET THE LINE FINISH. caption() only SCHEDULES narration (narrator.ts lays
  // each cue on the timeline at the moment it was spoken); it is beat()/act()
  // that hold for it. Without this the next caption starts ~2s in and the mux
  // plays both lines over each other. The floor is subsumed by the residual
  // speech, so the cost against the proxy's ~30s hold is ~3s, not 2.2+speech.
  await spotlight(page, heldRow);
  await caption(page, "Nothing has left the sandbox.");
  await beat(page, PACE.read);
  await spotlight(page, null);

  // ---- B2 · Default scope -------------------------------------------------
  // The bare split-button click — "This run", today's default scope. decide()
  // decides a NAMED host, never .first(): approving something you did not mean
  // to approve is the worst possible frame in a governance video.
  await decide(card, "Approve", "Approve.", HELD_HOST);
  // P8 (dialog review, owner-ratified 2026-08-23): the ladder on screen starts
  // at Once, so calling "this run" the SIMPLEST contradicted the picture. One
  // caption, and it claims reach-for-it-most instead of simplest.
  await caption(page, "The simplest one: just this run.");
  await beat(page, BEAT_SHORT);
  await caption(page, "It works for this run, and then it disappears.");
  await beat(page, PACE.read);

  // THE PAYOFF. The narration claims the same in-flight request finishes with
  // no retry; without this assertion a take stays green while narrating success
  // over a terminal showing a 403 and a fresh pending row. NOT fast-forwarded:
  // this is the frame the whole beat exists for, and the approve-to-response
  // gap is about a second anyway.
  //
  // expect.poll over innerText, NOT toContainText: two rehearsals (08-21)
  // failed here with the 200 demonstrably on screen and in the a11y tree at
  // failure time, while the same toContainText on the same locator matched
  // instantly in an isolated probe — whatever take-context detail starves it,
  // innerText is the exact signal the viewer sees. On timeout, dump what the
  // page actually held so the next failure explains itself.
  const term200 = card.locator(".xterm-screen").first();
  try {
    await expect
      .poll(async () => (await term200.innerText().catch(() => "<no .xterm-screen>")), {
        timeout: 60_000,
        message: `the held ${HELD_HOST} request never completed with a 200`,
      })
      .toMatch(RESPONDED);
  } catch (e) {
    const screens = await card.locator(".xterm-screen").count().catch(() => -1);
    const over = await demoOver(card).isVisible().catch(() => false);
    const txt = await term200.innerText().catch(() => "<unreadable>");
    console.warn(
      `[v10] payoff diagnostic: screens=${screens} demoOver=${over} innerText=${JSON.stringify(txt.slice(0, 200))}`,
    );
    throw e;
  }
  await caption(page, "The request that was already waiting completes.");
  await beat(page, PACE.read);
  await caption(page, "This policy holds instead of refusing — the request waited at the door, and the wait ended with a yes. No retry needed.");
  await beat(page, PACE.read + 600);

  // ---- B3 · Unwanted host -------------------------------------------------
  // Silent: the owner's next SAY-ON-CLICK ("Open the scope menu…") is the
  // first spoken line of this beat — typing the second host is unnarrated.
  await typeInTerminal(page, REACH_TELEMETRY, card);
  const telemetryRow = card.getByTestId("live-approval-row").filter({ hasText: TELEMETRY_HOST });
  await waitUnlessGone(
    expect(telemetryRow).toBeVisible({ timeout: APPROVAL_APPEARS }),
    demoOver(card),
    APPROVAL_APPEARS,
    `${TELEMETRY_HOST} never raised an approval row — the ladder beat has nothing to open`,
  );

  // ---- B4 · The scope ladder ----------------------------------------------
  // A pure LOOK — nothing is decided here, the row stays pending for beat 5.
  //
  // Both carets on the row carry the same aria-label ("More options") by
  // design: ScopeMenu's trigger may never say approve/deny, or an unanchored
  // /approve/i query elsewhere in the suite would match two buttons. DOM order
  // is Approve, Approve-caret, Deny, Deny-caret — .first() is Approve's, the
  // same disambiguation e2e/approvals.spec.ts and funnel.ts's decide() use.
  await act(page, telemetryRow.getByRole("button", { name: "More options" }).first(), "Open the scope menu — here's the ladder.");
  // Radix portals the menu content, so it is NOT a descendant of the row — and
  // scoping to the open menu (rather than the page) keeps /^Once/ from also
  // matching the "Once, or for good" card sitting further down this catalog.
  const menu = page.getByRole("menu");
  await expect(menu).toBeVisible();
  const onceBtn = menu.getByRole("button", { name: /^Once\b/ });
  const thisRunBtn = menu.getByRole("button", { name: /^This run\b/ });
  const untilBtn = menu.getByRole("button", { name: /^Until…/ });
  const alwaysOption = menu.getByRole("button", { name: /^Always\b/ });

  // FAST TOUR — just naming the four rungs, ring flying between them.
  await spotlight(page, onceBtn);
  await caption(page, "Once.");
  await beat(page, BEAT_SHORT);
  await spotlight(page, thisRunBtn);
  await caption(page, "This run.");
  await beat(page, BEAT_SHORT);
  await spotlight(page, untilBtn);
  await caption(page, "Until a specific time.");
  await beat(page, BEAT_SHORT);
  await spotlight(page, alwaysOption);
  await caption(page, "Or always.");
  await beat(page, BEAT_SHORT);
  await caption(page, "Each one gives that decision a different lifetime.");
  await beat(page, PACE.read + 400);
  await spotlight(page, null);

  // SLOW PASS — one at a time, in the same order, now explained.
  await spotlight(page, onceBtn);
  await caption(page, "Once means exactly what it sounds like.");
  await beat(page, PACE.read);
  await caption(page, "One connection.");
  await beat(page, BEAT_SHORT);
  await caption(page, "The next request asks again.");
  await beat(page, PACE.read);
  await spotlight(page, thisRunBtn);
  await caption(page, "This run lasts until this run ends.");
  await beat(page, PACE.read);

  // "Until…" swaps the menu for its preset list (untilMode) rather than
  // deciding — the four presets and a datetime picker, with "← Back" out.
  // CLICK FIRST, narrate second: naming the presets before the submenu that
  // shows them is a claim with no receipt yet (S1).
  await act(page, untilBtn);
  await expect(menu.getByRole("button", { name: "15 minutes" })).toBeVisible();
  await caption(page, "Until gives you a time limit.");
  await beat(page, PACE.read + 600);
  await act(page, menu.getByRole("button", { name: "← Back" }));

  await spotlight(page, alwaysOption);
  await caption(page, "And Always would save it to a workspace — greyed out here, because this demo run has none.");
  // Two lines back to back: the first is 5.7 s of speech, and caption() does
  // not wait — without this beat the second cue lands 4 ms after the first and
  // the mux has to slide it (the 2026-09-08 take failed its overlap gate).
  await beat(page);
  await caption(page, "We'll do that one for real in a minute.");
  await beat(page, PACE.read);
  // THE PAYOFF of this beat, and the setup for beat 6's contrast. A REAL
  // disabled attribute, not aria-disabled (ScopeMenu renders plain <button>s
  // precisely so this is true). Unnarrated (not in the owner's script), but
  // still proven: a claim silently dropped from the menu would otherwise ship
  // undetected.
  await expect(alwaysOption).toBeDisabled();
  await expect(menu.getByText("Always needs a workspace — this run isn't attached to one.")).toBeVisible();
  await beat(page, PACE.read);
  await spotlight(page, null);

  await caption(page, "Let's let one approval window expire.");
  await beat(page, PACE.read);

  // Close without picking: this approval has to survive to lapse on its own.
  await page.keyboard.press("Escape");
  await expect(page.getByRole("menu")).toHaveCount(0);

  // THE FAIL-CLOSED BEAT. The proxy's ~30s hold cannot outlast the ladder tour
  // above it — by the time the menu closes the window has almost certainly
  // lapsed and curl has already died with its own connection error. Wait for
  // that line and say what it proves, instead of letting it die silently under
  // a strip that still reads WAITING (Dana: it failed closed; that is a
  // selling point, not a glitch). The row itself still saying WAITING after
  // its connection died is a product gap, not this beat's to fix.
  // Same innerText-poll shape as the payoffs — this exact toContainText passed
  // this morning and starved in a sibling beat an hour later; uniform shape,
  // no bets on which polls survive take context.
  const lapseTerm = card.locator(".xterm-screen").first();
  await expect
    .poll(async () => (await lapseTerm.innerText().catch(() => "<no .xterm-screen>")), {
      timeout: 45_000,
      message: `${TELEMETRY_HOST}'s held request never lapsed with its own curl: (56) — the fail-closed beat has nothing to point at`,
    })
    .toMatch(/curl: \(56\)/);
  // SCREEN: Window expires.
  await spotlight(page, card.locator(".xterm-screen").first());
  await caption(page, "Nobody answered.");
  await beat(page, PACE.read);
  await caption(page, "Nobody answered inside the window — thirty seconds here — so the door stays shut: nothing was let through.");
  await beat(page, PACE.read);
  await caption(page, "The request was never granted.");
  await beat(page, PACE.read + 400);
  await spotlight(page, null);

  // ---- B5 · Deny ----------------------------------------------------------
  // The ~30s hold may well have expired during the ladder walk — the request
  // 403s and the row STAYS pending and decidable, which is why this beat is
  // written as "deny a pending row", not "deny a held one".
  //
  // decide() clicks the bare Deny, confirms in the alertdialog (Deny always
  // confirms, whatever the scope), and asserts the row is GONE afterwards.
  //
  // Cheap, and true without opening it: the row's Deny button carries the same
  // caret as Approve's. VERIFY the deny scope set at rehearsal (Once/This
  // run/Until/Always, or a narrower list) before this line airs as fact.
  await caption(page, "And deny has a scope too.");
  await beat(page, PACE.read);
  await caption(page, "A no can be just as deliberate as a yes.");
  await beat(page, PACE.read);
  await decide(card, "Deny", "Deny.", TELEMETRY_HOST);

  await caption(page, "The request is refused.");
  await beat(page, PACE.read);

  // THE RETRY, and it is load-bearing rather than decoration.
  //
  // The audit panel projects egress.allow/deny/pending ONLY (egressFromAudit,
  // lib/api/audit.ts) — an approval.decide is not in it. And the deny above
  // almost certainly landed AFTER the proxy's ~30s hold expired (the ladder
  // walk is many spoken lines), by which point the held connection had already
  // been released as `pending` and curl was gone. Nothing re-evaluates the host
  // on its own, so with no second attempt the panel shows a PENDING chip while
  // the narrator says "that refusal becomes part of the record" — the exact
  // class of dishonest frame this driver exists to refuse.
  //
  // Typing the same command again is what makes the line true: the deny is
  // scoped to this run, so the proxy refuses it from cache with no human in the
  // loop (no new row is raised — approvals.go's cached apDenied), the terminal
  // shows an instant refusal instead of a 60-second hang, and THAT is the
  // egress.deny the panel is about.
  await typeInTerminal(page, REACH_TELEMETRY, card);
  const auditPanel = card.getByTestId("demo-audit-panel");
  await spotlight(page, auditPanel);
  // The claim, asserted: a deny row for THIS host, not merely the pending row
  // that was already there. The panel polls every 2s.
  await waitUnlessGone(
    expect(
      auditPanel
        .getByTestId("demo-audit-rows")
        .locator("li")
        .filter({ hasText: TELEMETRY_HOST })
        .filter({ hasText: "deny" })
        .first(),
    ).toBeVisible({ timeout: 60_000 }),
    demoOver(card),
    60_000,
    `no egress.deny for ${TELEMETRY_HOST} reached the demo's audit panel`,
  );
  await caption(page, "And that refusal is a row in the record — with its own scope.");
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
  await act(page, page.getByRole("button", { name: "New run" }), "The demo cards have no workspace, so Always was greyed out. Create a new run on our real workspace, where Always is allowed.");
  await expect(page).toHaveURL(/\/runs\/new/, { timeout: 30_000 });

  const titleBox = page.getByLabel("Title");
  // Fill BEFORE ringing: a ring that lands before the fill sits on an empty
  // box for its whole visible span — .fill() is instant, so "ring then fill"
  // never actually shows content inside it (S3: point at content).
  await titleBox.fill(ALWAYS_RUN_TITLE);
  await spotlight(page, titleBox);
  await spotlight(page, null);

  // Terminal, not the agent: this video is keyless, and a shell is all a curl
  // needs. The wizard's own default is an INTERACTIVE agent run
  // (initialWizardState: runType "agent", mode "interactive"), so the "Start
  // with" segment is already on screen — this click only swaps which of its two
  // options is chosen ("Terminal — a shell in the workspace dir"). Real radios:
  // Seg renders role=radio buttons inside a radiogroup and the accessible name
  // is the FULL option label, so prefix-match it. Nothing else on this page has
  // a radio whose name starts with "Terminal".
  //
  // The startup field the segment reveals ("Startup command (optional)") is
  // deliberately LEFT BLANK: the run comes up idle with a shell, which is
  // exactly what beats 6 and 8 type into. It is also why Title is the only
  // required field here (new-run-screen's `problem`: needsTask is false for an
  // interactive agent run), so Launch is live the moment the title is in.
  //
  // Deliberately UNNARRATED from here through the launch and the boot below:
  // the owner's B6 line names only the FIRST click ("Create a new run…"),
  // and the next spoken line waits until there is something to decide.
  await act(page, page.getByRole("radio", { name: /^Terminal/ }));

  // The workspace trigger has NO accessible name (the Agent select beside it is
  // labelled, this one was never wired up), so it is addressed by its
  // placeholder text — an honest workaround for a real a11y gap, not a test
  // convenience.
  await act(page, page.getByRole("combobox").filter({ hasText: "Ephemeral scratch" }));
  await act(page, page.getByRole("option", { name: new RegExp(WORKSPACE, "i") }).first());

  // EVERYTHING ELSE IS LEFT ALONE, and that is the beat: Confined, the model
  // host allow-listed (allowedDomains: ["api.anthropic.com"]), unlisted hosts on
  // "Deny, but ask" (deny_with_review) — the shipped defaults. So example.org
  // fails FAST here rather than hanging, which is a visible difference from the
  // demo sandbox above.
  //
  // The Confinement/Network cards and their on-card "Unlisted hosts" Seg are
  // GONE — the same rule now lives IN the policy spec, as the Policy card's
  // shipped default (policy-panel.tsx's Minimal template, which is what a
  // fresh /runs/new opens on: first_use_approval: "deny_with_review").
  // Asserted straight off the JSON textarea rather than a radio's
  // aria-checked, because a changed default would make the approval below
  // never fire while the take still went green.
  await expect(
    page.getByLabel("Spec (JSON)"),
    "the default first_use_approval is no longer deny_with_review",
  ).toHaveValue(/"first_use_approval": "deny_with_review"/);

  // Silent launch, same reasoning as the Terminal click above.
  await act(page, page.getByRole("button", { name: "Launch run" }));

  // FAST-FORWARD. POST /runs dispatches SYNCHRONOUSLY (runs_dispatch.go:
  // "dispatch is invoked synchronously from the create-run handler"), so the
  // navigate to /runs/<id> does not happen until the container is provisioned:
  // the URL change and the terminal mount are ONE stretch of dead air, and the
  // URL wait therefore carries SANDBOX_UP, not a minute. Nothing is spoken
  // inside the span — this whole beat stays unnarrated until there is an
  // approval to decide.
  await beat(page, 200);
  await ffwdStart(page);
  try {
    await expect(page).toHaveURL(/\/runs\/[0-9a-f-]{8,}/i, { timeout: SANDBOX_UP });
    await waitUnlessGone(
      expect(page.locator(".xterm-screen").first()).toBeVisible({ timeout: SANDBOX_UP }),
      page.getByText(RUN_OVER).first(),
      SANDBOX_UP,
      "the run never came up as an attached terminal",
    );
  } finally {
    await ffwdEnd(page);
  }
  // The PTY websocket lands a moment after AttachTerminal mounts — see B0.
  await beat(page, PACE.read);
  await typeInTerminal(page, REACH_HELD);

  // The passive-pending header, NOT the held one — the run's own policy says
  // deny-but-ask, so nothing is parked waiting. Asserting the exact header is
  // what makes the narration's "Refused" honest: the held flavour would mean
  // the request is still open, which is a different sentence entirely.
  //
  // RACED against the run's own terminal state: the cockpit mounts the strip
  // only while RUNNING, so a sandbox that dies here takes the header, the row
  // and the entire `always` beat with it.
  await waitUnlessGone(
    expect(page.getByText("Approval needed — off-policy egress")).toBeVisible({
      timeout: APPROVAL_APPEARS,
    }),
    page.getByText(RUN_OVER).first(),
    APPROVAL_APPEARS,
    `${HELD_HOST} never raised an approval on the real-workspace run`,
  );
  await caption(page, "Now we'll save the decision to this workspace.");
  await beat(page, PACE.read + 400);
  await caption(page, "It holds until someone removes it from the workspace — and that removal is a row too.");
  await beat(page, BEAT_SHORT);

  // `always` is the whole point of the video. It is only clickable because THIS
  // run resolves to a workspace (run-detail passes hasWorkspace=runHasWorkspace
  // (run)); were it still disabled, this click would time out loudly rather
  // than narrate over a greyed option. decide() then asserts the row cleared —
  // a 400 from the scope rules would otherwise leave it pending while the
  // narrator says "Always".
  await decide(page, "Approve", "Always.", HELD_HOST, "always");
  await caption(page, "That's the widest yes.");
  await beat(page, PACE.read);
  await caption(page, "And now we can see it on the workspace itself.");
  await beat(page, PACE.read + 400);

  // ---- B7 · Receipt -------------------------------------------------------
  // The sidebar is reachable from the cockpit: focus mode (app-shell.tsx) is
  // opt-in and off unless the canvas's own button turns it on, which nothing
  // here clicks.
  await act(page, page.getByRole("link", { name: "Workspaces" }));
  await act(page, page.getByRole("row", { name: new RegExp(WORKSPACE, "i") }).first());
  await expect(page.getByRole("heading", { name: WORKSPACE, level: 1 })).toBeVisible({ timeout: 30_000 });

  // THE PAYOFF, and the reason beforeAll deletes this workspace: the count is
  // exactly one, the host is the one that was held, and its provenance names
  // where it came from. A stale grant from the last take would read "· 2" here
  // and the receipt would be someone else's.
  const allowed = page.getByText("Allowed hosts · 1");
  await expect(allowed).toBeVisible({ timeout: 30_000 });
  // Ring the ROW, not the "Allowed hosts" heading above it — Morgan: "the
  // single most important frame in the video is obscured by its own
  // subtitle." Center it first (S2): a bottom-of-list row sits exactly under
  // the caption bar otherwise, and a short caption clears the bar sooner too.
  const receiptRow = page.getByRole("listitem").filter({ hasText: HELD_HOST });
  await expect(receiptRow).toBeVisible();
  await expect(receiptRow).toContainText("approved for this workspace");
  await centerInFrame(receiptRow);
  await spotlight(page, receiptRow);
  await caption(page, "The workspace remembers.");
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
  await act(page, page.getByRole("button", { name: "New run" }), "Create another run.");
  await expect(page).toHaveURL(/\/runs\/new/, { timeout: 30_000 });
  await caption(page, "Same workspace.");
  await beat(page, BEAT_SHORT);
  await caption(page, "Same command.");
  await beat(page, BEAT_SHORT);

  const titleBox = page.getByLabel("Title");
  // Fill before ringing — see beat 6's identical fix (S3: point at content).
  await titleBox.fill(PROOF_RUN_TITLE);
  await spotlight(page, titleBox);
  await spotlight(page, null);

  await act(page, page.getByRole("radio", { name: /^Terminal/ }));
  await act(page, page.getByRole("combobox").filter({ hasText: "Ephemeral scratch" }));
  await act(page, page.getByRole("option", { name: new RegExp(WORKSPACE, "i") }).first());

  // Network is DELIBERATELY untouched — that omission IS the beat. The
  // workspace's approved_egress is unioned into this run's allowlist
  // server-side (unionWorkspaceEgress, runs_create.go), so there is nothing to
  // configure. Silent: the owner's lines for this beat land before the form
  // and after the run answers, not on the form or the launch itself.
  await act(page, page.getByRole("button", { name: "Launch run" }));

  // FAST-FORWARD, for beat 6's reason: this create dispatches synchronously too.
  await beat(page, 200);
  await ffwdStart(page);
  try {
    await expect(page).toHaveURL(/\/runs\/[0-9a-f-]{8,}/i, { timeout: SANDBOX_UP });
    await waitUnlessGone(
      expect(page.locator(".xterm-screen").first()).toBeVisible({ timeout: SANDBOX_UP }),
      page.getByText(RUN_OVER).first(),
      SANDBOX_UP,
      "the proof run never came up as an attached terminal",
    );
  } finally {
    await ffwdEnd(page);
  }
  await beat(page, PACE.read);
  await typeInTerminal(page, REACH_HELD);

  // BOTH halves of the claim, asserted. The response proves it got through;
  // the idle hint proves nothing was raised to decide — a pending row here
  // would mean the permanent grant never reached this run's policy, and the
  // closing line's "the decision outlived the run that raised it" would be
  // false.
  // expect.poll over innerText, same reason as B2's payoff: toContainText
  // starved here with the 200 visibly on screen (08-21 rehearsal).
  const proofTerm = page.locator(".xterm-screen").first();
  try {
    await expect
      .poll(async () => (await proofTerm.innerText().catch(() => "<no .xterm-screen>")), {
        timeout: 60_000,
        message: `${HELD_HOST} did not answer on the proof run — the workspace grant never reached this run's allowlist`,
      })
      .toMatch(RESPONDED);
  } catch (e) {
    const over = await page.getByText(RUN_OVER).first().isVisible().catch(() => false);
    const txt = await proofTerm.innerText().catch(() => "<unreadable>");
    console.warn(`[v10] proof-run diagnostic: runOver=${over} innerText=${JSON.stringify(txt.slice(0, 200))}`);
    throw e;
  }
  await expect(
    page.getByTestId("live-approvals-idle"),
    "the proof run RAISED an approval — the permanent grant did not carry into it",
  ).toBeVisible({ timeout: 15_000 });

  // SCREEN: No approval prompt — the idle assertion above IS this beat's proof.
  await caption(page, "And this time, nothing to click.");
  await beat(page, PACE.read);
  await caption(page, "The decision outlived the run that created it.");
  await beat(page, PACE.read);
  await caption(page, "That's what governance that remembers looks like.");
  await beat(page, PACE.read + 600);

  // ---- Conclusion -------------------------------------------------------------
  await caption(page, "Once.");
  await beat(page, BEAT_SHORT);
  await caption(page, "This run.");
  await beat(page, BEAT_SHORT);
  await caption(page, "Until.");
  await beat(page, BEAT_SHORT);
  await caption(page, "Always.");
  await beat(page, BEAT_SHORT);
  await caption(page, "You choose how far one decision is allowed to travel.");
  await beat(page, PACE.read);
  await caption(page, "Next, we're removing the human entirely.");
  await beat(page, PACE.read);
  await caption(page, "Because a CI pipeline — an automated build with nobody watching — can't click Approve.");
  await beat(page, PACE.read + 400);
  await silentCard(page, "Next — 11: CI & headless");
});

/**
 * A chapter card that is NOT spoken — the outro card convention episode 01
 * set (and episode 03 copies). overlay.ts's chapter() always speaks what it
 * renders; this drives the same overlay primitive directly for the one card
 * that must stay silent.
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
