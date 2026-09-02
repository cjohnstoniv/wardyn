/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

/*
 * V12 — Audit & attach. The series finale, and the BROWSER HALF of it.
 *
 * This video is a HYBRID: beats 1-3 are three real ssh terminals and live in
 * scripts/demo-beats/12-audit-and-attach.sh; beats 4-6 are this file. Both
 * lanes run under one `scripts/record-demo.sh --video 12 --terminal-script
 * scripts/demo-beats/12-audit-and-attach.sh` invocation, and record-demo.sh
 * concatenates them TERMINAL FIRST, then the console. So this file opens on
 * state the beat script left behind and must never re-stage it:
 *
 *   - one run, created by the SIGNED-IN HUMAN (never the admin token: sshAuth
 *     compares run.created_by against the key's principal, so a run created by
 *     `admin-token` refuses the owner's own key — sshgateway.go),
 *   - two successful ssh.auth rows for that run (the holder and the read-only
 *     observer, both the owner's own principal, two different keys),
 *   - exactly one ssh.auth FAILURE, reason "not the run owner", written under a
 *     second, genuinely foreign principal whose key IS registered (an
 *     unregistered key would log "unregistered key" instead and the money beat
 *     would be about the wrong thing),
 *   - both ssh sessions DETACHED. session.recording is emitted at detach
 *     (attach.go's finishRecording, and sshgateway_channels.go calls the same
 *     one), so a still-connected ssh client means an empty Session picker at
 *     beat 6 and a video that narrates a tape nobody can play.
 *
 * The beat script asserts every one of those before it exits, and hands this
 * file the run id through ui/test-results/demo-video/v10-run-id.txt. There is
 * still a fallback (see demoRunId) so the browser half can be rehearsed on its
 * own without re-shooting the terminals.
 *
 * This is NOT a test. It asserts only enough to keep itself honest and to know
 * when to advance; a failure here means the recording is wrong, not that the
 * product is broken. It runs against the REAL compose stack on :8080 with the
 * SSH gateway on (WARDYN_SSH_LISTEN=:2222 WARDYN_SSH_ADVERTISE=127.0.0.1:2222)
 * — the hermetic `-runner none` e2e backend has no sandbox to attach to and no
 * gateway to refuse anyone.
 *
 * Beat 6 draws on walkthrough.spec.ts's act 6 ("the receipts"): the chapter
 * card, the Recording-tab click and the closing motif are that act's code,
 * moved rather than rewritten — it is proven on camera.
 *
 * Selectors are getByRole + accessible names, matching ui/e2e/fixtures.ts and
 * the rest of the suite: a copy change breaks this loudly and in one place,
 * markup churn does not break it at all.
 */

import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { test, expect, type Page } from "@playwright/test";
import { act, beat, caption, chapter, PACE, spotlight } from "./overlay";
// The browser, the recorded context and the shared page live in stage.ts:
// importing it is what registers this file's beforeAll/afterAll, and each beat
// reads the page out of stage() rather than closing over a module-level `let`.
import { stage } from "./stage";
import { sweepStaleState } from "./sweep";

// S6 stage hygiene (and the 0.7 onboarded-install mark) — every post-setup
// episode sweeps before rolling; see sweep.ts.
test.beforeAll(async () => {
  if (!process.env.WARDYN_DEMO) return;
  await sweepStaleState();
});

test.skip(!process.env.WARDYN_DEMO, "demo recording — run via `make record-demo` (exports WARDYN_DEMO=1)");

test.describe.configure({ mode: "serial" });

// Ceilings for waiting on the PRODUCT, not pacing. The audit poll is 5s
// (AUDIT_POLL_MS) and a run's recording is fetched lazily on the first
// Recording-tab open, so both are seconds-to-a-minute, not the minutes a
// sandbox start costs elsewhere in the series. Pacing the viewer sees comes
// from overlay.ts.
const AUDIT_SETTLES = 60_000;
const RECORDING_LOADS = 120_000;

/** The owner's staccato lines read fast; PACE.read after one is dead air. */
const BEAT_SHORT = 1400;

// Where the terminal lane leaves the run id it just attacked (written by
// scripts/demo-beats/12-audit-and-attach.sh, beside the lane's own narration
// timeline). Resolved from this file so it does not depend on the process cwd.
const RUN_ID_HANDOFF = path.resolve(
  path.dirname(fileURLToPath(import.meta.url)),
  "../../test-results/demo-video/v10-run-id.txt",
);

/** Authorization header for the direct API reads below, when the stack has one. */
function apiHeaders(): Record<string, string> | undefined {
  // SV1: V10 shoots in LOCAL MODE and deliberately never seeds
  // WARDYN_DEMO_TOKEN — a token-authenticated browser would create runs as
  // `admin-token`, which is exactly the principal that cannot ssh into its own
  // run. Kept for a token-authed rehearsal stack only.
  return process.env.WARDYN_DEMO_TOKEN
    ? { Authorization: `Bearer ${process.env.WARDYN_DEMO_TOKEN}` }
    : undefined;
}

/** GET one of the console's own JSON endpoints, list-unwrapped. */
async function apiList<T>(page: Page, url: string): Promise<T[]> {
  const res = await page.request.get(url, { headers: apiHeaders() });
  if (!res.ok()) return [];
  const body = await res.json().catch(() => null);
  if (Array.isArray(body)) return body as T[];
  return (body?.items ?? []) as T[];
}

interface AuditRow {
  action: string;
  outcome: string;
  target?: string;
  run_id?: string;
  actor?: string;
  data?: Record<string, unknown>;
}

/**
 * WHICH run this video is about.
 *
 * Three sources, strongest first, because getting this wrong films the right
 * UI over the wrong run:
 *  1. WARDYN_DEMO_RUN_ID — the operator naming it outright.
 *  2. The handoff file the terminal lane just wrote (the normal path).
 *  3. The newest run that has an ssh.auth row at all. That is a tighter fallback
 *     than "the newest RUNNING run": ssh.auth only exists because beats 1-3
 *     happened, so it can only ever name the run this take attacked.
 */
async function demoRunId(page: Page): Promise<string> {
  const named = process.env.WARDYN_DEMO_RUN_ID?.trim();
  if (named) return named;

  const handed = fs.existsSync(RUN_ID_HANDOFF) ? fs.readFileSync(RUN_ID_HANDOFF, "utf8").trim() : "";
  if (handed) return handed;

  // Newest-first for the global window (audit.tsx's never-resort invariant), so
  // the first ssh.auth row carrying a run id is the most recent one.
  const rows = await apiList<AuditRow>(page, "/api/v1/audit?action=ssh.auth");
  const withRun = rows.find((e) => !!e.run_id);
  expect(
    withRun?.run_id,
    "no ssh.auth audit row anywhere — beats 1-3 (the terminal lane) never ran, so there is nothing for this half to film",
  ).toBeTruthy();
  return withRun!.run_id!;
}

// ---------------------------------------------------------------------------
// Beat 4 — the trail
// ---------------------------------------------------------------------------

test("beat 4 — the trail", async () => {
  test.setTimeout(360_000);
  const page = stage();

  // Act 6's card, verbatim: the browser half of this video is the receipts half.
  await chapter(page, "The receipts", "Everything above, on the record");

  await page.goto("/audit");
  await expect(page.getByRole("heading", { name: "Audit", level: 1 })).toBeVisible({ timeout: 60_000 });
  // The count badge is the screen's own "status === ready" tell — everything
  // below filters a window that has actually loaded.
  await expect(page.getByText(/^\d+ events?$/)).toBeVisible({ timeout: AUDIT_SETTLES });

  await caption(page, "Now the receipts — the record of everything you just watched.");
  await beat(page, BEAT_SHORT);
  await caption(page, "The record.");
  await beat(page, BEAT_SHORT);
  // The claim the dialog no longer states outright ("grouped by day"), kept as
  // a "the window actually loaded real, grouped content" guard.
  await expect(page.locator("main").getByText("Today").first()).toBeVisible({ timeout: AUDIT_SETTLES });
  await caption(page, "Every important action is written down.");
  await beat(page, PACE.read);
  await caption(page, "And each event tells us who or what caused it.");
  await beat(page, PACE.read);

  // The Actor facet. Options are the three ActorTypes; ssh.auth is written as
  // types.ActorHuman on BOTH the success and the failure path (sshgateway.go),
  // which is the whole reason this facet is the one the beat opens with.
  // SAY-ON-CLICK "Human." rides the OPTION click, not the combobox opener —
  // it names the choice being made, not the act of opening the list.
  await act(page, page.getByRole("combobox", { name: "Actor" }));
  await act(page, page.getByRole("option", { name: "Human", exact: true }), "Human.");
  await caption(page, "Human.");
  await beat(page, BEAT_SHORT);
  await caption(page, "Agent.");
  await beat(page, BEAT_SHORT);
  await caption(page, "System.");
  await beat(page, BEAT_SHORT);
  await caption(page, "The actor is part of the record.");
  await beat(page, PACE.read);

  // Search reads the raw dotted action, not the rendered verb (audit.tsx's
  // filter) — so "ssh.auth" is the honest query even though no row shows it.
  const search = page.getByPlaceholder("Search events, domains, run IDs…");
  await spotlight(page, search);
  await search.fill("ssh.auth");
  await spotlight(page, null);
  await beat(page, PACE.read);

  // Rows are the children of a day group's divider list. There is no test id on
  // an audit row; the group container's own class is the stable handle, and the
  // filter below is what actually picks the ssh rows out.
  //
  // B-DEPENDENT (flagged in the script as "audit facets/chips"): the rendered
  // verb is ACTION_VERB["ssh.auth"] = "an ssh authentication attempt against a
  // run's terminal", capitalized and suffixed with the event's TARGET, which for
  // ssh.auth is the key fingerprint. The script's friction note claiming ssh.*
  // has no ACTION_VERB is STALE — it has one today, which is the only reason
  // this beat reads as prose on camera.
  const sshRows = page.locator("main div.divide-y > div").filter({ hasText: /ssh authentication attempt/ });
  // Two successes and one refusal is the minimum this take can honestly narrate.
  // Not an equality: audit is append-only and `reset-all` belongs to video 01
  // alone (SV20), so a retake legitimately leaves earlier rows in the window.
  await expect(sshRows).not.toHaveCount(0, { timeout: AUDIT_SETTLES });
  expect(
    await sshRows.count(),
    "fewer than three ssh.auth rows — beats 1-3 did not produce two successes and a refusal",
  ).toBeGreaterThanOrEqual(3);

  // THE MONEY ROW. OutcomeBadge renders outcome "failure" as the red "failure"
  // label (primitives.tsx), so this both finds the refusal and proves it is
  // rendered as one — a green take narrating "refused" over a screen of
  // successes is the exact failure this assert exists to prevent.
  const refused = sshRows.filter({ hasText: "failure" }).first();
  await expect(refused).toBeVisible({ timeout: AUDIT_SETTLES });
  // The fingerprint IS the row's target, so "we can see the fingerprint" has
  // to be visible in that row and not merely true in the database.
  await expect(refused).toContainText("SHA256:");
  // FLAGGED (see report): "Open the human event." is a SAY-ON-CLICK in the
  // owner's script, but today's Audit UI has no per-event open/expand
  // affordance — a row's only click target is its run-id chip, which re-scopes
  // the whole list to that run rather than opening this one event. Spoken here
  // as narration over the spotlight rather than an invented click.
  await caption(page, "Here is the human event.");
  await spotlight(page, refused);
  await beat(page, BEAT_SHORT);
  // P10a (dialog review, owner-ratified 2026-08-23): "the refused key" arrived
  // with no antecedent — nothing before it had told the viewer a key WAS
  // refused. The ratified line carries the setup AND the fingerprint/identity
  // content, so the owner line that used to follow it ("We can see the
  // fingerprint and the identity it belongs to.") is folded in here rather
  // than left to stutter two lines later.
  //
  // FLAGGED, and inherited from the folded line: "the identity it belonged to"
  // — event.actor (the principal string) is read by the search box's filter
  // predicate but is never rendered in the row; only the fingerprint (target)
  // is genuinely on screen, so that half is true of the data but not
  // independently provable from the picture the way the fingerprint half is.
  await caption(page, "Here's a key Wardyn refused — its fingerprint, on the row.");
  await beat(page, PACE.read);
  await caption(page, "The key itself is never stored here — only its fingerprint.");
  await beat(page, BEAT_SHORT);
  await caption(page, "The decision is.");
  await beat(page, BEAT_SHORT + 400);
  await spotlight(page, null);
});

// ---------------------------------------------------------------------------
// Beat 5 — name the streams
// ---------------------------------------------------------------------------

test("beat 5 — name the streams", async () => {
  test.setTimeout(180_000);
  const page = stage();

  await caption(page, "There are really three kinds of evidence here.");
  await beat(page, PACE.read);
  await caption(page, "Wardyn's audit log.");
  await beat(page, BEAT_SHORT);
  const live = page.getByText("Live · appending");
  await expect(live).toBeVisible({ timeout: AUDIT_SETTLES });
  await spotlight(page, live);
  await caption(page, "The terminal recording.");
  await beat(page, BEAT_SHORT);
  await caption(page, "And, where supported, a second witness — the machine's own core, watching process and file events.");
  await beat(page, PACE.read);
  await spotlight(page, null);

  // Round-2 dialog review: the plan's first choice here was "a second witness
  // alongside Wardyn's own record", but P10b's ratified line says almost
  // exactly that only TWO spoken lines later (below) — under the plan's own
  // ≥4-line proximity rule this slot therefore takes the alternative, so the
  // series does not say "second witness" twice inside one beat.
  await caption(page, "The kernel can watch from outside the sandbox entirely.");
  await beat(page, PACE.read);
  // GroundTruthChip renders `Ground truth · {state}` off /healthz's
  // ebpf_groundtruth. Two states mean "no sensor feeding this stack":
  // "unavailable" (never opted into the compose profile) and "degraded" (it
  // ran once and went quiet — the heartbeat is PERSISTED, so a host that ever
  // ran the sensor keeps saying degraded even after a wardynd bounce; this
  // box ran it earlier today). FLAGGED (see report): the new line says the
  // sensor "isn't enabled for this barrier", which reads as the never-opted-in
  // case; on a "degraded" take (it WAS enabled and went stale) that phrasing is
  // less precise than the old "dark" wording babe5355 deliberately chose to
  // cover both states. The assert below still accepts either state — that is
  // mechanics, not dialog, and is left as-is. "healthy"/"partial" under this
  // line would be false on camera either way, so those still fail the take.
  const groundTruth = page.getByText(/Ground truth · (unavailable|degraded)/);
  await expect(
    groundTruth,
    "the ground-truth chip reads neither 'unavailable' nor 'degraded' — this take is on a stack with the eBPF sensor LIVE, and the line about the sensor being dark is false on camera",
  ).toBeVisible({ timeout: AUDIT_SETTLES });
  await spotlight(page, groundTruth);
  await caption(page, "On this machine that second witness is switched off — an opt-in sensor — so the chip says so instead of pretending.");
  await beat(page, PACE.read);
  // The "opt-in sensor" hint lives in the chip's native `title` tooltip, which
  // the browser draws as BROWSER chrome — Playwright's recordVideo captures page
  // content only, so the tooltip never lands in the file no matter how long we
  // hover. The hover is kept because it is what a presenter does and costs
  // nothing; the caption is what actually carries the point.
  await groundTruth.hover();
  await beat(page, PACE.read);
  // KEEP-VERIFY: "a second witness alongside Wardyn's own log" — re-check
  // against the sensor docs before the take. Support today:
  // internal/groundtruth/groundtruth.go frames this stream as "the tamper-
  // proof 'ground-truth' counterpart to the agent's own self-report (the
  // Postgres event log) and the human-watchable PTY replay" — drop this line
  // if a sharper read of the docs contradicts it.
  //
  // P10b (dialog review, owner-ratified 2026-08-23): "application-level
  // record" was the only phrase of its kind in the series, and this same line
  // had already called the kernel "an independent witness" — so it now says
  // witness twice on purpose and names the other stream in the series' own
  // words.
  await caption(page, "Where the sensor is available — a host that switched it on, which this one hasn't — that second witness runs alongside Wardyn's own log.");
  await beat(page, PACE.read);
  await caption(page, "A Vault run hides its guest from it — and Wardyn logs that blindness as its own row.");
  await beat(page, PACE.read + 400);
  await spotlight(page, null);
});

// ---------------------------------------------------------------------------
// Beat 6 — the tape, and the series outro
// ---------------------------------------------------------------------------

test("beat 6 — the tape", async () => {
  test.setTimeout(600_000);
  const page = stage();

  const runId = await demoRunId(page);

  // WHICH session to replay, decided BEFORE navigating.
  //
  // The picker is built from this run's session.recording rows in server order
  // (oldest-first for a per-run trail), prefixed by the run's own "Agent
  // session" — so an ssh session's option index is fixed the moment the list is
  // read. It is read here, not after the click, because opening /runs/:id lands
  // on Overview, Overview MOUNTS AttachTerminal, and leaving that tab detaches
  // it — writing a BRAND NEW session.recording row that the 4s detail poll can
  // append to the picker mid-beat. Picking "the last Attached option" would then
  // replay this browser's own five seconds instead of beat 1's keystrokes.
  const rows = await apiList<AuditRow>(page, `/api/v1/audit?run_id=${encodeURIComponent(runId)}&action=session.recording`);
  const recorded = rows.filter((e) => e.outcome === "success" && !!e.target); // attachSessions(), run-detail.tsx
  const sshIndex = recorded.map((e) => e.target!).reduce((last, t, i) => (t.includes("~ssh-") ? i : last), -1);
  expect(
    sshIndex,
    "no ssh attach session was recorded for this run — either beats 1-3 never attached, or an ssh client is still connected (session.recording is written at DETACH)",
  ).toBeGreaterThanOrEqual(0);

  await page.goto(`/runs/${encodeURIComponent(runId)}`);
  const recordingTab = page.getByRole("tab", { name: /Recording/ });
  await expect(recordingTab).toBeVisible({ timeout: 60_000 });

  await act(page, recordingTab, "Open the session.");
  await beat(page, BEAT_SHORT);

  // The picker only renders when the run HAS attach sessions, which the index
  // read above already proved — so its absence here is a UI regression, not a
  // staging miss.
  const picker = page.getByRole("combobox", { name: "Recorded session" });
  await expect(picker).toBeVisible({ timeout: RECORDING_LOADS });
  await act(page, picker);
  // Option 0 is "Agent session"; the run's attach sessions follow in list order.
  await act(page, page.getByRole("option").nth(sshIndex + 1));
  // Options read "Attached <clock> · <principal>" — proof the click landed on a
  // terminal session and not back on the agent's own cast.
  await expect(picker).toContainText("Attached");
  await caption(page, "And finally, the tape.");
  await beat(page, BEAT_SHORT);
  await caption(page, "You can watch it again.");
  await beat(page, BEAT_SHORT);

  // The real asciinema player, mounted with autoPlay:false — so the start
  // overlay is on screen until something clicks it.
  const player = page.locator(".ap-player").first();
  await expect(
    player,
    "the Recording tab has no player — this session's cast never loaded, and 'this is the tape' would narrate over an empty state",
  ).toBeVisible({ timeout: RECORDING_LOADS });

  // "or download the cast": TerminalPlayer's own control. The script's friction
  // note ("no download control in the Recording tab — CLI-only") is STALE; the
  // button exists and is what the line now points at. Deliberately NOT clicked:
  // a real download opens Chromium's download bubble, which is browser chrome
  // sitting over the frame for the rest of the take. The CLI lane is still real
  // and still the way to get a cast off the box:
  //   wardyn run recording <run-id> --session ssh-<uuid> -o session.cast
  const download = page.getByRole("button", { name: "Download recording (.cast)" });
  await expect(download).toBeVisible();
  await spotlight(page, download);
  await caption(page, "Or take the recording with you.");
  await beat(page, PACE.read + 500);
  await spotlight(page, null);

  const start = page.locator(".ap-overlay-start");
  await act(page, start, "Play.");
  // PLAYBACK ACTUALLY STARTED. The start overlay is removed the moment the
  // player leaves its idle state, so this is the difference between filming a
  // replay and filming a still frame with a play button on it.
  await expect(start, "the recording never started playing — the tape beat is a still frame").toHaveCount(0, {
    timeout: 30_000,
  });
  // Long enough for beat 1's keystrokes to actually play back on screen — the
  // dwell is now the sum of the beats below, not one long silent hold.
  await caption(page, "This isn't a summary.");
  await beat(page, BEAT_SHORT);
  await caption(page, "It's the session itself.");
  await beat(page, BEAT_SHORT);
  await caption(page, "And the record ties everything together:");
  await beat(page, PACE.read);
  await caption(page, "the policy that governed the run,");
  await beat(page, BEAT_SHORT);
  await caption(page, "the network decisions,");
  await beat(page, BEAT_SHORT);
  await caption(page, "the approvals and their scope,");
  await beat(page, BEAT_SHORT);
  await caption(page, "the work that happened,");
  await beat(page, BEAT_SHORT);
  await caption(page, "and the people or systems involved.");
  await beat(page, PACE.read + 400);

  // ----- outro (series finale) -----
  // DROPPED (see report): the old "one run, the whole record" detour — a
  // second /audit visit, searched by this run's id, narrating "leaves as a
  // file" — has no line left in the owner's script. The tape section now runs
  // straight into the closing motif, so the navigate/search/assert for that
  // detour is cut along with its captions rather than left to play silently.
  await caption(page, "That's the core of the series.");
  await beat(page, BEAT_SHORT);
  await caption(page, "Admins: 'Admin operations' is next. On a cluster: 'Your terminal, our cluster'.");
  await beat(page, BEAT_SHORT);
  // FACTUAL FIX (dialog round, E12-12): the owner's line said "Ten episodes.",
  // the reorder made it twelve, and the catalog now carries 13 numbered
  // episodes plus lettered detours with 00 in front — so no count is right on
  // camera for long. The line stops counting instead.
  await caption(page, "One series.");
  await beat(page, BEAT_SHORT);
  await caption(page, "One idea.");
  await beat(page, BEAT_SHORT);
  await caption(page, "Give agents the power they need to do useful work...");
  await beat(page, PACE.read);
  await caption(page, "without giving them your credentials.");
  await beat(page, PACE.read);
  await caption(page, "Let them work.");
  await beat(page, BEAT_SHORT);
  await caption(page, "Keep the boundary explicit.");
  await beat(page, BEAT_SHORT);
  await caption(page, "And keep the receipts.");
  await beat(page, PACE.read);
  // The motif closes the series (SV6) — the same words walkthrough.spec.ts act
  // 6 closes on, split across two beats so the four-part line lands as two.
  await caption(page, "Sandboxed. Governed.");
  await beat(page, BEAT_SHORT);
  await caption(page, "Self-hosted. Free.");
  await beat(page, PACE.chapter);
  await caption(page, "");
});
