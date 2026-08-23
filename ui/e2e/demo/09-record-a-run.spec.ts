/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

/*
 * Video 09 of the series — RECORD MODE.
 *
 * The moat feature, filmed end to end: record one open session, let Wardyn
 * synthesize a least-privilege policy from what the kernel and the proxy
 * actually saw, approve the observed hosts, then replay the SAME session
 * confined and reach for a host the recording never saw — on camera, held at
 * the proxy, denied by a human.
 *
 * This is NOT a test. It asserts only enough to keep itself honest and to know
 * when to advance; a failure here means the recording is wrong, not that the
 * product is broken. It runs against the REAL compose stack on :8080 with real
 * sandboxes — the hermetic `-runner none` backend cannot start one at all.
 *
 * WHAT IT INHERITS. The series shoots V4 → V5 → V6 → V7 in one keyless block,
 * so this take opens on a stack that is already set up (the funnel is done, the
 * console lands on /runs, not /setup) and that has NO model connected. That is
 * deliberate: a recorded session needs no model, the "No model provider is
 * configured…" banner fires, and the narration never mentions it. `record-demo`
 * is this video's OWN workspace and nothing else in the series touches it —
 * see the staging block below for the four facts that burn takes.
 *
 * WHAT IT OWNS. Its own workspace (`record-demo`), its own hosts (the canon
 * pair pypi.org + files.pythonhosted.org) and its own policy name
 * (`record-demo-build-test`). It never touches slugify (V01/V02), secrets-lab
 * (V04) or egress-lab (V07), and it deliberately reuses example.com — the one
 * host the whole series agrees means "not on any list" — only as the UNSEEN
 * host in the confined replay.
 *
 * PACING. Four stretches of this video are nothing but waiting on a container
 * or on a server-side reconcile: the recorded session's spin-up, its capture
 * settling after Done recording, the confined replay's spin-up, and that
 * replay's own capture. Each is wrapped in overlay.ts's ffwdStart/ffwdEnd and
 * squeezed 12x by scripts/demo-ffwd.py after assembly. NOTHING MAY SPEAK inside
 * a span — a line spoken over frames the encoder throws away lands 12x early
 * and drags every later cue with it — and each ffwdStart is preceded by a
 * beat(200) that DRAINS the previous line's audio, because caption() only
 * schedules speech (the double-speak bug V05's take-6 verifier caught). Each
 * ffwdEnd fires the instant the awaited state lands, BEFORE the next caption,
 * so every beat the viewer is meant to watch plays at human speed.
 *
 * Driven by `scripts/record-demo.sh --video 09`, which globs this exact
 * filename and names the take wardyn-09-record-a-run-<stamp>.mp4 (docs/README.md
 * already links that asset). Do not rename the file. It self-skips without
 * WARDYN_DEMO=1 so a bare `pnpm e2e` can never point a headed browser at a
 * developer's live stack and start recording sessions in it.
 *
 * The demo project records HEADLESS (playwright.config.ts): the browser records
 * ITSELF, there is no OS window during a take, and nothing here may depend on
 * window geometry. Nothing does — every locator is a role/testid on the page.
 *
 * Selectors are getByRole + accessible name, matching the rest of the suite.
 * Every literal targeted here was read out of ui/src at authoring time:
 * record-pane.tsx (the Sessions card), profile-review.tsx (the Synthesized
 * profile sheet + Save as policy dialog), confirm-egress-dialog.tsx and
 * live-approvals.tsx.
 */

import fs from "node:fs";
import { test, expect, type Page } from "@playwright/test";
import { act, beat, caption, centerInFrame, ffwdEnd, ffwdStart, PACE, spotlight, typeInTerminal } from "./overlay";
// The browser, the recorded context and the shared page live in stage.ts:
// importing it is what registers this file's beforeAll/afterAll, and each beat
// reads the page out of stage() rather than closing over a module-level `let`.
import { stage } from "./stage";
import { APPROVAL_APPEARS, decide } from "./funnel";
import { sweepStaleState } from "./sweep";

test.skip(!process.env.WARDYN_DEMO, "demo recording — run via `make record-demo` (exports WARDYN_DEMO=1)");

test.describe.configure({ mode: "serial" });

// ---------------------------------------------------------------------------
// This video's content. Local on purpose: nothing else in the series records a
// session, so none of it belongs in the shared task.ts.
// ---------------------------------------------------------------------------

/** This video's own workspace. Never shared with another video. */
const WORKSPACE_NAME = "record-demo";

/*
 * Where it lives on the host.
 *
 * MUST sit under the workspaces root `make setup` was given
 * (WARDYN_WORKSPACES_ROOT, defaulted by record-demo.sh to ~/wardyn-demo) or the
 * containerized daemon has no view of the path and the session's bind mount
 * fails at container create. Same resolution order the shell script uses, so an
 * operator who overrode WARDYN_DEMO_ROOT gets a matching path here for free.
 */
const WORKSPACES_ROOT = process.env.WARDYN_DEMO_ROOT || `${process.env.HOME}/wardyn-demo`;
const WORKSPACE_PATH = process.env.WARDYN_DEMO_RECORD_WORKSPACE || `${WORKSPACES_ROOT}/${WORKSPACE_NAME}`;

/** The session's name, and the record_results key the server slugs it to. */
const SESSION_NAME = "build & test";
const SESSION_KEY = "build-test"; // sessionKeyOf("build & test"), session-helpers.ts

/**
 * The saved policy's name. NOT typed on camera: policyNameFor(workspace,
 * session) already derives exactly this (session-helpers.ts), the Save-as
 * dialog opens pre-filled with it, and B4 asserts the pre-fill instead — a
 * stronger claim than watching someone type a string into an empty box.
 */
const POLICY_NAME = "record-demo-build-test";

/**
 * The canon pair. TWO hosts, and the second one is the beat: nobody hand-writes
 * files.pythonhosted.org into an allowlist, because nothing you type mentions
 * it — you only learn it by watching a real install resolve.
 */
const RECORDED_HOSTS = ["pypi.org", "files.pythonhosted.org"] as const;

/** The host the recording never saw — held, then denied, in the confined replay. */
const UNSEEN_HOST = "example.com";

/**
 * The two chips a confined replay can END on (STAGE_CHIP_META, record-pane.tsx).
 *
 * Load-bearing for B5/B6: SessionCard renders the attached terminal, the
 * LiveApprovals strip and the Done button ONLY while sessionStage() is
 * "replaying" (session-helpers.ts). The moment the server flips that capture to
 * recorded/record_failed, the whole branch UNMOUNTS and ConfinedReviewCard
 * takes its place — so every wait aimed at the live half is racing a target
 * that can stop existing. Racing against these two chips turns "poll five
 * minutes for a row that can no longer appear" into an immediate, named
 * failure. (V05 lost a take to exactly this shape, on the run-detail strip.)
 */
const REPLAY_OVER = /^(Replayed confined|Replay failed)$/;

/**
 * The credential-shaped file write, and it has to be credential-shaped.
 * groundtruth/sensitive.go records ONLY writes to credential-shaped paths, so a
 * NOTES.md write never reaches the File-writes list at all and B4's "file
 * writes: kernel ground truth" would narrate over an empty box. `/.gitconfig`
 * is on sensitiveSubstrings; the sandbox contract puts it at
 * /home/agent/.gitconfig (deploy/images/claude-code/Dockerfile).
 */
const GIT_IDENTITY_CMD = "git config --global user.email demo@wardyn.dev";

// Sandboxes are real containers and a capture is reconciled server-side after
// the run terminates, so these are minutes, not seconds. They are ceilings for
// waiting on the PRODUCT; the pacing the viewer sees comes from overlay.ts.
const SANDBOX_UP = 180_000;
const CAPTURE_SETTLES = 240_000;

/** The owner's staccato lines read fast; PACE.read after one is dead air. */
const BEAT_SHORT = 1400;

// ---------------------------------------------------------------------------
// Staging. Everything here is off camera and everything here has burned a take.
// ---------------------------------------------------------------------------

/*
 * THE OPERATOR'S HALF — the two facts no spec can set for itself:
 *
 *  1. THE STACK RUNS THE eBPF GROUND-TRUTH TIER: bring it up with
 *     `--profile groundtruth`. Without the sensor, Executed / File writes /
 *     Connects all render "None observed." and B4's whole middle — "kernel
 *     ground truth, raised as warnings, never as policy" — is spoken over three
 *     empty boxes. beforeAll below PREFLIGHTS this and refuses to roll without
 *     it, because the alternative is finding out 90 seconds into the take.
 *
 *  2. CONTAINERIZED MODE (`WARDYN_SETUP_MODE=container`, record-demo.sh's own
 *     default). On WSL2 host-mode NAT the sandbox cannot call the control plane
 *     back, the capture lands EMPTY, and the review card renders the
 *     reachability warning instead of a recording.
 *
 *  3. REHEARSE ONCE with `--no-record` before burning a take. The one thing no
 *     preflight can settle is whether the CONTROL-PLANE HOST lands in the
 *     approvable set: the client excludes it by comparing observed hosts to
 *     window.location.hostname ("localhost"), while the sandbox reaches it at
 *     WARDYN_CONTROL_PLANE_URL's host ("wardynd" on compose). If those two
 *     disagree AND the session made a brokered call, the button reads "Approve
 *     3 observed hosts" and this video cannot claim "exactly as wide as the
 *     work". B3 fails on it by name within ~2 minutes — see its assertion.
 *
 * The rest — a fresh marker-less directory, a deleted-and-recreated workspace —
 * is code, immediately below.
 */

type Ws = { id?: string; name?: string };

const API = process.env.WARDYN_DEMO_BASE_URL || "http://localhost:8080";
// The default containerized install is local-mode with no auth at all, so this
// is inert there — same shape stage.ts and funnel.ts already use.
const authHeaders = process.env.WARDYN_DEMO_TOKEN
  ? { Authorization: `Bearer ${process.env.WARDYN_DEMO_TOKEN}` }
  : undefined;

/*
 * Staging talks to the API on an ABSOLUTE url, unlike the page navigations
 * below. Two reasons: the DEMO_CDP lane attaches to a context that was never
 * given a baseURL, and — more importantly — a staging call that quietly
 * no-ops is exactly how take 2 films "Approve 2 observed hosts" missing.
 * Everything here throws.
 */
async function apiGet<T>(path: string): Promise<T> {
  const res = await stage().request.get(`${API}${path}`, { headers: authHeaders });
  if (!res.ok()) throw new Error(`staging: GET ${path} → ${res.status()}; the stack is not ready to film`);
  return (await res.json()) as T;
}

test.beforeAll(async () => {
  // Re-guarded, not left to the file-level test.skip: this hook rm -rf's a host
  // directory and DELETES a workspace. One conditional skip must never be the
  // only thing between a developer's stack and that.
  if (!process.env.WARDYN_DEMO) return;
  const page = stage();

  // (0) S6: deny stale pending approvals / kill stale runs first — the
  // Approvals badge otherwise carries a prior take's number through the whole
  // video, and a run still holding the OLD record-demo workspace could make
  // step (2)'s delete race under it.
  await sweepStaleState([WORKSPACE_NAME]);

  // (1) A FRESH DIRECTORY WITH NO ECOSYSTEM MARKERS.
  //
  // This is the single most missable fact in the whole video. A package.json or
  // a pyproject.toml in here is detected at attach time and its ecosystem hosts
  // fold straight into the workspace's requirements — which means the confined
  // replay already allows pypi.org before the recording ever taught it, and the
  // feature demonstrates nothing. An EMPTY directory is the honest starting
  // state: everything the policy ends up knowing was learned on camera.
  fs.rmSync(WORKSPACE_PATH, { recursive: true, force: true });
  fs.mkdirSync(WORKSPACE_PATH, { recursive: true });

  // (2) DELETE AND RE-ADD THE WORKSPACE — the per-take hygiene rule.
  //
  // THE APPROVAL PERSISTS, and not where the obvious reading says. B5's click
  // does NOT write approved_egress: handlePromoteRecordEgress (record.go, "One
  // contract, one place") writes `egress:<host>` REQUIREMENT rows on the
  // workspace overlay and leaves the legacy ApprovedEgress lane read-only —
  // and learnVerifyEgress (approvals.go) writes the same row shape when an
  // approval is decided inside a replay. Both lanes feed
  // egressPromotionDiff()'s `already` set (session-helpers.ts folds
  // effectiveWorkspaceRequirements), so a second take against the same
  // workspace finds pypi.org and files.pythonhosted.org already covered, they
  // bucket as alreadyApproved, the "Approve 2 observed hosts" button never
  // renders, and B5 has no beat. (The confined half never clears on re-record
  // either.) Those rows live on the WORKSPACE row, not on the shared source
  // library entry the path belongs to, so deleting the workspace really is the
  // whole reset — recreating it against the same directory inherits nothing.
  const body = await apiGet<{ items?: Ws[]; workspaces?: Ws[] } | Ws[]>("/api/v1/workspaces");
  const items: Ws[] = Array.isArray(body) ? body : (body.items ?? body.workspaces ?? []);
  for (const w of items) {
    if (w?.id && w.name === WORKSPACE_NAME) {
      const del = await page.request.delete(`${API}/api/v1/workspaces/${w.id}`, { headers: authHeaders });
      if (!del.ok()) throw new Error(`staging: could not delete the previous "${WORKSPACE_NAME}" (${del.status()})`);
    }
  }
  const created = await page.request.post(`${API}/api/v1/workspaces`, {
    headers: authHeaders,
    // The legacy scalar form (kind/source) rather than sources[]: same server
    // path, one line. Left read-only — the recording writes ~/.gitconfig inside
    // the sandbox, never the workspace.
    data: { name: WORKSPACE_NAME, kind: "local_dir", source: WORKSPACE_PATH },
  });
  if (!created.ok()) {
    throw new Error(
      `staging: could not create workspace "${WORKSPACE_NAME}" at ${WORKSPACE_PATH} (${created.status()}). ` +
        `The path must sit under the workspaces root make setup was given (WARDYN_WORKSPACES_ROOT).`,
    );
  }

  // (2b) DELETE THE POLICY THIS TAKE IS ABOUT TO CREATE.
  //
  // run_policies.name is UNIQUE (0001_init.sql). A second take against a stack
  // that still holds "record-demo-build-test" gets a 500 out of createPolicy,
  // the Save-as-policy dialog stays standing with its error, and B4 dies on
  // toBeHidden() three and a half minutes in. Worse, B4's own control-plane
  // check would still pass — on LAST take's policy. Deleting the row is the
  // only thing that makes take 2 byte-identical to take 1.
  const policies = await apiGet<{ id?: string; name?: string }[]>("/api/v1/policies");
  for (const p of Array.isArray(policies) ? policies : []) {
    if (p?.id && p.name === POLICY_NAME) {
      const del = await page.request.delete(`${API}/api/v1/policies/${p.id}`, { headers: authHeaders });
      if (!del.ok()) throw new Error(`staging: could not delete the previous "${POLICY_NAME}" policy (${del.status()})`);
    }
  }

  // (3) THE eBPF SENSOR HAS TO BE ALIVE. "unavailable" means no sensor has ever
  // beaten on this stack — B4's three observation groups would read "None
  // observed." and the render site cannot tell that apart from "the task did
  // nothing", which is precisely the honesty gap this video must not walk into.
  // A freshly-booted stack takes a moment to publish its first heartbeat, hence
  // the poll rather than a single read. ("idle" is fine here: nothing has run
  // yet, so zero observed events is the truth.)
  // INFORMATIONAL ONLY (2026-08-18): B4 no longer narrates the kernel groups,
  // so a quiet sensor no longer blocks the take. The pipeline on this host has
  // a real defect past the sensor — tetragon exports events, the ingest posts
  // batches, the control-plane counter stays frozen — filed for 0.6; until it
  // lands, the series' one on-camera ground-truth mention stays V10's honest
  // "Ground truth · unavailable — that sensor is opt-in."
  const gt = (await apiGet<{ ebpf_groundtruth?: { state?: string } }>("/healthz")).ebpf_groundtruth?.state ?? "unavailable";
  console.log(`[v06] ebpf_groundtruth state: ${gt} (informational — the kernel groups are not narrated)`);
});

// ---------------------------------------------------------------------------
// Cold open + B1 — the card that learns
// ---------------------------------------------------------------------------

test("cold open + B1 — the card that learns", async () => {
  test.setTimeout(180_000);
  const page = stage();
  // Straight to the list, not "/": firstRunLanding only redirects the root, and
  // by this point in the series the funnel is long finished anyway.
  await page.goto("/workspaces");
  await page.bringToFront();

  // Fail here rather than three minutes into a silent, caption-less take: every
  // narration call degrades to a no-op by design, so nothing downstream would
  // ever complain about an overlay that failed to install.
  await expect
    .poll(() => page.evaluate(() => typeof (window as unknown as Record<string, unknown>).__demo), { timeout: 15_000 })
    .toBe("object");

  await caption(page, "Here's the uncomfortable part about allowlists.");
  await beat(page, PACE.read);
  await caption(page, "If you haven't run the job yet, you don't really know what it needs.");
  await beat(page, PACE.read);
  await caption(page, "So don't guess.");
  await beat(page, BEAT_SHORT);
  await caption(page, "Watch it first.");
  await beat(page, BEAT_SHORT);
  await caption(page, "And turn what you observe into policy.");
  await beat(page, PACE.read);

  const workspaceRow = page.getByRole("row", { name: new RegExp(WORKSPACE_NAME, "i") }).first();
  await spotlight(page, workspaceRow);
  await caption(page, "We've got a fresh workspace.");
  await beat(page, PACE.read);
  await caption(page, "And this time we're going to record what a real job actually reaches.");
  await beat(page, PACE.read);
  await spotlight(page, null);

  await act(page, workspaceRow);
  await expect(page.getByRole("heading", { name: WORKSPACE_NAME, level: 1 })).toBeVisible({ timeout: 30_000 });

  // The ring goes on the card HEADER (h2 + its subtitle), not the whole card —
  // the subtitle is the half that says a policy gets learned here.
  //
  // KEYLESS, AND DELIBERATELY UNREAD: this take runs with no model connected,
  // so RecordPane's warning ("No model provider is configured, so an agent
  // won't reach a model in a session…") sits inside this same card, below the
  // ring, for the whole beat. Nothing here asserts on it in either direction —
  // its absence is not required and its presence is not narrated. modelReady
  // gates nothing but that note: Start recording stays enabled either way
  // (record-pane.tsx's NewSessionForm disables only on an in-flight session or
  // a 503 no-runner), which is what makes a keyless recording legal at all.
  const cardHeader = page.getByRole("heading", { name: "Recorded sessions", level: 2 }).locator("xpath=..");
  await spotlight(page, cardHeader);
  await caption(page, "This is where the policy gets learned.");
  await beat(page, PACE.read);
  await caption(page, "Not from a developer's memory.");
  await beat(page, BEAT_SHORT);
  await caption(page, "From the run itself.");
  await beat(page, PACE.read + 400);
  await spotlight(page, null);
});

// ---------------------------------------------------------------------------
// B2 — start a recorded session
// ---------------------------------------------------------------------------

test("B2 — start a recorded session", async () => {
  test.setTimeout(600_000);
  const page = stage();

  // NewSessionForm already pre-fills exactly "build & test" for an empty
  // workspace, so this usually types the string that is already there. Set it
  // anyway: an iteration against a workspace that somehow kept a session finds
  // the box EMPTY (the suggestion is only offered when there are none), and a
  // blank name is a 400 with the dialog still standing.
  const nameBox = page.getByLabel("Session name");
  await spotlight(page, nameBox);
  await nameBox.fill(SESSION_NAME);
  await caption(page, "Give the session a name.");
  await beat(page, PACE.read);
  await spotlight(page, null);

  // A DISABLED button is indistinguishable from a hung app on camera: act()
  // would simply park the ring on it for the full action timeout. The two
  // things that disable it are both real and both name themselves here.
  const start = page.getByRole("button", { name: "Start recording" });
  await expect(
    start,
    "Start recording is disabled — this stack has no runner (-runner none), or another session for this workspace is still running",
  ).toBeEnabled({ timeout: 30_000 });
  await caption(page, "And start recording.");
  await beat(page, PACE.read);
  // SPRINT: no SAY-ON-CLICK line for this click itself. Nothing is spoken from
  // here until the sandbox is up, because everything from here IS the wait.
  await act(page, start);

  // FAST-FORWARD. POST /workspaces/{id}/record dispatches SYNCHRONOUSLY
  // (runs_dispatch.go: "dispatch is invoked synchronously from the create-run
  // handler"), so doRecord's await does not return — and the session card does
  // not render at all — until the container is provisioned and the run is
  // RUNNING. That is 30s-3min of a spinner nobody needs to sit through, which
  // is also why all three waits below carry SANDBOX_UP and not a minute: the
  // card is the SLOW one here, not the terminal inside it.
  await beat(page, 200);
  await ffwdStart(page);

  const card = page.getByTestId(`session-${SESSION_KEY}`);
  await expect(card).toBeVisible({ timeout: SANDBOX_UP });
  await expect(card.getByText("Recording…")).toBeVisible({ timeout: SANDBOX_UP });
  await expect(card.locator(".xterm-screen").first()).toBeVisible({ timeout: SANDBOX_UP });

  // The sandbox is up: real time resumes here, before a word is spoken.
  await ffwdEnd(page);

  // The open-egress banner is the honest half of "nothing is denied while
  // recording": every open recording allows ALL egress while it learns, and
  // the card says so on every tier (the weakest-barrier line is added only
  // when the session genuinely runs under CC1). Point at it if rendered.
  const banner = page.getByTestId("record-open-egress-banner");
  if (await banner.isVisible().catch(() => false)) await spotlight(page, banner);
  await caption(page, "Recording is intentionally broad.");
  await beat(page, PACE.read);
  await caption(page, "We're observing the job before we constrain it.");
  await beat(page, PACE.read);
  await caption(page, "That makes this a privileged operation, and the product says so right on the screen.");
  await beat(page, PACE.read + 400);
  await spotlight(page, null);

  // launch.warnings ALWAYS carries recordMaskingCaveat (record.go: appended
  // unconditionally, every tier) — a second, separate amber box next to the
  // banner above, and just as unread on the take.
  const launchWarnings = page.getByTestId("record-launch-warnings");
  if (await launchWarnings.isVisible().catch(() => false)) await spotlight(page, launchWarnings);
  await caption(page, "Only record work you trust.");
  await beat(page, BEAT_SHORT);
  await caption(page, "Because the recording itself can contain sensitive information.");
  await beat(page, PACE.read + 400);
  await spotlight(page, null);

  await caption(page, "While we're recording, ordinary policy stops blocking — only the hard walls stay.");
  await beat(page, PACE.read);
  await caption(page, "Every host the job reaches is captured at the proxy.");
  await beat(page, PACE.read + 600);
});

// ---------------------------------------------------------------------------
// B3 — honest small work
// ---------------------------------------------------------------------------

test("B3 — honest small work", async () => {
  test.setTimeout(900_000);
  const page = stage();
  const card = page.getByTestId(`session-${SESSION_KEY}`);
  const screen = card.locator(".xterm-screen").first();

  // .xterm-screen went visible in B2 when AttachTerminal MOUNTED; the PTY
  // websocket lands a moment later, and a keystroke sent before it is dropped
  // outright (attach-terminal.tsx only sends on readyState OPEN). B2's own
  // banner beat has already spent that moment — this line is here so a future
  // edit that shortens B2 knows what it is spending.
  await caption(page, "Let's give it a small piece of real work.");
  await beat(page, PACE.read);
  await caption(page, "A Git identity.");
  await beat(page, BEAT_SHORT);
  await typeInTerminal(page, GIT_IDENTITY_CMD, card);
  await caption(page, "Then two network destinations.");
  await beat(page, PACE.read);

  // SCREEN-TYPE: First curl.
  await typeInTerminal(page, `curl -sS -o /dev/null -w '%{http_code}\\n' https://${RECORDED_HOSTS[0]}/`, card);
  await beat(page, PACE.read);
  await caption(page, "This is one of the hosts the build actually needs.");
  await beat(page, PACE.read + 400);

  // SCREEN-TYPE: Second curl. Concurrent, not sequential: a caption spoken
  // BEFORE this line typed (the original shape) put the punchline several
  // seconds ahead of the second host actually existing on screen — Sam/Dana's
  // "fires before it exists" finding. caption()'s own cue timestamp is
  // stamped when it is CALLED, so firing it alongside the keystrokes (instead
  // of awaiting it first) is what makes the line land while the second host
  // is actually being typed.
  await Promise.all([
    caption(page, "And here's the kind of thing a hand-written allowlist gets wrong."),
    typeInTerminal(page, `curl -sS -o /dev/null -w '%{http_code}\\n' https://${RECORDED_HOSTS[1]}/`, card),
  ]);
  await beat(page, PACE.read);

  // BOTH hosts must actually answer. Counting bare status-code lines rather
  // than matching "200": what this beat has to prove is that the request
  // reached the host and came back through the proxy, and
  // files.pythonhosted.org's bare root is entitled to answer 403/404 — a curl
  // that never resolved prints no code line at all, records no egress, and
  // B5's "Approve 2 observed hosts" then never appears at all. `-w
  // '%{http_code}\n'` (not `-I`'s full header dump) so each reply is one
  // line — the old header wall was long enough to bury the second host's own
  // line underneath it.
  await expect
    .poll(async () => ((await screen.innerText()).match(/^\d{3}$/gm) ?? []).length, { timeout: 90_000 })
    .toBeGreaterThanOrEqual(2);

  // S2: both replies landed at the tail of a scrolling terminal — center it
  // before speaking over it, or the second host's own line sits under the bar.
  await centerInFrame(screen);
  await caption(page, "Python packages don't necessarily come from the host you first think of.");
  await beat(page, PACE.read);
  await caption(page, "You might reach PyPI to find the package...");
  await beat(page, BEAT_SHORT);
  await caption(page, "and then reach files.pythonhosted.org to download it.");
  await beat(page, PACE.read);
  await caption(page, "The recording sees both.");
  await beat(page, PACE.read);

  await act(page, card.getByRole("button", { name: "Done recording" }), "Done recording.");
  await caption(page, "When we stop, Wardyn builds the session from the audit trail.");
  await beat(page, PACE.read);
  await caption(page, "Not from a guess about what happened inside the sandbox.");
  await beat(page, PACE.read);

  // FAST-FORWARD. The run has to die, and only THEN does the server reconcile
  // its capture out of the audit trail; the page polls for it (workspace-detail
  // polls while isRecording(ws)). Minutes of a "Recording…" chip nobody needs
  // to watch stop pulsing.
  await beat(page, 200);
  await ffwdStart(page);

  // AN EMPTY CAPTURE IS A FAILED TAKE, and it looks exactly like a good one
  // until someone reads the card: RecordReviewCard swaps the whole review for
  // the reachability warning (record-empty-capture), every later beat degrades,
  // and the narration keeps claiming a policy was learned. So RACE the two
  // outcomes rather than waiting four minutes for the good one and then
  // discovering the bad one had been on screen the whole time — the two are
  // mutually exclusive branches of the same component, so whichever resolves
  // first IS the verdict.
  const review = card.getByTestId("record-review");
  const emptyCapture = card.getByTestId("record-empty-capture");
  const captured = await Promise.race([
    review.waitFor({ state: "visible", timeout: CAPTURE_SETTLES }).then(
      () => "recorded" as const,
      () => "timeout" as const,
    ),
    emptyCapture.waitFor({ state: "visible", timeout: CAPTURE_SETTLES }).then(
      () => "empty" as const,
      () => "timeout" as const,
    ),
  ]);
  // Real time resumes the instant the verdict lands, before anything is said.
  await ffwdEnd(page);
  expect(
    captured,
    captured === "empty"
      ? `the recording captured NO egress — "${await emptyCapture.innerText().catch(() => "")}". ` +
        `On WSL2 host-mode NAT the sandbox cannot call the control plane back and every capture lands empty: ` +
        `shoot this video in CONTAINERIZED mode (WARDYN_SETUP_MODE=container, record-demo.sh's own default).`
      : `the session never settled into a review card within ${CAPTURE_SETTLES / 1000}s of Done recording`,
  ).toBe("recorded");
  await expect(card.getByText("Recorded", { exact: true })).toBeVisible({ timeout: 30_000 });

  const newHosts = card.getByTestId("record-new-hosts");
  await expect(newHosts).toContainText(RECORDED_HOSTS[0]);
  await expect(newHosts).toContainText(RECORDED_HOSTS[1]);
  // EXACTLY TWO, asserted HERE rather than only at B5's "Approve 2 observed
  // hosts" button: this list IS that button's count (both read
  // egressPromotionDiff().approvable). Asserted on the host NAMES rather than a
  // count, because the count alone cannot say which of the two causes it is:
  //
  //  - the workspace directory was not marker-free, so a scan folded an
  //    ecosystem registry into profile.egress_domains; or
  //  - the CONTROL-PLANE HOST leaked into the approvable bucket. Every capture
  //    contains one (the sandbox's brokered uploads emit a real egress.allow
  //    whose host is WARDYN_CONTROL_PLANE_URL's — "wardynd" on the compose
  //    stack). The server excludes it by name; the CLIENT excludes it by
  //    comparing against window.location.hostname, which on a published-port
  //    install is "localhost". Those two disagree, and when they do this list
  //    grows a third row nobody can honestly approve on camera.
  //
  // Failing here fails two beats and ~2 minutes of shooting earlier than B5's
  // button regex would, and it NAMES the extra host instead of a count.
  expect(
    (await newHosts.locator("li").allTextContents()).map((s) => s.trim()).sort(),
    "the approvable set is not exactly the canon pair — see the two causes above",
  ).toEqual([...RECORDED_HOSTS].sort());
  await beat(page, PACE.read + 900);
});

// ---------------------------------------------------------------------------
// B4 — evidence becomes policy
// ---------------------------------------------------------------------------

test("B4 — evidence becomes policy", async () => {
  test.setTimeout(600_000);
  const page = stage();
  const card = page.getByTestId(`session-${SESSION_KEY}`);

  await act(page, card.getByRole("button", { name: "Save session profile" }), "Save session profile.");

  // Two locators for one drawer, deliberately. `sheet` scopes content while it
  // is the only dialog on screen; `sheetTitle` is a TEXT locator, and it is the
  // one the closing assertion uses — Radix aria-hides the sheet behind a nested
  // dialog, so a role-based hidden check would pass while the drawer is still
  // standing there in frame (the exact bug this beat used to film).
  const sheetTitle = page.getByText("Synthesized profile", { exact: true });
  const sheet = page.getByRole("dialog").filter({ hasText: "Synthesized profile" });
  await expect(sheetTitle).toBeVisible({ timeout: 60_000 });
  // The body only renders once POST /runs/{id}/profile answers.
  await expect(sheet.getByText("Proposed allowed domains", { exact: true })).toBeVisible({ timeout: 60_000 });
  await caption(page, "Now we're looking at the evidence.");
  await beat(page, PACE.read);

  // The synthesis is allow-only and it is exactly the two hosts that were
  // reached — the claim the whole video rests on.
  const proposed = sheet.getByText("Proposed allowed domains", { exact: true }).locator("xpath=..");
  await expect(proposed).toContainText(RECORDED_HOSTS[0]);
  await expect(proposed).toContainText(RECORDED_HOSTS[1]);
  await spotlight(page, proposed);
  await caption(page, "These are the domains the job actually reached.");
  await beat(page, PACE.read);
  await spotlight(page, null);

  const observations = sheet.locator('[aria-label="Observations"]');
  // S3: ring the egress-domains CARD specifically, not the whole Observations
  // block (which also holds Minted grants / Executed / File writes /
  // Connects) — anchored on a host row already on screen, since the section's
  // own heading shares a div with an icon and a count badge, not a stable
  // text node on its own.
  const egressSection = observations
    .getByText(RECORDED_HOSTS[0])
    .locator("xpath=ancestor::div[contains(@class,'rounded-lg')][1]");
  await spotlight(page, egressSection);
  await caption(page, "How often it reached them.");
  await beat(page, BEAT_SHORT);
  await caption(page, "And where those observations came from.");
  await beat(page, PACE.read + 400);
  await spotlight(page, null);

  await caption(page, "The network observations come from the proxy.");
  await beat(page, PACE.read);
  // The kernel groups (Executed / File writes / Connects) are narrated as
  // UNRELIABLE, not as evidence: the ground-truth ingest pipeline on this host
  // posts events the control plane never counts (filed for 0.6 with the full
  // evidence trail), so those boxes read "None observed." — and a beat that
  // calls an empty box "kernel ground truth" is the exact honesty gap this
  // video must not walk into. Ring the whole Observations block while the
  // kernel counters are named — the egress domains above are inside it too.
  await spotlight(page, observations);
  await caption(page, "The kernel counters tell us what this particular barrier can see.");
  await beat(page, PACE.read);
  await caption(
    page,
    "And if the kernel can't observe something, Wardyn doesn't pretend that “none observed” means “none happened.”",
  );
  await beat(page, PACE.read + 400);
  await caption(page, "That's an important distinction.");
  await beat(page, BEAT_SHORT);
  await spotlight(page, null);

  // "Eligible grants: none" in the SUMMARY is real — but not because nothing
  // was minted. The session DID mint one credential; the WARNINGS bullet is
  // where the actual reason lives (a dropped grant, not on the operator's
  // eligible list), and "none eligible" is what survives into the proposed
  // policy.
  const grants = sheet.getByText("Eligible grants", { exact: true }).locator("xpath=..");
  await expect(grants).toContainText("none");
  const warningsBullet = sheet.getByText("Warnings", { exact: true }).locator("xpath=..").locator("li").first();
  await expect(warningsBullet).toBeVisible({ timeout: 30_000 });
  await spotlight(page, grants);
  await caption(page, "The synthesizer also looks at credentials.");
  await beat(page, PACE.read);
  await spotlight(page, warningsBullet);
  await caption(page, "If a credential was used but isn't eligible for policy, it doesn't simply turn that observation into a new grant.");
  await beat(page, PACE.read + 400);
  await caption(page, "The result is a policy that reflects the work without blindly widening access.");
  await beat(page, PACE.read);
  await spotlight(page, null);

  // Both halves of the next line, before it is spoken: allow-all is off, and an
  // unseen host has to ask (Synthesize forces deny_with_review → "Ask").
  const allowedDomains = sheet.getByText("Allowed domains", { exact: true }).locator("xpath=..");
  await expect(allowedDomains).not.toContainText("Allow all");
  await expect(sheet.getByText("First-use approval", { exact: true }).locator("xpath=..")).toContainText("Ask");

  // "Save as is" skips the name dialog entirely — profile-review.tsx's
  // saveAsIs() posts straight to POST /policies under suggestedName
  // (policyNameFor(workspace, session), the same derivation the dialog path
  // used to only PRE-FILL) and calls onClose() itself. One fewer dialog; the
  // name is still the honest, derived one — now spoken on the button's own
  // label instead of behind a second click.
  const saveAsIsBtn = sheet.getByTestId("profile-save-as-is");
  await expect(saveAsIsBtn).toContainText(POLICY_NAME);
  await act(page, saveAsIsBtn, "Save as policy.");
  await expect(sheetTitle).toBeHidden({ timeout: 30_000 });
  await caption(page, "And allow-all stays off.");
  await beat(page, PACE.read);
  await caption(page, "Anything we didn't observe has to ask.");
  await beat(page, PACE.read);

  // "Save as policy" is a claim about a WRITE. Ask the control plane.
  // A bare array, same as beforeAll reads it: servePage writeJSONs the slice
  // itself, with no envelope (internal/api/runs_policy.go).
  const policies = await apiGet<{ name?: string }[]>("/api/v1/policies");
  const named = policies.some((p) => p?.name === POLICY_NAME);
  expect(named, `the policy "${POLICY_NAME}" is not on the control plane — the save failed on camera`).toBe(true);
});

// ---------------------------------------------------------------------------
// B5 — replay confined
// ---------------------------------------------------------------------------

test("B5 — replay confined", async () => {
  test.setTimeout(900_000);
  const page = stage();
  const card = page.getByTestId(`session-${SESSION_KEY}`);

  // EXACTLY TWO. The count is the take's own smoke alarm — see B3, which
  // already failed on the same list by NAME two beats ago if it were wrong.
  // "exactly as wide as the work" is the claim the whole video rests on, so
  // this stays a literal 2 and never a \d+.
  const approveObserved = card.getByRole("button", { name: /^Approve 2 observed hosts$/ });
  await expect(approveObserved).toBeVisible({ timeout: 30_000 });
  await act(page, approveObserved, "Approve the observed hosts.");

  // Host names came from a session's observed traffic, so promotion routes
  // through the shared untrusted-content confirm.
  const confirm = page.getByRole("alertdialog");
  await expect(confirm).toBeVisible({ timeout: 30_000 });
  await expect(confirm).toContainText(RECORDED_HOSTS[0]);
  await expect(confirm).toContainText(RECORDED_HOSTS[1]);
  await caption(page, "Now we take those observed destinations and promote them into the workspace's standing policy.");
  await beat(page, PACE.read + 400);
  await caption(page, "That's why we're asked to approve them.");
  await beat(page, PACE.read);
  await caption(page, "This is the moment evidence becomes permission.");
  await beat(page, PACE.read + 600);
  await act(page, confirm.getByRole("button", { name: "Approve hosts" }));
  // The receipt for the click, before the replay that depends on it: without a
  // promoted set, confinedEgressDomains() for a local_dir workspace is EMPTY
  // and both curls below would be held, not allowed.
  await expect(card.getByText("Promoted", { exact: true })).toBeVisible({ timeout: 60_000 });
  await beat(page, PACE.read);

  // launchRecordRun's replay does NOT read the saved policy at all (no
  // policy_id lookup) — it builds a fresh RunPolicySpec inline straight off
  // the WORKSPACE: AllowedDomains = confinedEgressDomains(ws) (clone hosts ∪
  // profile/approved egress ∪ required-egress rows — workspace_egress.go),
  // FirstUseApproval hardcoded to wait_for_review when confined
  // (workspace_run.go's launchRecordRun). So "the policy governs the replay"
  // is false on this stack; what actually governs it is the workspace's own
  // approved-hosts list B5 just widened above — which is exactly what the
  // owner's lines below say ("workspace's standing policy", never "the saved
  // policy").
  await act(page, card.getByRole("button", { name: "Replay confined" }), "Replay confined.");

  // FAST-FORWARD, for B2's reason: this POST dispatches synchronously too, so
  // the card does not flip to "Replaying confined…" until the confined sandbox
  // is up.
  await beat(page, 200);
  await ffwdStart(page);

  const screen = card.locator(".xterm-screen").first();
  // RACED against the two chips that mean the replay is already OVER. A replay
  // that never came up (image gone, sandbox died on start) settles straight to
  // replay_failed, SessionCard renders ConfinedReviewCard instead of the
  // terminal, and a bare wait on .xterm-screen would poll three minutes for a
  // node that can no longer exist. Same shape B6's held-row wait uses.
  const live = await Promise.race([
    screen.waitFor({ state: "visible", timeout: SANDBOX_UP }).then(
      () => "replaying" as const,
      () => "timeout" as const,
    ),
    card
      .getByText(REPLAY_OVER)
      .waitFor({ state: "visible", timeout: SANDBOX_UP })
      .then(
        () => "over" as const,
        () => "timeout" as const,
      ),
  ]);
  // The terminal is on screen: real time resumes before a word is spoken.
  await ffwdEnd(page);
  expect(
    live,
    "the confined replay never came up as a live session — it settled (or failed) before B5 could type into it; " +
      "check the workspace's base image and the runner, then re-run the take",
  ).toBe("replaying");

  await caption(page, "Same workspace.");
  await beat(page, BEAT_SHORT);
  await caption(page, "Same work.");
  await beat(page, BEAT_SHORT);
  await caption(page, "But now we're back behind default-deny.");
  await beat(page, PACE.read);

  // .xterm-screen renders when AttachTerminal MOUNTS, before the PTY websocket
  // is up — typing here eats the first characters and the shell reports
  // "command not found" on camera.
  await beat(page, PACE.read);

  // SCREEN-TYPE: First host.
  await typeInTerminal(page, `curl -sSI https://${RECORDED_HOSTS[0]}`, card);
  await beat(page, PACE.read);
  await caption(page, "Allowed.");
  await beat(page, BEAT_SHORT);

  // SCREEN-TYPE: Second host.
  await typeInTerminal(page, `curl -sSI https://${RECORDED_HOSTS[1]}`, card);
  await expect
    .poll(async () => ((await screen.innerText()).match(/HTTP\/[0-9.]+ \d{3}/g) ?? []).length, { timeout: 90_000 })
    .toBeGreaterThanOrEqual(2);
  await caption(page, "Also allowed.");
  await beat(page, BEAT_SHORT);
  await caption(page, "Nothing else needed a decision.");
  // "nothing else needed a decision" is the whole point of the beat, so prove
  // it: the idle hint, not a pending row. A held host here would mean the
  // promotion did not land, and the line would be narrating over a queue.
  await expect(card.getByTestId("live-approvals-idle")).toBeVisible();
  await beat(page, PACE.read + 900);
});

// ---------------------------------------------------------------------------
// B6 — the unseen host, and the outro
// ---------------------------------------------------------------------------

test("B6 — the unseen host", async () => {
  test.setTimeout(900_000);
  const page = stage();
  const card = page.getByTestId(`session-${SESSION_KEY}`);
  const screen = card.locator(".xterm-screen").first();

  await caption(page, "Now let's try something the recording never saw.");
  await beat(page, PACE.read);
  await typeInTerminal(page, `curl -sSI --max-time 60 https://${UNSEEN_HOST}`, card);

  // HELD, not refused: a confined replay runs wait_for_review, so the
  // connection parks at the proxy while the strip waits on a person. The header
  // is what proves it is a hold rather than a fast denial — the difference the
  // next line is entirely about.
  //
  // RACED against the replay ending, because the strip unmounts with it: the
  // LiveApprovals panel lives inside SessionCard's `replaying` branch only, so
  // a replay that goes terminal for any reason (a dead sandbox, an auto-stop,
  // a stray kill) takes the row, the terminal and the Done button with it. The
  // on-camera decision IS this video; the honest outcome then is a fast loud
  // failure naming that, not five minutes polling for a header that can no
  // longer render (the exact way a V05 take died on the run-detail strip).
  const heldHeader = card.getByText("Sandbox is waiting — approve to let it through");
  const held = await Promise.race([
    heldHeader.waitFor({ state: "visible", timeout: APPROVAL_APPEARS }).then(
      () => "held" as const,
      () => "timeout" as const,
    ),
    card
      .getByText(REPLAY_OVER)
      .waitFor({ state: "visible", timeout: APPROVAL_APPEARS })
      .then(
        () => "over" as const,
        () => "timeout" as const,
      ),
  ]);
  expect(
    held,
    `${UNSEEN_HOST} never surfaced as a held request while the replay was still live — ` +
      `the replay ended (or the wait timed out) before the on-camera decision. ` +
      `The whole video is that decision; re-run the take.`,
  ).toBe("held");

  await caption(page, "This host wasn't part of the recorded work.");
  await beat(page, PACE.read);
  await caption(page, "So it isn't part of the policy.");
  await beat(page, PACE.read);

  // Decide it on camera and QUICKLY. The hold expires after 30s
  // (defaultHoldTimeout); decide() speaks one line and clicks inside ~6s, which
  // is the point — a demo that waits out a clock is a bad demo, and an expired
  // hold films as a timeout nobody decided.
  await decide(card, "Deny", "Deny.", UNSEEN_HOST);
  await beat(page, PACE.read);

  // The refusal has to be VISIBLE, not just true: the deny lands at the proxy,
  // then the parked curl itself has to resolve as a failure on screen — a 403
  // body, curl's own connection error, or (if the hold's teardown races the
  // request) a bare timeout. REHEARSAL-VERIFY: confirm which of the three this
  // stack actually prints before the take; if the sandbox genuinely sees
  // nothing at all (a silent deny), say THAT instead of promising a receipt
  // that never renders (Priya: "that's a better fact than an error message").
  await expect(screen).toContainText(/403|curl: \(\d+\)|timed out/, { timeout: 45_000 });
  await spotlight(page, screen);
  await caption(page, "Held.");
  await beat(page, BEAT_SHORT);
  await caption(page, "Denied.");
  await beat(page, BEAT_SHORT);
  await caption(page, "And the command fails with that decision.");
  await beat(page, PACE.read + 400);
  await spotlight(page, null);

  await act(page, card.getByRole("button", { name: "Done", exact: true }));

  // FAST-FORWARD, for B3's reason: the confined run has to die and its capture
  // has to be reconciled server-side before the containment review exists.
  await beat(page, 200);
  await ffwdStart(page);
  const review = card.getByTestId("verify-session-review");
  const reviewOrFailed = await Promise.race([
    review.waitFor({ state: "visible", timeout: CAPTURE_SETTLES }).then(
      () => "review" as const,
      () => "timeout" as const,
    ),
    card
      .getByTestId("verify-session-failed")
      .waitFor({ state: "visible", timeout: CAPTURE_SETTLES })
      .then(
        () => "failed" as const,
        () => "timeout" as const,
      ),
  ]);
  // The review is on screen: real time resumes before the closing lines.
  await ffwdEnd(page);
  expect(
    reviewOrFailed,
    "the confined replay captured no egress decisions — the containment proof the closing line names does not exist. " +
      "Same cause as B3's empty capture: shoot in containerized mode.",
  ).toBe("review");

  // The two halves the closing line claims. The allowed count is a floor, not
  // an equality: the control plane's own host is legitimately reachable from a
  // confined replay and is counted here (it is only excluded from what an
  // operator can APPROVE), so pinning it to 2 would fail on a truthful frame.
  await expect(review).toContainText(/[2-9]\d* hosts reached, all allowed/);
  const blocked = card.getByTestId("verify-session-blocked");
  await expect(blocked).toContainText(UNSEEN_HOST);
  await expect(blocked).toContainText("blocked");

  await spotlight(page, review);
  await caption(page, "That's the whole loop.");
  await beat(page, PACE.read);
  await caption(page, "Watch the job.");
  await beat(page, BEAT_SHORT);
  await caption(page, "Capture what it actually uses.");
  await beat(page, PACE.read);
  await caption(page, "Turn that evidence into policy.");
  await beat(page, PACE.read);
  await caption(page, "Then run it confined.");
  await beat(page, PACE.read + 400);
  await spotlight(page, null);
  await caption(page, "You don't have to predict the future.");
  await beat(page, PACE.read);
  await caption(page, "You have to observe the work and make the boundary explicit.");
  await beat(page, PACE.read + 600);

  await caption(page, "Watch it first.");
  await beat(page, BEAT_SHORT);
  await caption(page, "Enforce it from then on.");
  await beat(page, PACE.read);
  await caption(page, "Next, we tackle the question that comes after every approval:");
  await beat(page, PACE.read);
  await caption(page, "How long should that yes last?");
  await beat(page, PACE.read + 400);
  await silentCard(page, "Next — 10: Approval scopes");
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
