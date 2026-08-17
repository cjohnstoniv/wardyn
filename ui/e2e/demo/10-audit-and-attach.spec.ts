/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

/*
 * V10 — Audit & attach. The series finale, and the BROWSER HALF of it.
 *
 * This video is a HYBRID: beats 1-3 are three real ssh terminals and live in
 * scripts/demo-beats/10-audit-and-attach.sh; beats 4-6 are this file. Both
 * lanes run under one `scripts/record-demo.sh --video 10 --terminal-script
 * scripts/demo-beats/10-audit-and-attach.sh` invocation, and record-demo.sh
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

test.skip(!process.env.WARDYN_DEMO, "demo recording — run via `make record-demo` (exports WARDYN_DEMO=1)");

test.describe.configure({ mode: "serial" });

// Ceilings for waiting on the PRODUCT, not pacing. The audit poll is 5s
// (AUDIT_POLL_MS) and a run's recording is fetched lazily on the first
// Recording-tab open, so both are seconds-to-a-minute, not the minutes a
// sandbox start costs elsewhere in the series. Pacing the viewer sees comes
// from overlay.ts.
const AUDIT_SETTLES = 60_000;
const RECORDING_LOADS = 120_000;

// Where the terminal lane leaves the run id it just attacked (written by
// scripts/demo-beats/10-audit-and-attach.sh, beside the lane's own narration
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
  test.setTimeout(300_000);
  const page = stage();

  // Act 6's card, verbatim: the browser half of this video is the receipts half.
  await chapter(page, "The receipts", "Everything above, on the record");

  await page.goto("/audit");
  await expect(page.getByRole("heading", { name: "Audit", level: 1 })).toBeVisible({ timeout: 60_000 });
  // The count badge is the screen's own "status === ready" tell — everything
  // below filters a window that has actually loaded.
  await expect(page.getByText(/^\d+ events?$/)).toBeVisible({ timeout: AUDIT_SETTLES });

  await caption(page, "Every attempt is written down, grouped by day.");
  // The claim is "grouped by day", so prove a day header is on screen rather
  // than trusting that groupByDay ran.
  await expect(page.locator("main").getByText("Today").first()).toBeVisible({ timeout: AUDIT_SETTLES });
  await beat(page, PACE.read + 800);

  // The Actor facet. Options are the three ActorTypes; ssh.auth is written as
  // types.ActorHuman on BOTH the success and the failure path (sshgateway.go),
  // which is the whole reason this facet is the one the beat opens with.
  await act(page, page.getByRole("combobox", { name: "Actor" }), "Human, agent or system — every row is stamped.");
  await act(page, page.getByRole("option", { name: "Human", exact: true }));
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

  await caption(page, "There it is: the refused key, by fingerprint.");
  // THE MONEY ROW. OutcomeBadge renders outcome "failure" as the red "failure"
  // label (primitives.tsx), so this both finds the refusal and proves it is
  // rendered as one — a green take narrating "refused" over a screen of
  // successes is the exact failure this assert exists to prevent.
  const refused = sshRows.filter({ hasText: "failure" }).first();
  await expect(refused).toBeVisible({ timeout: AUDIT_SETTLES });
  // The fingerprint IS the row's target, so the narration's "by fingerprint"
  // has to be visible in that row and not merely true in the database.
  await expect(refused).toContainText("SHA256:");
  await spotlight(page, refused);
  await beat(page, PACE.read + 1400);
  await spotlight(page, null);
});

// ---------------------------------------------------------------------------
// Beat 5 — name the streams
// ---------------------------------------------------------------------------

test("beat 5 — name the streams", async () => {
  test.setTimeout(180_000);
  const page = stage();

  await caption(page, "Two streams here: Wardyn's own log, and the terminal replay.");
  const live = page.getByText("Live · appending");
  await expect(live).toBeVisible({ timeout: AUDIT_SETTLES });
  await spotlight(page, live);
  await beat(page, PACE.read + 600);

  await caption(page, "Kernel ground truth is the third — unavailable, because that sensor is opt-in.");
  // GroundTruthChip renders `Ground truth · {state}` off /healthz's
  // ebpf_groundtruth. "unavailable" is the default for every install that has
  // not opted into the sensor's compose profile — and the narration says that
  // word out loud, so assert it rather than filming "healthy" under it.
  const groundTruth = page.getByText("Ground truth · unavailable");
  await expect(
    groundTruth,
    "the ground-truth chip does not read 'unavailable' — this take is on a stack with the eBPF sensor profile up, and the line about an opt-in sensor is false on camera",
  ).toBeVisible({ timeout: AUDIT_SETTLES });
  await spotlight(page, groundTruth);
  // The "opt-in sensor" hint lives in the chip's native `title` tooltip, which
  // the browser draws as BROWSER chrome — Playwright's recordVideo captures page
  // content only, so the tooltip never lands in the file no matter how long we
  // hover. The hover is kept because it is what a presenter does and costs
  // nothing; the caption above is what actually carries the point.
  await groundTruth.hover();
  await beat(page, PACE.read + 1200);
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

  await act(page, recordingTab, "Pick the session. Play it, or download the cast.");
  await beat(page, PACE.read);

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
  await beat(page, PACE.read);

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
  await beat(page, PACE.read + 500);
  await spotlight(page, null);

  const start = page.locator(".ap-overlay-start");
  await act(page, start, "This is the tape, not a summary. Secrets are masked.");
  // PLAYBACK ACTUALLY STARTED. The start overlay is removed the moment the
  // player leaves its idle state, so this is the difference between filming a
  // replay and filming a still frame with a play button on it.
  await expect(start, "the recording never started playing — the tape beat is a still frame").toHaveCount(0, {
    timeout: 30_000,
  });
  // Long enough for beat 1's keystrokes to actually play back on screen.
  await beat(page, PACE.read + 4000);

  // ----- outro (series finale) -----
  await caption(page, "Ten videos, one idea: agents get power, never your credentials.");
  await beat(page, PACE.read + 700);
  await caption(page, "Everything they did stays on the record — yours.");
  await beat(page, PACE.read + 700);
  // The motif closes the series (SV6). walkthrough.spec.ts act 6's last three
  // lines, verbatim.
  await caption(page, "Run anything. Keep your keys.");
  await beat(page, PACE.chapter);
  await caption(page, "");
});
