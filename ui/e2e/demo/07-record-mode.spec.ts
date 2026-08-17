/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

/*
 * Video 07 of the 0.5 series — RECORD MODE.
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
 * (V04) or egress-lab (V05), and it deliberately reuses example.com — the one
 * host the whole series agrees means "not on any list" — only as the UNSEEN
 * host in the confined replay.
 *
 * Driven by `scripts/record-demo.sh --video 07`, which globs this exact
 * filename and names the take wardyn-07-record-mode-<stamp>.mp4 (docs/README.md
 * already links that asset). Do not rename the file. It self-skips without
 * WARDYN_DEMO=1 so a bare `pnpm e2e` can never point a headed browser at a
 * developer's live stack and start recording sessions in it.
 *
 * Selectors are getByRole + accessible name, matching the rest of the suite.
 * Every literal targeted here was read out of ui/src at authoring time:
 * record-pane.tsx (the Sessions card), profile-review.tsx (the Synthesized
 * profile sheet + Save as policy dialog), confirm-egress-dialog.tsx and
 * live-approvals.tsx.
 */

import fs from "node:fs";
import { test, expect } from "@playwright/test";
import { act, beat, caption, PACE, spotlight, typeInTerminal } from "./overlay";
// The browser, the recorded context and the shared page live in stage.ts:
// importing it is what registers this file's beforeAll/afterAll, and each beat
// reads the page out of stage() rather than closing over a module-level `let`.
import { stage } from "./stage";
import { APPROVAL_APPEARS, decide } from "./funnel";

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
  // approved_egress PERSISTS. A second take against the same workspace finds
  // pypi.org and files.pythonhosted.org already approved, so egressPromotionDiff
  // buckets them as alreadyApproved, the "Approve 2 observed hosts" button never
  // renders, and B5 has no beat. (The confined half never clears on re-record
  // either.) Deleting the row is the only thing that resets it.
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
  await expect
    .poll(
      async () => (await apiGet<{ ebpf_groundtruth?: { state?: string } }>("/healthz")).ebpf_groundtruth?.state ?? "unavailable",
      {
        timeout: 60_000,
        message:
          "no eBPF ground-truth sensor on this stack — bring it up with `--profile groundtruth`, " +
          "or B4 films three boxes reading “None observed.” while the narration calls them kernel ground truth",
      },
    )
    .not.toBe("unavailable");
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

  await caption(page, "Nobody writes a correct allowlist for a task they have never run.");
  await beat(page, PACE.read + 600);
  await caption(page, "So don't. Run it once, watched, and let Wardyn write the policy.");
  await beat(page, PACE.read + 600);

  await act(page, page.getByRole("row", { name: new RegExp(WORKSPACE_NAME, "i") }).first());
  await expect(page.getByRole("heading", { name: WORKSPACE_NAME, level: 1 })).toBeVisible({ timeout: 30_000 });

  // The ring goes on the card HEADER (h2 + its subtitle), not the whole card —
  // the subtitle is the half that says a policy gets learned here.
  const cardHeader = page.getByRole("heading", { name: "Recorded sessions", level: 2 }).locator("xpath=..");
  await spotlight(page, cardHeader);
  await caption(page, "Open the workspace. Recorded sessions is where a policy gets learned, not guessed.");
  await beat(page, PACE.read + 900);
  await spotlight(page, null);
});

// ---------------------------------------------------------------------------
// B2 — start a recorded session
// ---------------------------------------------------------------------------

test("B2 — start a recorded session", async () => {
  test.setTimeout(600_000);
  const page = stage();

  await caption(page, "Name the session after the work it does, then Start recording.");
  // NewSessionForm already pre-fills exactly "build & test" for an empty
  // workspace, so this usually types the string that is already there. Set it
  // anyway: an iteration against a workspace that somehow kept a session finds
  // the box EMPTY (the suggestion is only offered when there are none), and a
  // blank name is a 400 with the dialog still standing.
  const nameBox = page.getByLabel("Session name");
  await spotlight(page, nameBox);
  await nameBox.fill(SESSION_NAME);
  await spotlight(page, null);
  await beat(page, PACE.read);

  await act(page, page.getByRole("button", { name: "Start recording" }));

  const card = page.getByTestId(`session-${SESSION_KEY}`);
  await expect(card).toBeVisible({ timeout: 60_000 });
  await expect(card.getByText("Recording…")).toBeVisible({ timeout: 60_000 });
  // A real container comes up here — minutes, not seconds.
  await expect(card.locator(".xterm-screen").first()).toBeVisible({ timeout: SANDBOX_UP });

  // The Fence banner is the honest half of "nothing is denied while recording":
  // an open-egress session on a shared kernel is the widest window this product
  // ever opens, and the card says so. Leave it in frame; point at it if it is
  // there (a CC2/CC3 host renders no banner and the beat still plays).
  const cc1 = page.getByTestId("record-cc1-banner");
  if (await cc1.isVisible().catch(() => false)) await spotlight(page, cc1);
  await caption(page, "Nothing is denied while recording. Every host and command is written down.");
  await beat(page, PACE.read + 900);
  await spotlight(page, null);
});

// ---------------------------------------------------------------------------
// B3 — honest small work
// ---------------------------------------------------------------------------

test("B3 — honest small work", async () => {
  test.setTimeout(900_000);
  const page = stage();
  const card = page.getByTestId(`session-${SESSION_KEY}`);
  const screen = card.locator(".xterm-screen").first();

  await caption(page, "Real work: a git identity, then the two hosts this build actually needs.");
  await beat(page, PACE.read);
  await typeInTerminal(page, GIT_IDENTITY_CMD, card);
  await beat(page, PACE.read);

  await typeInTerminal(page, `curl -sSI https://${RECORDED_HOSTS[0]}`, card);
  await beat(page, PACE.read + 800);
  await caption(page, "The second host is the one a hand-written allowlist always forgets.");
  await beat(page, PACE.read);
  await typeInTerminal(page, `curl -sSI https://${RECORDED_HOSTS[1]}`, card);

  // BOTH hosts must actually answer. Counting status lines rather than matching
  // "200": what this beat has to prove is that the request reached the host and
  // came back through the proxy, and files.pythonhosted.org's bare root is
  // entitled to answer 403/404 — a curl that never resolved prints no status
  // line at all, records no egress, and B5's "Approve 2 observed hosts" then
  // never appears at all.
  await expect
    .poll(async () => ((await screen.innerText()).match(/HTTP\/[0-9.]+ \d{3}/g) ?? []).length, { timeout: 90_000 })
    .toBeGreaterThanOrEqual(2);
  await beat(page, PACE.read);

  await act(
    page,
    card.getByRole("button", { name: "Done recording" }),
    "Done recording. Wardyn captures on termination, from the audit trail, not the sandbox.",
  );

  // The capture is reconciled server-side after the run dies, and the page
  // polls for it — so this is the product, not pacing.
  await expect(card.getByText("Recorded", { exact: true })).toBeVisible({ timeout: CAPTURE_SETTLES });
  await expect(card.getByTestId("record-review")).toBeVisible({ timeout: CAPTURE_SETTLES });

  // AN EMPTY CAPTURE IS A FAILED TAKE, and it looks exactly like a good one
  // until someone reads the card: record-pane renders the reachability warning
  // where the review should be, every later beat degrades, and the narration
  // keeps claiming a policy was learned. Fail here instead.
  await expect(card.getByTestId("record-empty-capture")).toHaveCount(0);
  const newHosts = card.getByTestId("record-new-hosts");
  await expect(newHosts).toContainText(RECORDED_HOSTS[0]);
  await expect(newHosts).toContainText(RECORDED_HOSTS[1]);
  // EXACTLY TWO, asserted HERE rather than only at B5's "Approve 2 observed
  // hosts" button: this list IS that button's count (both read
  // egressPromotionDiff().approvable), and a third row means the workspace dir
  // was not marker-free. Failing on the list fails two beats and ~2 minutes of
  // shooting earlier, and names the extra host instead of a regex that missed.
  await expect(newHosts.locator("li")).toHaveCount(2);
  await beat(page, PACE.read + 900);
});

// ---------------------------------------------------------------------------
// B4 — evidence becomes policy
// ---------------------------------------------------------------------------

test("B4 — evidence becomes policy", async () => {
  test.setTimeout(600_000);
  const page = stage();
  const card = page.getByTestId(`session-${SESSION_KEY}`);

  await act(
    page,
    card.getByRole("button", { name: "Save session profile" }),
    "Save session profile. This is the synthesis, and every line is evidence.",
  );

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
  await beat(page, PACE.read + 600);

  // The synthesis is allow-only and it is exactly the two hosts that were
  // reached — the claim the whole video rests on.
  const proposed = sheet.getByText("Proposed allowed domains", { exact: true }).locator("xpath=..");
  await expect(proposed).toContainText(RECORDED_HOSTS[0]);
  await expect(proposed).toContainText(RECORDED_HOSTS[1]);

  const observations = sheet.locator('[aria-label="Observations"]');
  await spotlight(page, observations);
  await caption(page, "Egress domains: what it connected to, and how often.");
  await beat(page, PACE.read + 900);

  // The kernel's half. If the sensor were absent this is the assertion that
  // fails — and it fails on the ONE observation the beat actually names.
  // (/\.gitconfig/ rather than the full path: git rewrites the file through
  // .gitconfig.lock, and either shape is the credential-shaped write the
  // narration is pointing at.)
  const gitconfig = observations.getByText(/\.gitconfig/).first();
  await spotlight(page, gitconfig);
  await caption(page, "Executed, and file writes: kernel ground truth, raised as warnings, never as policy.");
  await beat(page, PACE.read + 900);
  await spotlight(page, null);

  // "no grants" is a real, asserted fact, not a turn of phrase: the session
  // minted no credential, so the synthesized spec carries no eligible grant.
  const grants = sheet.getByText("Eligible grants", { exact: true }).locator("xpath=..");
  await spotlight(page, grants);
  await expect(grants).toContainText("none");
  await caption(page, "It minted no credentials, so the policy grants none. Zero is evidence too.");
  await beat(page, PACE.read + 900);
  await spotlight(page, null);

  // Both halves of the next line, before it is spoken: allow-all is off, and an
  // unseen host has to ask (Synthesize forces deny_with_review → "Ask").
  const allowedDomains = sheet.getByText("Allowed domains", { exact: true }).locator("xpath=..");
  await expect(allowedDomains).not.toContainText("Allow all");
  await expect(sheet.getByText("First-use approval", { exact: true }).locator("xpath=..")).toContainText("Ask");

  await act(
    page,
    // "Save as with a name…" — the ellipsis is U+2026, so match the prefix.
    sheet.getByRole("button", { name: /^Save as with a name/ }),
    "Save as policy. Allow-all is off; an unseen host has to ask.",
  );

  const saveDialog = page.getByRole("dialog").filter({ hasText: "Save as policy" });
  await expect(saveDialog).toBeVisible({ timeout: 30_000 });
  // NOT typed: policyNameFor(workspace, session) already derived it and the
  // field opens holding it. Asserting the pre-fill is the honest version of the
  // script's "→ record-demo-build-test".
  const nameBox = saveDialog.getByLabel("Policy name");
  await expect(nameBox).toHaveValue(POLICY_NAME);
  await spotlight(page, nameBox);
  await beat(page, PACE.read);
  await spotlight(page, null);
  await act(page, saveDialog.getByRole("button", { name: "Save policy" }));
  await expect(saveDialog).toBeHidden({ timeout: 30_000 });

  // The script ends this beat on Esc, from the days when the dialog-save path
  // left the drawer standing behind it still claiming "nothing is created until
  // you save it". profile-review.tsx's onSaved closes both now — so Esc stays
  // only as a fallback, and the assertion below is what actually holds.
  if (await sheetTitle.isVisible().catch(() => false)) await page.keyboard.press("Escape");
  await expect(sheetTitle).toBeHidden({ timeout: 15_000 });

  // "Save as policy" is a claim about a WRITE. Ask the control plane.
  const policies = await apiGet<{ items?: { name?: string }[] } | { name?: string }[]>("/api/v1/policies");
  const named = (Array.isArray(policies) ? policies : (policies.items ?? [])).some((p) => p?.name === POLICY_NAME);
  expect(named, `the policy "${POLICY_NAME}" is not on the control plane — the save failed on camera`).toBe(true);
});

// ---------------------------------------------------------------------------
// B5 — replay confined
// ---------------------------------------------------------------------------

test("B5 — replay confined", async () => {
  test.setTimeout(900_000);
  const page = stage();
  const card = page.getByTestId(`session-${SESSION_KEY}`);

  // EXACTLY TWO. The count is the take's own smoke alarm: a third approvable
  // host means the workspace directory was not marker-free (its ecosystem hosts
  // got folded in), and "exactly as wide as the work" stops being true three
  // beats before anyone notices.
  const approveObserved = card.getByRole("button", { name: /^Approve 2 observed hosts$/ });
  await expect(approveObserved).toBeVisible({ timeout: 30_000 });
  await act(page, approveObserved, "Approve the observed hosts, then replay the same session confined.");

  // Host names came from a session's observed traffic, so promotion routes
  // through the shared untrusted-content confirm.
  const confirm = page.getByRole("alertdialog");
  await expect(confirm).toBeVisible({ timeout: 30_000 });
  await expect(confirm).toContainText(RECORDED_HOSTS[0]);
  await expect(confirm).toContainText(RECORDED_HOSTS[1]);
  await beat(page, PACE.read);
  await act(page, confirm.getByRole("button", { name: "Approve hosts" }));
  // The receipt for the click, before the replay that depends on it: without a
  // promoted set, confinedEgressDomains() for a local_dir workspace is EMPTY
  // and both curls below would be held, not allowed.
  await expect(card.getByText("Promoted", { exact: true })).toBeVisible({ timeout: 60_000 });
  await beat(page, PACE.read);

  await act(page, card.getByRole("button", { name: "Replay confined" }));
  await expect(card.getByText("Replaying confined…")).toBeVisible({ timeout: 120_000 });
  const screen = card.locator(".xterm-screen").first();
  await expect(screen).toBeVisible({ timeout: SANDBOX_UP });
  // .xterm-screen renders when AttachTerminal MOUNTS, before the PTY websocket
  // is up — typing here eats the first characters and the shell reports
  // "command not found" on camera.
  await beat(page, PACE.read);

  await typeInTerminal(page, `curl -sSI https://${RECORDED_HOSTS[0]}`, card);
  await beat(page, PACE.read + 600);
  await typeInTerminal(page, `curl -sSI https://${RECORDED_HOSTS[1]}`, card);
  await expect
    .poll(async () => ((await screen.innerText()).match(/HTTP\/[0-9.]+ \d{3}/g) ?? []).length, { timeout: 90_000 })
    .toBeGreaterThanOrEqual(2);

  await caption(page, "Default-deny now. Same commands, same two hosts, and nothing to approve.");
  // "nothing to approve" is the whole point of the beat, so prove it: the idle
  // hint, not a pending row. A held host here would mean the promotion did not
  // land, and the line would be narrating over a queue.
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

  await caption(page, "Now reach for a host the recording never saw.");
  await beat(page, PACE.read);
  await typeInTerminal(page, `curl -sSI --max-time 60 https://${UNSEEN_HOST}`, card);

  // HELD, not refused: a confined replay runs wait_for_review, so the
  // connection parks at the proxy while the strip waits on a person. The header
  // is what proves it is a hold rather than a fast denial — the difference the
  // next line is entirely about.
  await expect(card.getByText("Sandbox is waiting — approve to let it through")).toBeVisible({
    timeout: APPROVAL_APPEARS,
  });

  // Decide it on camera and QUICKLY. The hold expires after 30s
  // (defaultHoldTimeout); decide() speaks one line and clicks inside ~6s, which
  // is the point — a demo that waits out a clock is a bad demo, and an expired
  // hold films as a timeout nobody decided.
  await decide(card, "Deny", "Held at the proxy, waiting on a person. Deny, refused.", UNSEEN_HOST);
  await beat(page, PACE.read);

  await act(page, card.getByRole("button", { name: "Done", exact: true }));

  const review = card.getByTestId("verify-session-review");
  await expect(review).toBeVisible({ timeout: CAPTURE_SETTLES });
  // The two halves the closing line claims. The allowed count is a floor, not
  // an equality: the control plane's own host is legitimately reachable from a
  // confined replay and is counted here (it is only excluded from what an
  // operator can APPROVE), so pinning it to 2 would fail on a truthful frame.
  await expect(review).toContainText(/[2-9]\d* hosts reached, all allowed/);
  const blocked = card.getByTestId("verify-session-blocked");
  await expect(blocked).toContainText(UNSEEN_HOST);
  await expect(blocked).toContainText("blocked");

  await spotlight(page, review);
  await caption(page, "The policy wrote itself, and it is exactly as wide as the work.");
  await beat(page, PACE.read + 1200);
  await spotlight(page, null);

  await caption(page, "Watch it once. Enforce it forever.");
  await beat(page, PACE.read + 600);
  await caption(page, "Next: model access — how a run uses a model without holding a key.");
  await beat(page, PACE.chapter);
  await caption(page, "");
});
