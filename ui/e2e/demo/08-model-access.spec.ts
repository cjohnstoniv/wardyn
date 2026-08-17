/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

/*
 * Video 08 — Model access.
 *
 * The one video that answers "where does the API key live?" by opening the box
 * and showing that it isn't in there. Six beats: the three lanes that can
 * credential a Claude run (Settings → Model provider), the sandbox's own
 * environment (a real base URL and a base64 blob, no key), the blob decoded into
 * the credentials file Claude Code reads at startup (an access token that names
 * itself an inert sentinel), a REAL model call from that same shell that
 * nonetheless works, the single audit event that records the injection, and the
 * closing claim the whole video exists to earn.
 *
 * This is NOT a test. It asserts only enough to keep itself honest and to know
 * when to advance; a failure here means the recording is wrong, not that the
 * product is broken. It runs against the REAL compose stack on :8080 with a real
 * sandbox and a real, quota-consuming model call.
 *
 * ---------------------------------------------------------------------------
 * STATE THIS VIDEO INHERITS — it launches nothing, and it will not stage
 * itself. Every one of these is asserted in beforeAll (or by the first beat
 * that depends on it) so a mis-staged take dies in seconds instead of filming
 * ninety of them:
 *
 *  1. THE MANAGED SUBSCRIPTION LANE, connected. `wardyn subscription connect
 *     --token-stdin` (record-demo.sh's Act 0 does exactly this) captures a
 *     Wardyn-managed token; that is the lane whose dispatch branch sets
 *     WARDYN_CLAUDE_MANAGED_B64. The OTHER subscription lane — a resident
 *     ~/.claude on the host, which deriveIntegrations reports as hostCli — takes
 *     the mount branch instead (runs_dispatch_llm.go), sets NO b64 blob, and
 *     Beat 2 loses its centrepiece. Compose mode is distroless and has no host
 *     ~/.claude to mount, so this is the normal shape; it is asserted anyway
 *     because "the wrong lane won" is invisible until Beat 2 films an empty
 *     grep.
 *
 *  2. ONE INTERACTIVE, RUNNING claude-code AGENT RUN — pass its id in
 *     WARDYN_DEMO_RUN_ID, or leave it unset and the newest matching run wins.
 *     Interactive is not a preference: an exec-mode run has no credential, no
 *     injection and no audit event at all, so beats 2 through 5 would all be
 *     filming an empty room. TerminalPane only attaches when
 *     `run.interactive && run.state === "RUNNING"`, so both are checked.
 *
 *     AND IT MUST START ON A SHELL. The New Run wizard's "Start with" default
 *     is the AGENT, not a terminal, and interactive_start=agent makes
 *     attach-bashrc.sh launch `claude` in the shared tmux session's first
 *     shell — beats 2-4 would then type into Claude's prompt box. Checked
 *     from run.create's audit data, which is the only place the field is kept.
 *
 *  3. THE RUN'S POLICY ALLOWS EGRESS AND CARRIES NO anthropic-api-key GRANT.
 *     Either one suppresses the managed fallback SILENTLY (runs_dispatch_llm.go:
 *     a sealed policy must not be widened by a fallback; an api-key injection
 *     means the operator chose api-key). Rather than re-deriving that rule
 *     client-side, beforeAll asserts the fact it produces: a
 *     `run.llm.subscription_inject` audit event exists for this run. That single
 *     check subsumes preconditions 1–3 — if it is there, the managed credential
 *     was injected proxy-side for this run, and Beat 5 has a row to film.
 *
 *  4. NOBODY ELSE HOLDS THE PTY. First attach wins the holder, and a spectator's
 *     keystrokes are dropped server-side — a read-only take would type three
 *     commands into a terminal that never runs them. Beat 2 asserts the
 *     terminal's own "you are driving" badge before typing a character. Per
 *     SV16, V03's off-camera holder attaches AFTER this video, never before.
 *
 *  5. QUOTA. Beat 4 makes a real Anthropic call. There is no fallback lane and
 *     no shell variant: without a completed model call the video reads as a
 *     broken install. Shoot this first in the quota window (series order
 *     V01 → V08 → V02 → V03), and inside that window every take runs
 *     `record-demo.sh --no-reset` (SV20) — a reset would destroy the very run
 *     this video and V03 share.
 *
 * SV16 also makes this spec responsible for two things V03 depends on: it types
 * `clear` before it leaves the terminal (so V03's "same scrollback" beat does
 * not spotlight this video's sentinel commands), and closing the context at the
 * end closes every cockpit tab, which releases the holder for V03's off-camera
 * attach. Do not remove either.
 *
 * OFF-CAMERA, OPERATOR-ONLY: nothing here can create the run, connect the
 * subscription, or refill a quota. Those are shoot-day setup, and if any of them
 * is missing this file says so by name and stops.
 * ---------------------------------------------------------------------------
 *
 * Selectors are getByRole + accessible names, matching the rest of the suite:
 * every literal below exists in ui/src today.
 */

import { test, expect, type Page } from "@playwright/test";
import { act, beat, caption, PACE, spotlight, typeInTerminal } from "./overlay";
// The browser, the recorded context and the shared page live in stage.ts:
// importing it is what registers this file's beforeAll/afterAll, and each beat
// reads the page out of stage() rather than closing over a module-level `let`.
import { stage } from "./stage";

test.skip(!process.env.WARDYN_DEMO, "demo recording — run via `make record-demo` (exports WARDYN_DEMO=1)");

test.describe.configure({ mode: "serial" });

// Waiting on the PRODUCT, not on the viewer. A sandbox is already up by the time
// this video rolls, but the attach socket, a cold `claude` boot and a real model
// round-trip are all seconds-to-minutes. Pacing the viewer sees comes from
// overlay.ts and nothing here.
const TERMINAL_READY = 120_000;
const COMMAND_ECHOES = 60_000;
const MODEL_ANSWERS = 180_000;

/**
 * The load-bearing string of the whole video.
 *
 * internal/api/harnesscred.go's managedSentinelAccessToken, verbatim. It is the
 * ONLY thing inside the sandbox that labels the credential blob as inert —
 * nothing else in the environment says so — which makes its exact spelling a
 * contract, not a detail. Beat 3 both narrates it and asserts it; if the Go
 * constant ever changes, this take fails rather than narrating "inert sentinel"
 * over whatever replaced it.
 */
const SENTINEL = "sk-ant-oat01-wardyn-inert-sentinel-proxy-injects-the-live-token";

/** The audit action the managed injection writes (runs_dispatch_llm.go). */
const INJECT_ACTION = "run.llm.subscription_inject";

/** What Beat 5 types into the audit search box — a fragment of the action. */
const INJECT_SEARCH = "subscription_inject";

/**
 * The audit screen's own prose for that action (audit.tsx's ACTION_VERB,
 * capitalized by describeEvent). Named here because Beat 5 asserts on it: the
 * row is the video's receipt, and matching the raw dotted string instead would
 * pass on a screen showing the fallback rendering.
 */
const INJECT_ROW_TEXT = "Injected the subscription credential at the proxy";

/** Optional pin for the run this video films. Unset = newest match wins. */
const RUN_ID_ENV = "WARDYN_DEMO_RUN_ID";

// Resolved in beforeAll, read by every beat from Beat 2 on.
let runId = "";

/** Same bearer shape funnel.ts's clearWorkspace uses — inert in local mode. */
function apiHeaders(): Record<string, string> | undefined {
  return process.env.WARDYN_DEMO_TOKEN
    ? { Authorization: `Bearer ${process.env.WARDYN_DEMO_TOKEN}` }
    : undefined;
}

/** GET an API path as JSON, or null. Setup only — never choreography. */
async function apiGet<T>(page: Page, path: string): Promise<T | null> {
  const res = await page.request.get(path, { headers: apiHeaders() }).catch(() => null);
  if (!res?.ok()) return null;
  return (await res.json().catch(() => null)) as T | null;
}

/** The list endpoints answer as a bare array or as {items:[…]}; accept both. */
function asList<T>(body: unknown): T[] {
  if (Array.isArray(body)) return body as T[];
  const o = (body ?? {}) as Record<string, unknown>;
  for (const key of ["items", "runs", "events"]) {
    if (Array.isArray(o[key])) return o[key] as T[];
  }
  return [];
}

/**
 * Ring the terminal ROW carrying `needle`, or the whole screen if it straddles
 * two of them.
 *
 * xterm's DOM renderer paints one div per VISUAL row, so a row locator gives a
 * tight ring around the exact line the narration is talking about — but a long
 * line wraps, and where it wraps depends on the pane's column count at 1080p.
 * Falling back to the screen keeps the beat honest either way: a ring around
 * the terminal is a weaker frame than a ring around the token, never a wrong one.
 */
async function spotlightTerminalRow(page: Page, needle: string): Promise<void> {
  const screen = page.locator(".xterm-screen").first();
  const row = screen.locator(".xterm-rows > div").filter({ hasText: needle }).first();
  await spotlight(page, (await row.count().catch(() => 0)) ? row : screen);
}

// ---------------------------------------------------------------------------
// Preconditions. Everything this video inherits, checked before a frame of it
// is worth filming — see the file header for why each one matters.
// ---------------------------------------------------------------------------

test.beforeAll(async () => {
  // stage.ts's beforeAll registered first, so the page exists by now. Belt and
  // braces against a runner that decides to execute hooks under a file-level
  // skip: this hook talks to a live stack, and nothing outside a real take
  // should.
  if (!process.env.WARDYN_DEMO) return;
  const page = stage();

  // (1) The MANAGED lane, not the resident-mount one. deriveIntegrations calls
  // a captured anthropic harness credential the managed lane, and a host CLI
  // login with auth_mode "subscription" the resident one — and when BOTH exist
  // the resident row is the one the card and the dispatcher pick, which is the
  // failure this checks for.
  const status = await apiGet<{
    harness?: { provider: string; captured: boolean }[];
    providers?: { tool: string; logged_in: boolean; auth_mode?: string }[];
  }>(page, "/api/v1/setup/status");
  const managed = (status?.harness ?? []).some((h) => h.provider === "anthropic" && h.captured);
  expect(
    managed,
    "V08 needs the MANAGED subscription lane connected (`wardyn subscription connect --token-stdin`). " +
      "Without it there is no WARDYN_CLAUDE_MANAGED_B64 in the sandbox and Beat 2 has nothing to show.",
  ).toBe(true);
  const resident = (status?.providers ?? []).some(
    (p) => p.tool === "claude" && p.logged_in && p.auth_mode === "subscription",
  );
  expect(
    resident,
    "wardynd sees a RESIDENT Claude login on this host, so dispatch takes the mount branch and sets no " +
      "base64 blob. Record this video from the containerized (compose) stack, where there is no ~/.claude to mount.",
  ).toBe(false);

  // (2) One interactive, RUNNING claude-code run — pinned, or the newest match.
  const runs = asList<{
    id: string;
    agent: string;
    state: string;
    interactive?: boolean;
    created_at: string;
  }>(await apiGet(page, "/api/v1/runs?limit=1000"));
  const pinned = process.env[RUN_ID_ENV];
  const candidates = runs
    .filter((r) => r.interactive && r.state === "RUNNING" && r.agent === "claude-code")
    .sort((a, b) => (a.created_at < b.created_at ? 1 : -1));
  // Lower-cased: the id is compared against audit rows' run_id (Go emits a
  // lower-case uuid) and rendered into Beat 5's drill-in chip, so an
  // upper-cased WARDYN_DEMO_RUN_ID would pass /runs/{id} (uuid.Parse is
  // case-insensitive) and then silently miss both.
  runId = (pinned || candidates[0]?.id || "").replace(/^run_/, "").toLowerCase();
  expect(
    runId,
    `V08 films a live INTERACTIVE claude-code run and never launches one. Start it before the take — ` +
      `New run → Agent task → Interactive → Start with: "Terminal — a shell in the workspace dir" → ` +
      `a workspace, egress allowed, no anthropic-api-key grant — ` +
      `then either leave it as the newest RUNNING interactive run or pin it with ${RUN_ID_ENV}. ` +
      `(An exec-mode run is disqualified: no credential, no injection, no audit event at all.)`,
  ).not.toBe("");

  if (pinned) {
    const run = await apiGet<{ state?: string; interactive?: boolean }>(
      page,
      `/api/v1/runs/${encodeURIComponent(runId)}`,
    );
    expect(run?.interactive === true && run?.state === "RUNNING", `${RUN_ID_ENV}=${runId} is not an interactive RUNNING run`).toBe(true);
  }

  // (3) THE ATTACH SHELL MUST BE A SHELL. The New Run wizard DEFAULTS
  // interactive_start to "agent" (new-run/wizard-types.ts), and with that set
  // attach-bashrc.sh runs `claude` on the FIRST shell of the sandbox's shared
  // tmux session — so beats 2-4 would type `env | grep …`, `cat …` and
  // `claude -p …` into Claude Code's own prompt box instead of a shell. The run
  // ROW cannot answer this (interactive_start is request-scoped and never
  // stored); run.create's audit data is the only record of it.
  const created = asList<{ data?: { interactive_start?: string } }>(
    await apiGet(page, `/api/v1/audit?run_id=${encodeURIComponent(runId)}&action=run.create`),
  );
  expect(
    created.some((e) => e.data?.interactive_start === "agent"),
    `run ${runId} was created with interactive_start="agent", so its attach session opens INSIDE the agent CLI ` +
      `and beats 2-4 would type shell commands into Claude's prompt. Relaunch it with ` +
      `New run → Start with → "Terminal — a shell in the workspace dir" (the wizard defaults to the agent).`,
  ).toBe(false);

  // (4) The one fact that proves the whole credential path fired for THIS run:
  // policy allowed egress, no api-key grant pre-empted the fallback, and the
  // managed token was injected proxy-side. Beat 5 films this event; if it does
  // not exist, beats 2-5 are all narrating a lane that never ran.
  //
  // Queried GLOBALLY, not per-run, because Beat 5's search box is global too:
  // its toHaveCount(1) is the narration's own word "one", and a SECOND
  // injection row (a rehearsal run, or this run relaunched after an auto-stop)
  // makes that line false on camera. Catching it here costs seconds; catching
  // it at Beat 5 costs the quota turn Beat 4 already spent, which cannot be
  // re-shot.
  const injections = asList<{ action: string; run_id?: string }>(
    await apiGet(page, `/api/v1/audit?action=${INJECT_ACTION}`),
  );
  expect(
    injections.some((e) => (e.run_id ?? "").toLowerCase() === runId),
    `run ${runId} has no ${INJECT_ACTION} audit event, so nothing injected a managed credential for it. ` +
      `Most likely its policy allows no egress at all, or it carries an anthropic-api-key grant — either one ` +
      `suppresses the managed fallback silently. Relaunch the run with egress allowed and no api-key grant.`,
  ).toBe(true);
  expect(
    injections.length,
    `${injections.length} ${INJECT_ACTION} events exist on this stack, and Beat 5 says "one event" out loud ` +
      `(its toHaveCount(1) would fail AFTER Beat 4 burned the quota turn). Every earlier interactive run left ` +
      `one of these behind — shoot V08 first in the window, against a stack whose only such run is this one, ` +
      `and pin it with ${RUN_ID_ENV}.`,
  ).toBe(1);
});

// ---------------------------------------------------------------------------
// Cold open
// ---------------------------------------------------------------------------

test("cold open — where does the key live", async () => {
  test.setTimeout(180_000);
  const page = stage();
  await page.goto("/settings");
  await page.bringToFront();

  // Fail here rather than deep into a silent, caption-less take: every narration
  // call degrades to a no-op by design, so nothing downstream would ever
  // complain about an overlay that failed to install.
  await expect
    .poll(() => page.evaluate(() => typeof (window as unknown as Record<string, unknown>).__demo), { timeout: 15_000 })
    .toBe("object");

  await expect(page.getByRole("heading", { name: "Settings", level: 1 })).toBeVisible({ timeout: 60_000 });

  await caption(page, "Every agent sandbox faces one question: where does the API key live?");
  await beat(page, PACE.read + 600);
  await caption(page, "Wardyn's answer: not in the sandbox. The box will prove it.");
  await beat(page, PACE.read + 600);
});

// ---------------------------------------------------------------------------
// Beat 1 — three lanes
// ---------------------------------------------------------------------------

test("beat 1 — three lanes credential a run", async () => {
  test.setTimeout(180_000);
  const page = stage();

  // The card is a real radiogroup, aria-labelled with its own title
  // (connection-cards.tsx's S.MODEL_TITLE) — so the three lanes are radios, and
  // the group is the thing to ring while the line about them is spoken.
  const card = page.getByRole("radiogroup", { name: "Model provider" });
  await expect(card).toBeVisible({ timeout: 30_000 });
  await spotlight(page, card);
  await caption(page, "Three lanes credential a Claude run: managed subscription, API key, or AWS Bedrock.");

  // Assert the claim the line just made. Each lane's accessible name carries its
  // hint copy too, so match on the title prefix.
  const subscription = card.getByRole("radio", { name: /Claude subscription/ });
  await expect(subscription).toBeVisible();
  await expect(card.getByRole("radio", { name: /^API key/ })).toBeVisible();
  await expect(card.getByRole("radio", { name: /AWS Bedrock/ })).toBeVisible();
  await beat(page, PACE.read + 800);

  await spotlight(page, subscription);
  await caption(page, "This one is connected: the operator captured it once, in a login sandbox.");
  // Two assertions, because the second is the one the rest of the video rests
  // on. "Connected" alone is true of the resident host-CLI lane as well; this
  // sentence — connection-cards.tsx's managedSub branch — renders ONLY for the
  // Wardyn-captured token, which is the lane that sets the base64 blob Beat 2
  // opens on.
  await expect(subscription).toContainText("Connected");
  await expect(page.getByText("Captured through a login sandbox and stored by Wardyn.")).toBeVisible();
  await beat(page, PACE.read + 900);

  // The card's footer, held on screen under the line about it. B-DEPENDENT, and
  // it MOVED: the script quotes "Keys never enter the sandbox — the egress proxy
  // injects them on the wire.", which no longer exists. connection-cards.tsx's
  // S.MODEL_FOOTER now leads with the mechanism and admits the one lane the old
  // sentence overclaimed (Bedrock's SSO path signs inside the sandbox). Nothing
  // is spoken over it — the narration above already carries the beat — so the
  // change costs the script nothing but this comment.
  const footer = page.getByText("The egress proxy injects these on the wire, so keys never enter the sandbox");
  await spotlight(page, footer);
  await beat(page, PACE.read + 900);
  await spotlight(page, null);
});

// ---------------------------------------------------------------------------
// Beat 2 — inside the box
// ---------------------------------------------------------------------------

test("beat 2 — inside the box", async () => {
  test.setTimeout(300_000);
  const page = stage();

  await page.goto(`/runs/${runId}`);
  await expect(page.getByRole("tab", { name: /Overview/ })).toBeVisible({ timeout: 60_000 });

  await caption(page, "Now inside a live run — the agent's own shell, its own environment.");

  // THE HOLDER CHECK, and it is not cosmetic. Attach is a shared tmux session:
  // arriving second admits us read-only and the server DROPS our keystrokes
  // (attach-terminal.tsx / attachPump), so all three commands below would type
  // into a terminal that never runs them while the narration described their
  // output. "you are driving" (copy.ts's RUN_COCKPIT.driving) renders only when
  // this client holds the PTY.
  const screen = page.locator(".xterm-screen").first();
  await expect(screen).toBeVisible({ timeout: TERMINAL_READY });
  await expect(
    page.getByText("you are driving"),
    "another client holds this run's terminal — close it (or take over) before rolling; a read-only take types into nothing",
  ).toBeVisible({ timeout: TERMINAL_READY });
  // .xterm-screen renders when the terminal MOUNTS, before the PTY websocket is
  // up; typing in that gap eats the first characters and the shell reports
  // "command not found" on camera.
  await beat(page, PACE.read);

  await typeInTerminal(page, "env | grep -i -E 'anthropic|claude'");
  await caption(page, "No API key here. A real base URL, one base sixty-four blob.");

  // The payoff, in the order the line claims it. The base URL is the REAL
  // Anthropic endpoint (the proxy is reached via HTTPS_PROXY and swaps the
  // credential by TLS-MITM), the config dir is where Beat 3 goes looking, and
  // the blob is the managed lane's fingerprint.
  await expect(screen).toContainText("ANTHROPIC_BASE_URL=https://api.anthropic.com", { timeout: COMMAND_ECHOES });
  await expect(screen).toContainText("CLAUDE_CONFIG_DIR=");
  await expect(screen).toContainText("WARDYN_CLAUDE_MANAGED_B64=");
  // "No API key here" is a claim about an ABSENCE, so it gets its own assertion.
  // Checked last, after the grep's output has demonstrably landed — an empty
  // screen would satisfy it trivially.
  await expect(screen).not.toContainText("ANTHROPIC_API_KEY");
  await beat(page, PACE.read + 1200);
});

// ---------------------------------------------------------------------------
// Beat 3 — the decoy
// ---------------------------------------------------------------------------

test("beat 3 — the decoy", async () => {
  test.setTimeout(300_000);
  const page = stage();
  const screen = page.locator(".xterm-screen").first();

  await caption(page, "That blob decodes into the credentials file Claude Code reads at startup.");
  await beat(page, PACE.read);

  // agent-run-lib.sh's materialize_managed_claude_config wrote this file from
  // WARDYN_CLAUDE_MANAGED_B64 at sandbox start — the same JSON, one base64
  // decode away, which is why the beat can `cat` it instead of piping.
  await typeInTerminal(page, "cat $CLAUDE_CONFIG_DIR/.credentials.json");

  // The full token, not a prefix. It is a Go constant (harnesscred.go) and this
  // is the frame the narration points at; a partial match would let a changed
  // spelling through while the line says "names itself an inert sentinel".
  // json.Marshal emits accessToken first (map keys sort), so at any sane column
  // count it lands whole on the first wrapped row.
  await expect(screen).toContainText(SENTINEL, { timeout: COMMAND_ECHOES });
  await spotlightTerminalRow(page, "sk-ant-oat01-wardyn-inert-sentinel");
  await caption(page, "The token names itself an inert sentinel: the shape a harness demands, carrying nothing.");
  await beat(page, PACE.read + 1400);
  await spotlight(page, null);
});

// ---------------------------------------------------------------------------
// Beat 4 — the boundary
// ---------------------------------------------------------------------------

test("beat 4 — the boundary", async () => {
  test.setTimeout(600_000);
  const page = stage();
  const screen = page.locator(".xterm-screen").first();

  await caption(page, "Same shell, real model call. The proxy strips what the sandbox sent.");
  await beat(page, PACE.read);

  // A REAL, quota-consuming Anthropic call, made by a shell holding nothing but
  // the sentinel above. There is deliberately no fallback: a shell-only variant
  // of this beat would film the sandbox failing to reach the model, which is the
  // opposite of the video's claim.
  await typeInTerminal(page, 'claude -p "name three primary colors"');
  await caption(page, "It attaches the live token at the boundary — outside the box.");

  // THE PAYOFF. \b matters: "credentials" from Beat 3 is still in the scrollback
  // and contains "red". A word-bounded colour proves an actual answer arrived.
  await expect(screen).toContainText(/\b(red|yellow|blue)\b/i, { timeout: MODEL_ANSWERS });
  // And the failure this most plausibly degrades into — a rejected credential,
  // an exhausted quota — must not be on screen while the narration says the call
  // went through. This is the assertion that stops a green take from narrating
  // success over a 401.
  await expect(screen).not.toContainText(/invalid api key|authentication_error|API Error|credit balance|\/login/i);
  await beat(page, PACE.read + 1600);

  // SV16: this run is handed to V03 next, and its "same scrollback" beat must
  // not open on this video's sentinel commands. Wipe the screen before leaving
  // the terminal — the holder itself is released when the context closes.
  await typeInTerminal(page, "clear");
  await beat(page, PACE.read);
});

// ---------------------------------------------------------------------------
// Beats 5 and 6 — on the record, and the close
// ---------------------------------------------------------------------------

test("beat 5 — on the record, and the close", async () => {
  test.setTimeout(300_000);
  const page = stage();

  await act(page, page.getByRole("link", { name: /^Audit/ }));
  await expect(page.getByRole("heading", { name: "Audit", level: 1 })).toBeVisible({ timeout: 30_000 });

  // Typed rather than filled: the search IS the beat, and the audit list filters
  // as it goes.
  const search = page.getByPlaceholder(/Search events/);
  await spotlight(page, search);
  await search.click();
  await search.pressSequentially(INJECT_SEARCH, { delay: 60 });
  await spotlight(page, null);

  await caption(page, "The injection lands in the audit trail: one event, this run.");

  // audit.tsx renders the action through ACTION_VERB; matching that prose (not
  // the raw dotted string) is what proves the row rendered as a sentence rather
  // than falling back. toHaveCount(1) is the narration's own word "one" — if a
  // previous take's run left a second injection event in the window, the line is
  // false and this take should die rather than say it. (Re-taking V08 against
  // the SAME run, which SV16 requires anyway, keeps this at one.)
  const row = page.getByText(INJECT_ROW_TEXT);
  await expect(row).toHaveCount(1, { timeout: 30_000 });
  await spotlight(page, row.first());
  // ...and it is THIS run's event, not some other run's — the row carries its
  // run id as a drill-in chip (EventRow), which is the whole "this run" half of
  // the claim.
  await expect(page.getByRole("button", { name: runId })).toBeVisible();
  await beat(page, PACE.read + 1400);
  await spotlight(page, null);

  await caption(page, "A compromised agent cannot exfiltrate a credential it never held.");
  await beat(page, PACE.read + 1200);

  await caption(page, "Next: the same guarantees, unattended — Wardyn inside a CI pipeline.");
  await beat(page, PACE.chapter);
  await caption(page, "");
});
