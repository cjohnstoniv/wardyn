/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

/*
 * Episode 03a of the series — "What it stops: the demos". THE CORE EPISODE.
 *
 * WHERE IT SITS. Third in the series — 01 problem/solution, 02 make setup +
 * Essentials, 03a (this) the boundary proved on camera, 04 add a workspace, 05
 * your first policy. Driven by `scripts/record-demo.sh --video 03a`, which globs
 * this exact filename. Three OPTIONAL detours hang off it — 03b (the network,
 * three more ways), 03c (authorized, then issued), 03d (the kinds that can't use
 * a header) — and all three hand back to 04, so this episode STANDS ALONE: no
 * caption here may depend on one of them, and the conclusion only points at them.
 *
 * WHAT THIS FILMS — the boundary on both axes: six demo cards and one masked
 * write, in five acts plus a conclusion.
 *   ACT 1-2, the egress quartet, deep-driven in one continuous walk:
 *     sealed-box (always deny), fail-then-approve (deny with review),
 *     held-at-the-door (wait for review) and lines-that-can't-be-crossed
 *     (allow-all egress + the 169.254.169.254 metadata probe that is refused
 *     anyway). Act 2 has no entry point of its own: it advance()s from where
 *     act 1 landed, which is why the two never split.
 *   ACT 3-5, the two secrets basics a first-time viewer needs. One: YOU CANNOT
 *     READ IT BACK — a masked write on camera (act 3, the store's write-only
 *     door) proved from inside a sandbox by write-only-by-design's 404 (act 4).
 *     Two: A RUN USES IT WITHOUT HOLDING IT — key-never-in-the-box (act 5).
 *     The third mechanism demo (authorized-not-issued) is a refinement, and
 *     opens the 03c detour instead.
 *
 * PROVENANCE. Not a rewrite from scratch — a MERGE of proven choreography,
 * split out of 03-what-it-stops.spec.ts on 2026-08-24 with every line moved
 * verbatim:
 *   - The four-test egress quartet is the owner-ratified cut carried verbatim
 *     from the retired 01-getting-started.spec.ts act 3 (via walkthrough.spec
 *     act 3 before that; both in git history). Its xterm assertions are moved
 *     to expect.poll(innerText) per the series LAW (toContainText starves in a
 *     take while the same text is on screen — episodes 09/10 both burned
 *     rehearsals on it); the dialogue is untouched.
 *   - The secrets acts LIFT the write-only masked write (old-07 beat 6), the
 *     write-only-by-design drive (the retiring 04's beats 4-5), and the
 *     key-never drive (old-07 beat 7) (the plan retires those beats there:
 *     new-04 = old-04 MINUS its demo beat, new-07 = old-06 MINUS beats 6-8).
 *     Those lines are the owner's, already ratified in
 *     local/secrets-episodes-dialog-proposals.md — carried verbatim.
 *   - The act-2 opener and the conclusion's detour pointer are
 *     [OWNER SLOT — drafted] in local/episode-03-mega-proposal.md (in take
 *     order). local/episode-03-stanza-check.py fails if a spec string and a
 *     proposal stanza ever drift.
 *
 * STAGING THIS FILE OWNS (off camera):
 *   1. NO RESET. `record-demo.sh --no-reset` against the same long-lived stack
 *      the earlier videos left behind (barrier + Secrets store carry over).
 *      beforeAll sweeps stale approvals/runs (S6) and DELETES wardyn-demo-key,
 *      so act 3's masked write always films a genuine creation. It stages
 *      nothing else: this episode's only secret is the one it writes on camera.
 *   2. A FRESH BROWSER CONTEXT. stage.ts registers its own beforeAll/afterAll
 *      per spec FILE; act 1 reopens /setup and seeds wardyn-onboarding-seen the
 *      way this host's operator did when they walked the welcome in episode 02.
 *
 * This is NOT a test. It asserts only enough to stay honest and to know when to
 * advance; a failure here means the recording is wrong, not the product. It
 * runs against the REAL compose stack on :8080 with real sandboxes — the
 * hermetic `-runner none` backend cannot start one at all.
 *
 * Selectors are getByRole + accessible names + the `demo-*` testids, matching
 * the rest of the suite: a copy change breaks this loudly and in one place.
 */

import { test, expect } from "@playwright/test";
import { FUNNEL_DEMOS } from "./task";
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
// The browser, the recorded context and the shared page live in stage.ts:
// importing it is what registers this file's beforeAll/afterAll, and each act
// reads the page out of stage() rather than closing over a module-level `let`.
import { stage } from "./stage";
import { advance, decide } from "./funnel";
import { sweepStaleState } from "./sweep";
// Every demo-card primitive is shared with the 03b/03c/03d detours.
import {
  BEAT_SHORT,
  DEMO_KEY,
  DEMO_KEY_VALUE,
  deleteSecret,
  frameRun,
  noteDemoRun,
  openDemo,
  openEpisode,
  pollScreen,
  SANDBOX_UP,
  silentCard,
  startAndBoot,
  walkPolicyKey,
} from "./demos";

test.skip(!process.env.WARDYN_DEMO, "demo recording — run via `make record-demo` (exports WARDYN_DEMO=1)");

test.describe.configure({ mode: "serial" });


test.beforeAll(async () => {
  if (!process.env.WARDYN_DEMO) return;
  const page = stage();
  // S6: deny stale pending approvals / kill stale runs first — otherwise the
  // Approvals badge carries a prior take's number through the whole video.
  await sweepStaleState();
  // wardyn-demo-key is written on camera (act 3) — clear a prior take's copy so
  // that write always films a real creation. Nothing else is staged: the three
  // per-kind secrets belong to the 03c/03d detours, which stage their own.
  await deleteSecret(page, DEMO_KEY);
});

// ===========================================================================
// ACTS 1-2 — THE EGRESS QUARTET
// ===========================================================================

// ---------------------------------------------------------------------------
// The four guardrail demos, exactly as the quartet cut films them.
// FUNNEL_DEMOS (task.ts) is the shared definition; once-or-for-good is filtered
// out — it headlines the optional 03b detour, and its caret-scope beat belongs
// to episode 10. `lines-that-cant-be-crossed` keeps the local trim: the
// example.com contrast + the metadata probe, minus the LAN probe.
// ---------------------------------------------------------------------------
type StopDemo = {
  id: string;
  label: string;
  cmds: readonly string[];
  caption: string;
  approve: boolean;
  scope: "run" | "once";
};

const LINES_CMDS = [
  "curl -sSI https://example.com",
  "curl -sSI --max-time 2 http://169.254.169.254/latest/meta-data/",
] as const;

const STOP_DEMOS: StopDemo[] = FUNNEL_DEMOS.filter((d) => d.id !== "once-or-for-good").map((d) => ({
  ...d,
  cmds: d.id === "lines-that-cant-be-crossed" ? LINES_CMDS : d.cmds,
}));

test("V03a act 1 — open on the demos", async () => {
  test.setTimeout(120_000);
  const page = stage();
  // A fresh take context has never seen the welcome hero — seed the flag the
  // way this host's operator did in episode 02, then open the first demo.
  await openEpisode(page, "sealed-box", "The sealed box");
});

// ---------------------------------------------------------------------------
// Act 2 — the four demos. Every wait in this loop is load-bearing; each is
// moved verbatim from the retired quartet cut. Two things are NOT verbatim and
// neither is a word: the xterm assertions are expect.poll(innerText) per the
// series LAW, and each boot wait is ffwd-wrapped the way startAndBoot wraps
// every other act's. The dialogue is untouched.
// ---------------------------------------------------------------------------
test("V03a act 2 — four ways the boundary holds", async () => {
  test.setTimeout(1_200_000);
  const page = stage();

  await chapter(page, "What it stops", "Every guardrail, proved on camera");
  await caption(page, "Setup is one thing.");
  await beat(page, BEAT_SHORT);
  await caption(page, "Now let's see the boundary actually work.");
  await beat(page, PACE.read);
  // [OWNER SLOT — drafted] Re-framed for the restructure: this episode films
  // EVERY guardrail across both groups (egress + secrets), not just the four-
  // test quartet — the old "Four small tests / Four real sandboxes" opener
  // undercounted it. The network's other three demos (agent-in-the-box,
  // record-a-policy, once-or-for-good) now run in full in 03b, an OPTIONAL
  // detour, so this opener counts only the four this episode films and the
  // conclusion points at the detours. A core caption may never depend on one.
  await caption(page, "We'll walk every guardrail Wardyn puts around a run — where it can reach, and what it can hold.");
  await beat(page, PACE.read);
  // [OWNER SLOT — drafted]
  await caption(page, "Start with the network. Four demos, each in its own sandbox — one for each thing that can happen to a host.");
  await beat(page, BEAT_SHORT);
  await caption(page, "And every decision is visible on screen.");
  await beat(page, PACE.read);

  for (const demo of STOP_DEMOS) {
    await expect(page.getByRole("heading", { name: demo.label, level: 2 })).toBeVisible({ timeout: 60_000 });

    // Ring the demo's own card as its intro speaks — the owner's plain-language
    // lines below already say what changed. fail-then-approve rings its steps
    // block (the approvable arc lives there); the rest ring the card.
    const card = page.getByTestId(`demo-card-${demo.id}`);
    await card.scrollIntoViewIfNeeded().catch(() => {});
    await spotlight(page, demo.id === "fail-then-approve" ? card.getByTestId("demo-steps") : card);

    if (demo.id === "sealed-box") {
      await caption(page, "Test one: denied.");
      await beat(page, BEAT_SHORT);
      await caption(page, "First, something the policy simply doesn't allow.");
      await beat(page, PACE.read);
    } else if (demo.id === "fail-then-approve") {
      await caption(page, "Test two: denied, but it can ask.");
      await beat(page, BEAT_SHORT);
      await caption(page, "Now we'll give the policy a different instruction.");
      await beat(page, PACE.read);
      await caption(page, "Don't silently refuse.");
      await beat(page, BEAT_SHORT);
      await caption(page, "Ask me.");
      await beat(page, BEAT_SHORT);
    } else if (demo.id === "held-at-the-door") {
      await caption(page, "Test three: held for a decision.");
      await beat(page, BEAT_SHORT);
      await caption(page, "This time, the request is already in progress when the policy stops it.");
      await beat(page, PACE.read);
    }
    // lines-that-cant-be-crossed: no intro of its own — the owner's script
    // picks this demo back up mid-scene (the held-at-the-door tail below).
    await spotlight(page, null);

    // THE POLICY, LINE BY LINE. The first demo walks every key/value; each later
    // demo highlights ONLY the line that changed and why. sealed-box is the
    // baseline the whole episode diffs against.
    const policyCard = page.getByTestId(`demo-card-${demo.id}`);
    await policyCard.getByTestId(`demo-policy-${demo.id}`).scrollIntoViewIfNeeded().catch(() => {});
    if (demo.id === "sealed-box") {
      await caption(page, "Before it runs, look at the policy — the whole contract, four lines.");
      await beat(page, PACE.read);
      await walkPolicyKey(page, policyCard, demo.id, "min_confinement_class",
        "The barrier: CC1, Fence — the lightest sandbox tier, the wall between this run and your host.");
      await walkPolicyKey(page, policyCard, demo.id, "auto_stop_after_sec",
        "A dead-man's switch: if nothing else stops it, the run halts itself after fifteen minutes.");
      await walkPolicyKey(page, policyCard, demo.id, "allowed_domains",
        "The allowlist — empty. Not one destination is permitted.");
      await walkPolicyKey(page, policyCard, demo.id, "first_use_approval",
        "And for anything not listed: always deny. Refused the instant it's dialed — no prompt, no wait.");
    } else if (demo.id === "fail-then-approve") {
      await walkPolicyKey(page, policyCard, demo.id, "first_use_approval",
        "One line changed. Always-deny became deny-with-review.",
        "Now an unlisted host isn't silently refused — it's refused, but you're asked, and you can let it through.");
    } else if (demo.id === "held-at-the-door") {
      await walkPolicyKey(page, policyCard, demo.id, "first_use_approval",
        "Same line again — now wait-for-review.",
        "The request is held in flight while it waits for you. The command doesn't fail; it pauses.");
    } else if (demo.id === "lines-that-cant-be-crossed") {
      await walkPolicyKey(page, policyCard, demo.id, "allow_all_egress",
        "This one throws the door open — allow-all-egress, true. Every public host is permitted.",
        "And yet some destinations are still refused — link-local and private addresses are denied beneath the policy, whatever it says.",
        "Those are the lines that can't be crossed.");
    }

    // No caption on the click — "starting the sandbox" narrates itself.
    await act(page, page.getByTestId(`demo-start-${demo.id}`));
    // The same ffwd discipline startAndBoot gives every other act. Four real
    // container boots in one test is ~4 minutes of dead air otherwise — the
    // "carried verbatim" law is about the words, not the boot wait.
    await beat(page, 200);
    await ffwdStart(page);
    try {
      await expect(page.locator(".xterm-screen").first()).toBeVisible({ timeout: SANDBOX_UP });
    } finally {
      await ffwdEnd(page);
    }
    await noteDemoRun(page, demo.id);
    await beat(page, PACE.read);
    // Cockpit framing: policy at the top, terminal + audit below, one frame.
    await frameRun(page, demo.id);

    for (const [i, cmd] of demo.cmds.entries()) {
      if (demo.approve && i === 1) {
        await decide(page, "Approve", "Approve it.", "example.com", demo.scope);
        if (demo.id === "fail-then-approve") {
          await caption(page, "Now run the same command again.");
          await beat(page, BEAT_SHORT);
        }
      }
      if (demo.id === "fail-then-approve" && i === 0) {
        await caption(page, "This command tries to get out.");
        await beat(page, BEAT_SHORT);
      }
      await typeInTerminal(page, cmd);
      // Let the command RESOLVE before moving on — ending a demo while a curl
      // is in flight kills the sandbox before its decision reaches the audit
      // log. Derived from the command's own --max-time; approve demos are
      // excluded (their curl is SUPPOSED to hang when we decide it).
      const maxTime = demo.approve ? null : cmd.match(/--max-time (\d+)/);
      await beat(page, maxTime ? (Number(maxTime[1]) + 2) * 1000 : PACE.read + 800);

      if (demo.id === "fail-then-approve" && i === 0) {
        await caption(page, "The first time, it fails.");
        await beat(page, BEAT_SHORT);
        await caption(page, "But now there's a question waiting for us.");
        await beat(page, PACE.read);
      }

      // The retry after an approval MUST visibly succeed — the payoff of
      // fail-then-approve. Without the assertion a failed retry just raises a
      // fresh approval the NEXT step latches onto, and the take narrates
      // success over a terminal showing two refusals.
      if (demo.approve && i === 1) {
        const term = page.locator(".xterm-screen").first();
        await centerInFrame(term);
        await pollScreen(term, /HTTP\/[\d.]+ 200/, "the approved retry never returned a 200");
        if (demo.id === "fail-then-approve") {
          await caption(page, "The command continues.");
          await beat(page, PACE.read);
          await caption(page, "Same request.");
          await beat(page, BEAT_SHORT);
          await caption(page, "Same sandbox.");
          await beat(page, BEAT_SHORT);
          await caption(page, "The only thing that changed was the decision.");
          await beat(page, PACE.read);
        }
      }
    }

    if (demo.id === "sealed-box") {
      // "Instant refusal on camera" is the whole beat: prove the refusal is on
      // camera. always_deny + empty allowlist → the proxy 403s the CONNECT.
      const term = page.locator(".xterm-screen").first();
      await centerInFrame(term);
      await pollScreen(term, /403|curl: \(\d+\)/, "sealed-box never showed a refusal on screen");
      // No decide() to attach the owner's SAY-ON-DECIDE line to — this policy
      // never asks a human — so it is spoken here, as the refusal lands.
      await caption(page, "The request is denied.");
      await beat(page, BEAT_SHORT);
      await caption(page, "The sandbox asked.");
      await beat(page, BEAT_SHORT);
      await caption(page, "The proxy said no.");
      await beat(page, BEAT_SHORT);
      await caption(page, "And the command gets a normal refusal.");
      await beat(page, PACE.read);
    }

    if (demo.approve && demo.cmds.length === 1) {
      // held-at-the-door: the curl is still hanging at the proxy right now.
      await caption(page, "Notice what's happening.");
      await beat(page, PACE.read);
      await caption(page, "The command hasn't failed.");
      await beat(page, BEAT_SHORT);
      await caption(page, "It's waiting.");
      await beat(page, BEAT_SHORT);
      await caption(page, "Nothing has been sent through the door yet.");
      await beat(page, PACE.read);
      await decide(page, "Approve", "Approve it.", "example.com");
      await caption(page, "And now the request completes.");
      await beat(page, PACE.read);
      await caption(page, "No retry.");
      await beat(page, BEAT_SHORT);
      await caption(page, "It was already waiting at the boundary.");
      await beat(page, PACE.read);

      // The owner's fourth test begins here, still inside this same sandbox —
      // the product's own "held-at-the-door" card carries the wikipedia+Deny
      // step as its third step, so this test rides it.
      await typeInTerminal(page, "curl -sSI --max-time 60 https://wikipedia.org");
      await beat(page, 1200);
      await caption(page, "Test four: walled off.");
      await beat(page, BEAT_SHORT);
      await caption(page, "Now another ordinary host.");
      await beat(page, BEAT_SHORT);
      await caption(page, "This one should be refused outright.");
      await beat(page, PACE.read);
      await decide(page, "Deny", "Deny.", "wikipedia.org");
      await expect(page.getByTestId("demo-audit-panel")).toContainText(/wikipedia/, { timeout: 30_000 });
      await spotlight(page, page.getByTestId("demo-audit-panel"));
      await caption(page, "The refusal lands beside the approval in the record.");
      await beat(page, PACE.read);
      await caption(page, "And then there's the address you really don't want an arbitrary workload reaching:");
      await beat(page, PACE.read);
      await caption(page, "the cloud metadata service.");
      await beat(page, BEAT_SHORT);
      await caption(page, "That's where a cloud machine's own credentials can live.");
      await beat(page, PACE.read);
      await spotlight(page, null);
    }

    if (demo.id === "lines-that-cant-be-crossed") {
      // Prove the block from the TERMINAL. The http:// metadata probe reaches
      // the egress proxy (NO_PROXY covers only proxy+localhost), which refuses
      // to dial it → `HTTP/1.1 403`; on a stack where the proxy is out of that
      // path it dies at the network layer (curl 7). Either shape is the same
      // fact; a success is not accepted.
      const term = page.locator(".xterm-screen").first();
      await pollScreen(
        term,
        /HTTP\/1\.1 403|Failed to connect to 169\.254\.169\.254|curl: \(\d+\)/,
        "the metadata probe was not refused on screen",
      );
      await centerInFrame(term);
      await spotlight(page, term);
      await caption(page, "Here, the request doesn't even get a chance to connect.");
      await beat(page, PACE.read);
      await caption(page, "The proxy refuses it.");
      await beat(page, BEAT_SHORT);
      await caption(page, "The kernel has no route there either.");
      await beat(page, BEAT_SHORT);
      await spotlight(page, null);

      // No pending decision, because there was never a decision to make. Assert
      // the strip really is empty — narrating "no approval was raised" over a
      // pending row would invert the lesson.
      await expect(page.getByTestId("live-approval-row")).toHaveCount(0);
      await caption(page, "And importantly, there's no approval prompt.");
      await beat(page, PACE.read);
      await caption(page, "Some destinations aren't merely disallowed.");
      await beat(page, BEAT_SHORT);
      await caption(page, "They're outside the set of things a human can approve.");
      await beat(page, PACE.read + 900);
    }

    const endDemo = page.getByRole("button", { name: "End demo" });
    if (await endDemo.isVisible().catch(() => false)) await act(page, endDemo);
    // The quartet is consecutive in catalog order, so Next never has to skip an
    // interleaved step. After the fourth, this lands us on the fifth egress
    // step; the next act navigates explicitly regardless.
    await advance();
  }
});

// ===========================================================================
// ACTS 3-5 — THE SECRETS BASICS
// ===========================================================================

// ---------------------------------------------------------------------------
// Act 3 — a key of your own. One masked write on camera (lifted from old-07's
// beat 6): it stores wardyn-demo-key AND teaches the store's write-only door,
// the precondition the mechanism demos borrow.
// ---------------------------------------------------------------------------
test("V03a act 3 — a key of your own", async () => {
  test.setTimeout(180_000);
  const page = stage();

  await chapter(page, "A key of your own", "The other kind of boundary — a value, not a destination");
  await caption(page, "So far, we've been governing where a run may reach.");
  await beat(page, PACE.read);
  await caption(page, "A secret is the other axis: a value a run may use, without ever holding it.");
  await beat(page, PACE.read);
  await caption(page, "But most runs need a key that's yours.");
  await beat(page, BEAT_SHORT);

  await page.goto("/secrets");
  await expect(page.getByRole("heading", { name: "Secrets", level: 1 })).toBeVisible({ timeout: 30_000 });

  await act(page, page.getByRole("button", { name: "Add secret", exact: true }), "Store one now.");
  const dlg = page.getByRole("dialog");
  await expect(dlg.getByRole("heading", { name: "Add secret" })).toBeVisible();
  await dlg.getByLabel("Name").fill(DEMO_KEY);
  await caption(page, "The name is the handle.");
  await beat(page, BEAT_SHORT);
  // The Value field masks at entry (secrets.tsx's -webkit-text-security), so
  // the canary's glyphs are never on screen — one beat here, the full lesson is
  // still the write-only-by-design demo next.
  await dlg.getByLabel("Value", { exact: true }).fill(DEMO_KEY_VALUE);
  await caption(page, "The value is masked the moment it's typed.");
  await beat(page, PACE.read);
  await act(page, dlg.getByRole("button", { name: "Save secret" }), "Save it.");
  await expect(dlg).toBeHidden({ timeout: 30_000 });

  const row = page.getByRole("row", { name: new RegExp(DEMO_KEY) });
  await expect(row).toBeVisible({ timeout: 30_000 });
  // The negative IS the claim, and it holds for the rest of the take.
  await expect(page.locator("body"), "the demo key's VALUE rendered on screen after save").not.toContainText(
    DEMO_KEY_VALUE,
  );
  await spotlight(page, row);
  await caption(page, "That's the last time anyone sees that value.");
  await beat(page, PACE.read);
  await caption(page, "The demos ahead use this key by name.");
  await beat(page, BEAT_SHORT);
  await caption(page, "None of them can read it. The ones we authorize can still use it.");
  await beat(page, BEAT_SHORT + 400);
  await spotlight(page, null);
});

// ---------------------------------------------------------------------------
// Act 4 — write-only, proved from inside (write-only-by-design). Lifted from
// old-04's beat 5; re-pointed at wardyn-demo-key (the card's own probe secret),
// so the 404 is for the very key just saved on camera. `curl -i` deviation kept
// so the 404 STATUS LINE is on screen (unnarrated mechanics, per episode 10).
// ---------------------------------------------------------------------------
test("V03a act 4 — write-only, proved from inside", async () => {
  test.setTimeout(600_000);
  const page = stage();

  // The row's chip carries the product's one honest sentence for write-only
  // (secrets.tsx's WRITE_ONLY_TOOLTIP). Located BY that sentence, so a reworded
  // tooltip fails the take here instead of outliving the product's own words.
  const row = page.getByRole("row", { name: new RegExp(DEMO_KEY) });
  const writeOnlyChip = row.getByTitle(/never read back — not even by you/);
  await centerInFrame(writeOnlyChip);
  await spotlight(page, writeOnlyChip);
  await caption(page, "The store tells us the rule. Now let's test whether the rule is actually true.");
  await beat(page, BEAT_SHORT);
  await caption(page, "Write-only — you can replace it, you can delete it, you can never read it.");
  await beat(page, PACE.read);
  await caption(page, "But that's Wardyn describing itself.");
  await beat(page, BEAT_SHORT);
  await caption(page, "Let's make it prove it, from inside a sandbox.");
  await beat(page, BEAT_SHORT + 400);
  await spotlight(page, null);

  const card = await openDemo(page, "write-only-by-design", "Write-only, even for you");
  const start = card.getByTestId("demo-start-write-only-by-design");
  await expect(start, "the demo Start button is disabled — this stack has no ready barrier").toBeEnabled({
    timeout: 60_000,
  });
  await caption(page, "This sandbox has no permission to use a secret.");
  await beat(page, BEAT_SHORT);
  // The policy is the sealed box again — and crucially, no eligible_grants. The
  // absence IS the point here, so ring the whole block rather than one line.
  await spotlight(page, card.getByTestId("demo-policy-write-only-by-design"));
  await caption(page, "The policy is the sealed box again — and there's no eligible_grants section at all.");
  await beat(page, PACE.read);
  await caption(page, "Nothing was ever going to hand it our secret.");
  await beat(page, BEAT_SHORT);
  await spotlight(page, null);
  await act(page, start, "Start the demo.");

  await beat(page, 200);
  await ffwdStart(page);
  try {
    await expect(card.locator(".xterm-screen").first()).toBeVisible({ timeout: SANDBOX_UP });
  } finally {
    await ffwdEnd(page);
  }
  await noteDemoRun(page, "write-only-by-design");
  await beat(page, PACE.read);
  // Cockpit framing: policy at the top, terminal + audit below, one frame.
  await frameRun(page, "write-only-by-design");

  const screen = card.locator(".xterm-screen").first();
  await typeInTerminal(page, "printenv | sort", card);
  await pollScreen(screen, /WARDYN_PROXY_URL=/, "printenv never echoed inside the demo sandbox");
  const envText = await screen.innerText();
  expect(envText.includes(DEMO_KEY_VALUE), "the stored secret's VALUE printed inside the demo sandbox").toBe(false);
  expect(
    envText.includes(DEMO_KEY),
    `"${DEMO_KEY}" reached the demo sandbox — this demo's policy carries no grant, so nothing should have injected it`,
  ).toBe(false);
  await caption(page, "The proxy's address is in there. The secret value isn't.");
  await beat(page, PACE.read);

  await caption(page, "So ask Wardyn for it directly — by name, from inside the box.");
  await beat(page, PACE.read);
  await typeInTerminal(page, `curl -sS -i --noproxy '*' "$WARDYN_PROXY_URL/wardyn/v1/secrets/${DEMO_KEY}"`, card);
  await pollScreen(
    screen,
    /404|unknown brokered route/,
    "the read-back probe never answered — expected a 404 from the proxy's brokered routes",
  );
  await expect(page.locator("body"), "the stored key's VALUE rendered after the read-back probe").not.toContainText(
    DEMO_KEY_VALUE,
  );
  await caption(page, "Four-oh-four. The read-back request has nowhere to go.");
  await beat(page, BEAT_SHORT);
  await caption(page, "Not 'forbidden'. Wardyn has no read-back route for that secret.");
  await beat(page, PACE.read);
  await caption(page, "The run can't read it back — and neither can the operator through this store.");
  await beat(page, PACE.read);
  await caption(page, "Write-only isn't a permission you can escalate past. There simply isn't a read-back operation.");
  await beat(page, PACE.read + 400);
  await spotlight(page, null);
  await act(page, card.getByRole("button", { name: "End demo" }));
});

// ---------------------------------------------------------------------------
// Act 5 — the key that never enters the box (key-never-in-the-box). Lifted
// verbatim from old-07's beat 7: the header is stitched on OUTBOUND at the
// proxy, so the proof is the audit panel, never the response.
// ---------------------------------------------------------------------------
test("V03a act 5 — the key that never enters the box", async () => {
  test.setTimeout(900_000);
  const page = stage();

  const card = await openDemo(page, "key-never-in-the-box", "The key that never enters the box");

  await caption(page, "This demo gives the run permission to use one secret by name.");
  await beat(page, PACE.read);
  // The secrets group adds a whole new section to the policy — walk it once,
  // the way sealed-box walked the egress lines. Later secrets demos diff it.
  await walkPolicyKey(page, card, "key-never-in-the-box", "eligible_grants",
    "A new section appears: eligible_grants. This is where a run is authorized to USE a secret.");
  await walkPolicyKey(page, card, "key-never-in-the-box", "kind",
    "The kind — an API key, attached to outbound requests as a header.");
  await walkPolicyKey(page, card, "key-never-in-the-box", "scope",
    "And the scope: which host, which header — and the secret's NAME.",
    "Not the secret itself. The value never appears in the policy, and never enters the box.");
  await spotlight(page, null);

  const screen = await startAndBoot(page, card, "key-never-in-the-box");

  // THE AUDIT PANEL FIRST — before a single command. The injected header is
  // invisible from inside the box by construction, so the proof is the record.
  const auditRows = card.getByTestId("demo-audit-rows");
  await expect(
    auditRows,
    "no credential.mint row — the inline grant never minted (member role clamps it away, or the secret is missing)",
  ).toContainText("credential.mint", { timeout: 60_000 });
  await expect(auditRows).toContainText("secret.read");
  await centerInFrame(auditRows);
  await spotlight(page, auditRows);
  await caption(page, "Before we type anything, look at the record.");
  await beat(page, PACE.read);
  await caption(
    page,
    "Wardyn has already authorized the proxy to use the secret; the secret value still isn't in the box.",
  );
  await beat(page, PACE.read);
  await caption(page, "That happened outside the box, before the request crossed the boundary.");
  await beat(page, PACE.read + 400);
  await spotlight(page, null);

  // Cockpit re-pin: the audit beat above centered the record and pushed the
  // policy off the top; put policy + terminal + audit back in one frame before typing.
  await frameRun(page, "key-never-in-the-box");
  await typeInTerminal(page, "printenv | sort", card);
  await pollScreen(screen, /WARDYN_PROXY_URL=/, "printenv never echoed inside the demo sandbox");
  const envText = await screen.innerText();
  expect(envText.includes(DEMO_KEY_VALUE), "the granted key's VALUE printed inside the sandbox").toBe(false);
  expect(
    envText.includes(DEMO_KEY),
    "the granted key's NAME reached the sandbox — injection is proxy-side, nothing should carry it in",
  ).toBe(false);
  await caption(page, "The key isn't in the environment.");
  await beat(page, BEAT_SHORT);
  await caption(page, "Its name isn't either.");
  await beat(page, BEAT_SHORT);

  await typeInTerminal(page, "curl -sSI http://example.com", card);
  await pollScreen(screen, /HTTP\/[\d.]+ 200/, "the allowed host never answered — expected a 200 through the proxy");
  await caption(page, "And yet the request left with the credential attached.");
  await beat(page, PACE.read);
  await caption(
    page,
    "The sandbox sent the request without the secret. The proxy attached the header before forwarding it.",
  );
  await beat(page, PACE.read);
  await caption(
    page,
    "The response can't prove where the credential came from. The audit record shows the proxy attached it.",
  );
  await beat(page, PACE.read + 400);

  await typeInTerminal(page, "curl -sSI http://wikipedia.org", card);
  await pollScreen(screen, /403/, "the unlisted host was not refused — always_deny should 403 it instantly");
  await caption(page, "One more thing.");
  await beat(page, BEAT_SHORT);
  await caption(page, "Authorizing a run to use a credential never widens where it can go.");
  await beat(page, PACE.read);
  await caption(page, "This host was never on the list.");
  await beat(page, BEAT_SHORT);
  await caption(page, "Refused instantly.");
  await beat(page, BEAT_SHORT + 400);
  await act(page, card.getByRole("button", { name: "End demo" }));
});

// ===========================================================================
// Conclusion — recap both groups, hand off to episode 04 (Add a workspace).
// ===========================================================================
test("V03a conclusion", async () => {
  test.setTimeout(60_000);
  const page = stage();

  await chapter(page, "What you just saw", "Two kinds of boundary, proved on camera");
  await caption(page, "You saw a boundary hold two different ways.");
  await beat(page, PACE.read);
  await caption(page, "A destination — denied, held for a decision, or walled off entirely.");
  await beat(page, PACE.read);
  await caption(page, "And a secret — used by a run that never once held it.");
  await beat(page, PACE.read);
  await caption(page, "None of those were screenshots.");
  await beat(page, PACE.read);
  await caption(page, "They were real requests, inside real sandboxes, under real policies.");
  await beat(page, PACE.read);
  await caption(page, "And we're ready to give it some actual work.");
  await beat(page, PACE.read);
  // [OWNER SLOT — drafted] The only pointer at the detours anywhere in the core.
  await caption(page, "Optional detours cover the rest of the demos — the network three more ways, and a secret five ways.");
  await beat(page, PACE.read);
  await caption(page, "");
  await silentCard(page, "Next — 04: Add a workspace");
  await caption(page, "Next, we'll give a run something real to work on.");
  await beat(page, PACE.read);
  await caption(page, "A workspace.");
  await beat(page, BEAT_SHORT + 400);
  await caption(page, "");
});
