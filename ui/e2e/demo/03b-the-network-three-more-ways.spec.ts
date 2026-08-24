/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

/*
 * Episode 03b of the series — "The network, three more ways". OPTIONAL DETOUR.
 *
 * WHERE IT SITS. A detour off 03a (the core "What it stops"), watched right
 * after it, handing straight back to 04 — nothing in the core path depends on
 * it. Driven by `scripts/record-demo.sh --video 03b`, which globs this exact
 * filename.
 *
 * WHAT THIS FILMS — the three egress demos 03a's quartet leaves out, each RUN
 * in full, not named in passing:
 *   ACT 1 agent-in-the-box — the flagship: a real coding agent in the same box,
 *     reaching Anthropic and nothing else. The one act on the whole series that
 *     needs a connected model, which is exactly why it is quarantined here:
 *     probe the model quota before the take, because an out-of-quota `claude`
 *     still logs egress.allow api.anthropic.com while doing nothing — the honest
 *     proof is the FILE it writes (the WROTE_HELLO marker), not the audit.
 *   ACT 2 record-a-policy — run wide open once, then synthesize the least-
 *     privilege allowlist from what it actually reached.
 *   ACT 3 once-or-for-good — how long an approval lasts: Once spends itself on
 *     the single connection it was raised for.
 * Each of the three also headlines a later episode (agent -> 08, record -> 09,
 * scope caret -> 10); here they run end to end, as the complete network story.
 *
 * PROVENANCE. Split from 03-what-it-stops.spec.ts 2026-08-24 (acts 3/3b/3c),
 * lines moved verbatim. The chapter card, the opener and the close are
 * [OWNER SLOT — drafted] in local/episode-03-mega-proposal.md;
 * local/episode-03-stanza-check.py fails if a spec string and a stanza drift.
 *
 * STAGING THIS FILE OWNS (off camera): sweepStaleState() only. None of these
 * three demos carries `needsSecret`, so there is no secret to stage — but
 * agent-in-the-box carries `needsModel`, and stepOrder(status) DROPS it until a
 * model is connected, which would send act 1's deep link to a neighbouring card.
 * A model on this stack is the one precondition this episode cannot stage
 * itself. Any episode re-takes alone, in any order.
 *
 * This is NOT a test. It asserts only enough to stay honest and to know when to
 * advance; a failure here means the recording is wrong, not the product.
 */

import { test, expect } from "@playwright/test";
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
// Importing stage.ts is what registers this file's own recorded context.
import { stage } from "./stage";
import { decide } from "./funnel";
import { sweepStaleState } from "./sweep";
import {
  BEAT_SHORT,
  openDemo,
  openEpisode,
  pillCmd,
  pollScreen,
  silentCard,
  startAndBoot,
  walkPolicyKey,
} from "./demos";

test.skip(!process.env.WARDYN_DEMO, "demo recording — run via `make record-demo` (exports WARDYN_DEMO=1)");

test.describe.configure({ mode: "serial" });


test.beforeAll(async () => {
  if (!process.env.WARDYN_DEMO) return;
  // S6: deny stale pending approvals / kill stale runs first — otherwise the
  // Approvals badge carries a prior take's number through the whole video.
  // No secrets: none of these three demos carries `needsSecret`.
  await sweepStaleState();
});

// ---------------------------------------------------------------------------
// Act 1 — the flagship, and the episode's cold open. agent-in-the-box needs a
// live model: probe quota before the take (WEEKLY-LIMIT risk — an out-of-quota
// `claude` still logs egress.allow api.anthropic.com while doing nothing, so
// the honest proof is the FILE it writes, asserted via a WROTE_HELLO marker,
// not the audit). As the FIRST demo of its episode it walks the whole policy,
// not just the line that changed — a viewer who started here saw no baseline.
// ---------------------------------------------------------------------------
test("V03b act 1 — the agent in the box", async () => {
  test.setTimeout(900_000);
  const page = stage();

  const card = await openEpisode(page, "agent-in-the-box", "The agent in the box");

  await chapter(page, "The network, three more ways", "The other three demos — each also headlines a later episode");
  // [OWNER SLOT — drafted] The detour's own opener: 03a's viewer arrives here
  // having seen the quartet, so this says what the three are and that they run
  // in full rather than being named in passing.
  await caption(page, "Three more network demos. Each of these also headlines a later episode — here they run in full.");
  await beat(page, PACE.read);

  await spotlight(page, page.getByTestId("demo-policy-agent-in-the-box"));
  await caption(page, "First, the flagship: the same box, now running a real coding agent.");
  await beat(page, PACE.read);
  await caption(page, "It reaches Anthropic to think — and nothing else.");
  await beat(page, BEAT_SHORT);
  await spotlight(page, null);

  await walkPolicyKey(page, card, "agent-in-the-box", "min_confinement_class",
    "The barrier: CC1, Fence — the lightest sandbox tier, the wall between this run and your host.");
  await walkPolicyKey(page, card, "agent-in-the-box", "auto_stop_after_sec",
    "A dead-man's switch: if nothing else stops it, the run halts itself after fifteen minutes.");
  await walkPolicyKey(page, card, "agent-in-the-box", "allowed_domains",
    "The change is the allowlist: two entries now — Anthropic's API.",
    "That's the whole network contract. The agent can reach the model, and no other host.");
  await walkPolicyKey(page, card, "agent-in-the-box", "first_use_approval",
    "And for anything not listed: always deny. Refused the instant it's dialed — no prompt, no wait.");

  const screen = await startAndBoot(page, card, "agent-in-the-box");

  // Step 1 — the agent task. In -p mode Claude Code writes the file and exits.
  const agentCmd = await pillCmd(card, 0);
  await caption(page, "Attach the terminal and hand it a one-shot task.");
  await beat(page, BEAT_SHORT);
  await typeInTerminal(page, agentCmd, card);
  await caption(page, "It authenticates through the model you connected — injected proxy-side, never resident in the box.");
  await beat(page, PACE.read);
  // The agent's thinking is dead air on camera; compress it. The span ends when
  // the honest proof lands: the FILE, not the audit (an out-of-quota agent still
  // logs egress.allow to Anthropic — see the act header).
  await ffwdStart(page);
  await typeInTerminal(page, "test -s HELLO.md && echo WROTE_HELLO || echo NO_HELLO", card);
  await pollScreen(screen, /WROTE_HELLO/, "the agent never wrote HELLO.md — model quota exhausted, or the run did no work");
  await ffwdEnd(page);
  await caption(page, "It did the work, inside the box, and wrote its file.");
  await beat(page, PACE.read);

  // Step 2 — the same policy still holds against the agent's box.
  const offlist = await pillCmd(card, 1);
  await typeInTerminal(page, offlist, card);
  await pollScreen(screen, /\b(403|refused|Could not resolve|Failed to connect)\b/i, "example.com was not refused inside the agent box");
  await caption(page, "Any host off the allowlist is refused, exactly as it was for the human.");
  await beat(page, PACE.read);

  // Step 3 — the record.
  const auditRows = card.getByTestId("demo-audit-rows");
  await expect(auditRows, "no egress decisions recorded for the agent run").toContainText("api.anthropic.com", {
    timeout: 60_000,
  });
  await centerInFrame(auditRows);
  await spotlight(page, auditRows);
  await caption(page, "Every decision on the record — allowed to Anthropic, denied elsewhere, attributed to the run.");
  await beat(page, PACE.read);
  await caption(page, "The same confinement as the four tests. This is the job Wardyn exists for.");
  await beat(page, PACE.read);
  await spotlight(page, null);

  await act(page, card.getByRole("button", { name: "End demo" }));
});

test("V03b act 2 — record a policy", async () => {
  test.setTimeout(900_000);
  const page = stage();

  const card = await openDemo(page, "record-a-policy", "Record a policy");

  await spotlight(page, page.getByTestId("demo-policy-record-a-policy"));
  await caption(page, "The second flips the usual order.");
  await beat(page, BEAT_SHORT);
  await caption(page, "Instead of guessing an allowlist upfront, run wide open once and let Wardyn watch.");
  await beat(page, PACE.read);
  await spotlight(page, null);

  await walkPolicyKey(page, card, "record-a-policy", "allow_all_egress",
    "The allowlist is empty, but allow-all-egress is true — wide open on purpose.",
    "You can't record what a policy already blocks.");

  // Boots the sandbox; the record-a-policy proof reads the audit panel, not the
  // terminal, so the screen locator is not needed here.
  await startAndBoot(page, card, "record-a-policy");

  // Three real hosts, all recorded (allow_all_egress) — nothing is denied while
  // recording; every host reached becomes a candidate line. The PROOF is the
  // record, not the terminal status line: a HEAD's headers scroll out of the
  // xterm viewport in a blink (the run's audit confirms egress.allow landed),
  // so assert the AUDIT PANEL — which is exactly what this demo is teaching:
  // what gets recorded is what becomes the policy.
  const auditRows = card.getByTestId("demo-audit-rows");
  const recorded = [
    { i: 0, host: "pypi.org", line: "Reach out to a package registry — recorded, not blocked." },
    { i: 1, host: "registry.npmjs.org", line: "A second registry. Every host it touches becomes a candidate line." },
    { i: 2, host: "example.com", line: "A third, unrelated host — recorded the same way." },
  ];
  for (const { i, host, line } of recorded) {
    const cmd = await pillCmd(card, i);
    await typeInTerminal(page, cmd, card);
    await caption(page, line);
    await beat(page, PACE.read);
    await expect(auditRows, `${host} never landed in the audit panel — the recording missed it`).toContainText(host, {
      timeout: 30_000,
    });
  }
  await centerInFrame(auditRows);
  await spotlight(page, auditRows);
  await caption(page, "Every host it reached is on the record — the raw material for the policy.");
  await beat(page, PACE.read);
  await spotlight(page, null);

  await act(page, card.getByRole("button", { name: "End demo" }));

  // The payoff: synthesize the least-privilege policy from what it actually did.
  await act(page, card.getByTestId("demo-turn-into-policy-record-a-policy"), "Now turn what it did into a policy.");
  const sheet = page.getByRole("dialog").filter({ hasText: "Proposed allowed domains" });
  const proposed = sheet.getByText("Proposed allowed domains", { exact: true }).locator("xpath=..");
  await expect(proposed, "the synthesis never proposed the recorded hosts").toContainText("pypi.org", {
    timeout: 60_000,
  });
  await centerInFrame(proposed);
  await spotlight(page, proposed);
  await caption(page, "Wardyn read back exactly what it reached, and proposes the allowlist that would have let it through.");
  await beat(page, PACE.read);
  await caption(page, "Approve it, and the next run is confined to only that. Episode nine drives this on a real workspace.");
  await beat(page, PACE.read);
  await spotlight(page, null);
  // Close the sheet — saving the policy is episode nine's beat, not this showcase's.
  await page.keyboard.press("Escape");
});

test("V03b act 3 — once, or for good", async () => {
  test.setTimeout(900_000);
  const page = stage();

  const card = await openDemo(page, "once-or-for-good", "Once, or for good");

  await spotlight(page, page.getByTestId("demo-policy-once-or-for-good"));
  await caption(page, "The last one is about how long an approval lasts.");
  await beat(page, BEAT_SHORT);
  await caption(page, "Every approval so far stuck around for the rest of the run. Once is narrower.");
  await beat(page, PACE.read);
  await spotlight(page, null);

  await walkPolicyKey(page, card, "once-or-for-good", "first_use_approval",
    "The policy is back to deny-with-review — so a request is held for you.",
    "What's new isn't the policy line. It's how you answer: once, or for the whole run.");

  const screen = await startAndBoot(page, card, "once-or-for-good");

  // First attempt — refused, raises an approval.
  const first = await pillCmd(card, 0);
  await typeInTerminal(page, first, card);
  await pollScreen(screen, /\b(403|refused)\b/i, "the first request was not refused — deny_with_review should hold it");
  await caption(page, "Refused — and an approval appears below the terminal.");
  await beat(page, PACE.read);

  // Grant it ONCE — via the split button's caret, not a plain Approve.
  await decide(
    page,
    "Approve",
    "Approve it — but only this one connection.",
    "example.com",
    "once",
    "Once spends itself on the single connection it was raised for.",
  );

  // Same command — now it gets through.
  const second = await pillCmd(card, 1);
  await typeInTerminal(page, second, card);
  await pollScreen(screen, /HTTP\/[\d.]+ 200/, "the Once-approved retry never returned a 200");
  await caption(page, "Same command — this time it's through.");
  await beat(page, PACE.read);

  // Run it a third time — the Once grant already spent itself, so it asks again.
  await typeInTerminal(page, second, card);
  await expect(
    page.getByTestId("live-approval-row").filter({ hasText: "example.com" }),
    "the third attempt did not raise a fresh approval — Once should have spent itself",
  ).toBeVisible({ timeout: 60_000 });
  await caption(page, "Run it again and it's refused all over again — a brand-new approval.");
  await beat(page, PACE.read);
  await caption(page, "Nothing lingered by accident. One connection, not the run. Episode ten drives the full scope ladder.");
  await beat(page, PACE.read);

  await act(page, card.getByRole("button", { name: "End demo" }));
});

// ===========================================================================
// Conclusion — the detour closes and hands back to the core path (04).
// ===========================================================================
test("V03b conclusion", async () => {
  test.setTimeout(60_000);
  const page = stage();

  await chapter(page, "Back to the main path", "");
  // [OWNER SLOT — drafted]
  await caption(page, "That's the network, complete.");
  await beat(page, PACE.read);
  await caption(page, "");
  await silentCard(page, "Next — 04: Add a workspace");
});
