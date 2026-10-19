/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

/*
 * Episode 03d of the series — "The kinds that can't use a header". OPTIONAL
 * DETOUR.
 *
 * WHERE IT SITS. A detour off 03a (the core "What it stops"), watched after 03c,
 * handing straight back to 04 — nothing in the core path depends on it. Driven
 * by `scripts/record-demo.sh --video 03d`, which globs this exact filename. It
 * finishes the ladder 03c started: header-injected -> piped -> SIGNED AT THE
 * PROXY -> RE-ORIGINATED -> REFUSED.
 *
 * WHAT THIS FILMS — the three credential kinds a header cannot carry:
 *   ACT 1 ssh-at-the-proxy — git's SSH transport has no credential-helper
 *     seam, so the proxy holds the key and signs the clone's sign-in as an ssh
 *     agent. The agent closes after the start-up clones, before attach; every
 *     way in from the sandbox (a key file, the mint route, the agent) is shown
 *     refused.
 *   ACT 2 github-app-broker — TEACH+GATE: its installation token is minted from
 *     the LIVE GitHub API, so it cannot be faked locally. Start stays disabled
 *     behind the `needsGitHubApp` gate copy and the card teaches the lane.
 *   ACT 3 sts-fail-closed — the CREATE refusal IS the demo: a cloud_sts grant
 *     needs an attested identity, and on the embedded provider there is none, so
 *     run-create 422s before any sandbox is built.
 * Two of the three never start a sandbox, and that is the lesson, not a gap.
 *
 * PROVENANCE. Split from 03-what-it-stops.spec.ts 2026-08-24 (acts 10/11/12),
 * lines moved verbatim — including the conclusion's "Five credential types,
 * five different boundaries" payoff, which moved here from the 03 conclusion
 * because this is where the fifth kind lands. The chapter card, the opener and
 * the close are [OWNER SLOT — drafted] in the take-3 proposal; an
 * untracked stanza-check script fails if a spec string and a stanza drift.
 *
 * STAGING THIS FILE OWNS (off camera): sweepStaleState(), then
 * wardyn-demo-ssh-key, because ssh-at-the-proxy carries `needsSecret` and
 * stepOrder(status) DROPS a demo whose secret is missing — a `?step=` to a
 * dropped card silently re-corrects to a neighbour and films the wrong thing.
 * The MASKED ENTRY of a secret is taught once on camera in 03a with
 * wardyn-demo-key; staging this one off camera is the same lesson, not a hidden
 * one — and it is what lets 03d re-take alone, in any order. The stored key is a
 * canary sentinel, not a real PEM: nothing authenticates, by design.
 *
 * This is NOT a test. It asserts only enough to stay honest and to know when to
 * advance; a failure here means the recording is wrong, not the product.
 */

import { test, expect } from "@playwright/test";
import { act, beat, caption, centerInFrame, chapter, PACE, spotlight, typeInTerminal } from "./overlay";
// Importing stage.ts is what registers this file's own recorded context.
import { stage } from "./stage";
import { sweepStaleState } from "./sweep";
import {
  BEAT_SHORT,
  openDemo,
  openEpisode,
  pillCmd,
  pollScreen,
  putSecret,
  silentCard,
  SSH_SECRET,
  SSH_VALUE,
  startAndBoot,
  walkPolicyKey,
  frameRun,
  noteDemoRun,
} from "./demos";

test.skip(!process.env.WARDYN_DEMO, "demo recording — run via `make record-demo` (exports WARDYN_DEMO=1)");

test.describe.configure({ mode: "serial" });


test.beforeAll(async () => {
  if (!process.env.WARDYN_DEMO) return;
  const page = stage();
  // S6: deny stale pending approvals / kill stale runs first — otherwise the
  // Approvals badge carries a prior take's number through the whole video.
  await sweepStaleState();
  // ssh-at-the-proxy carries `needsSecret`. Its masked entry is the same
  // lesson 03a teaches once on camera with wardyn-demo-key, so staging it here
  // off camera is what lets 03d re-take alone, in any order.
  await putSecret(page, SSH_SECRET, SSH_VALUE);
});

// ---------------------------------------------------------------------------
// Act 1 — the SSH key that stays at the proxy (ssh-at-the-proxy). git's SSH
// transport has no credential-helper seam, so the proxy signs instead: it holds
// the key and answers as the ssh agent, and closes the agent after the start-up
// clones. Commands off the card's pills (two carry a {grant_id}); the
// load-bearing proof is that no key file exists and both proxy routes refuse.
// As the FIRST demo of its episode it walks the whole policy, not just the line
// that changed — a viewer who started here saw no baseline to diff against.
// ---------------------------------------------------------------------------
test("V03d act 1 — the SSH key that stays at the proxy", async () => {
  test.setTimeout(900_000);
  const page = stage();

  const card = await openEpisode(page, "ssh-at-the-proxy", "The SSH key that stays at the proxy");

  await chapter(page, "The kinds that can't use a header", "Signed at the proxy, brokered, or refused");
  // [OWNER SLOT — drafted] The detour's own opener.
  await caption(page, "Three credential kinds that can't be injected as a header — and what Wardyn does instead.");
  await beat(page, PACE.read);
  await caption(page, "One we can try here; one stays gated until you configure a GitHub App; one is refused before a sandbox exists — and that refusal is the lesson.");
  await beat(page, BEAT_SHORT);

  await spotlight(page, page.getByTestId("demo-policy-ssh-at-the-proxy"));
  await caption(page, "SSH can't take a header, and git's SSH has no credential helper.");
  await beat(page, PACE.read);
  await caption(page, "So the proxy holds the key and signs the sign-in for ssh, as its agent. The key never comes into the box.");
  await beat(page, PACE.read);
  await walkPolicyKey(page, card, "ssh-at-the-proxy", "min_confinement_class",
    "Same barrier as the core — Fence.");
  await walkPolicyKey(page, card, "ssh-at-the-proxy", "auto_stop_after_sec",
    "Same fifteen-minute idle stop.");
  await walkPolicyKey(page, card, "ssh-at-the-proxy", "eligible_grants",
    "The eligible grants section is back; the kind is what's different.");
  await walkPolicyKey(page, card, "ssh-at-the-proxy", "kind",
    "kind: SSH key — signed for at the proxy, never written into the sandbox.",
    "A start-up clone reaches the proxy's agent through a socket that exists only while that one ssh runs.");
  await spotlight(page, null);

  const screen = await startAndBoot(page, card, "ssh-at-the-proxy");

  // Step 0: ls ~/.ssh — empty. No key file was ever written.
  await typeInTerminal(page, await pillCmd(card, 0), card);
  await beat(page, PACE.read);
  await caption(page, "This listing is empty, and it always was: no key file is written for a clone.");
  await beat(page, PACE.read);

  // Step 1: the raw mint by grant id — withheld.
  await typeInTerminal(page, await pillCmd(card, 1), card);
  await pollScreen(screen, /never enters the sandbox/, "the mint route did not refuse the ssh key");
  await caption(page, "Ask the mint route for the key by its grant id: refused. The proxy withholds it.");
  await beat(page, PACE.read);

  const auditRows = card.getByTestId("demo-audit-rows");
  await expect(auditRows, "no credential.mint row for the ssh key").toContainText("credential.mint", {
    timeout: 60_000,
  });
  await centerInFrame(auditRows);
  await spotlight(page, auditRows);
  await caption(page, "The record shows that mint: issued to the proxy, which kept it. The grant is issued once per run.");
  await beat(page, PACE.read + 400);
  await spotlight(page, null);

  // Cockpit re-pin: the audit beat above centered the record and pushed the
  // policy off the top; put policy + terminal + audit back in one frame before typing.
  await frameRun(page, "ssh-at-the-proxy");
  // Step 2: the agent itself — closed after the start-up clones.
  await typeInTerminal(page, await pillCmd(card, 2), card);
  await pollScreen(screen, /start-up clone finished/, "the ssh agent did not answer closed");
  await caption(page, "And the agent itself closed when the start-up clones finished.");
  await beat(page, BEAT_SHORT);
  await caption(page, "Nothing in this sandbox can have the proxy sign with the key now.");
  await beat(page, PACE.read);

  // Step 3: ssh straight to the host — nothing to offer, narrow reach.
  await typeInTerminal(page, await pillCmd(card, 3), card);
  await beat(page, PACE.read + 800);
  await caption(page, "Now try SSH directly. There is no key in here to offer, so the host refuses.");
  await beat(page, BEAT_SHORT);
  await caption(page, "And notice what the connection was even allowed to reach: one SSH host, on 443.");
  await beat(page, PACE.read);
  await caption(page, "What you get: no key file, no key on the mint route, and an agent open only for the start-up clone.");
  await beat(page, BEAT_SHORT);
  await act(page, card.getByRole("button", { name: "End demo" }));
});

// ---------------------------------------------------------------------------
// Act 2 — a token the sandbox never even sees (github-app-broker). TEACH+GATE:
// its installation token is minted from the LIVE GitHub API, so it can't be
// faked locally — the card teaches the broker lane and Start stays disabled
// behind gate copy (the `needsGitHubApp` gate). Filmed as teach + gate.
// ---------------------------------------------------------------------------
test("V03d act 2 — a token the sandbox never sees", async () => {
  test.setTimeout(180_000);
  const page = stage();

  const card = await openDemo(page, "github-app-broker", "A token the sandbox never even sees");
  await caption(page, "One step further out. Git can be handed a token on a pipe — but this kind never enters the sandbox at all, in any form.");
  await beat(page, PACE.read);

  await spotlight(page, card.getByTestId("demo-policy-github-app-broker"));
  await caption(page, "git is pointed at a broker route on the proxy.");
  await beat(page, BEAT_SHORT);
  await caption(page, "It mints a short-lived, repo-scoped token from the live GitHub API and attaches it on its own outbound leg.");
  await beat(page, PACE.read);
  await caption(page, "Ask for it by name from inside and you'd be refused — we can't type that here; read on.");
  await beat(page, PACE.read);
  await walkPolicyKey(page, card, "github-app-broker", "kind",
    "kind: GitHub token — scoped to one repo and one permission. Anything in the box can reach that broker route; it only ever yields this repo, with this permission.",
    "One step past that pipe — in the other detour, a Git token was issued onto a pipe inside the box. This kind never enters the box in any form: it's attached on the proxy's outbound leg alone.");
  await spotlight(page, null);

  // The gate is the demo here: Start is disabled and the gate copy explains
  // why. On a stack with a GitHub App configured this would run instead; the
  // recording stack has none, so we film the honest closed door.
  await expect(
    card.getByTestId("demo-needs-github-app"),
    "the github-app gate copy never rendered — is a GitHub App configured on this stack?",
  ).toBeVisible({ timeout: 30_000 });
  // The gate means no run was ever created: record the ABSENCE for the grader
  // (noteDemoRun warns "no run id" on a correct take — that warning is the lesson).
  await noteDemoRun(page, "github-app-broker");
  await spotlight(page, card.getByTestId("demo-needs-github-app"));
  await caption(page, "We can't show it here — no GitHub App is configured on this box. Take this one as a description, not a demonstration.");
  await beat(page, PACE.read);
  await caption(page, "Without a GitHub App configured, Start stays disabled — and the screen tells you what to set up rather than faking a run.");
  await beat(page, PACE.read + 400);
  await spotlight(page, null);
});

// ---------------------------------------------------------------------------
// Act 3 — no identity, no credential (sts-fail-closed). The CREATE refusal IS
// the demo: a cloud_sts grant needs an attested identity, and on the embedded
// provider there is none, so the run is refused before any sandbox is built.
// Start stays ENABLED; pressing it renders demo-create-refused on the card, and
// the refusal earns the card's checkmark (refusalCompletes).
// ---------------------------------------------------------------------------
test("V03d act 3 — no identity, no credential", async () => {
  test.setTimeout(180_000);
  const page = stage();

  const card = await openDemo(page, "sts-fail-closed", "No identity, no credential");
  await caption(page, "The last kind is the one where nothing is handed out at all.");
  await beat(page, PACE.read);
  await spotlight(page, page.getByTestId("demo-policy-sts-fail-closed"));
  await caption(page, "It uses a cloud identity to request short-lived credentials from the cloud's token service — STS.");
  await beat(page, PACE.read);
  await caption(page, "So it only means anything when the cloud itself can vouch for this machine's identity — when something is attesting.");
  await beat(page, PACE.read);
  await walkPolicyKey(page, card, "sts-fail-closed", "kind",
    "kind: cloud STS — and the scope is empty, because there's no identity here to fill it.",
    "Which is the whole point of the last demo: with nothing attesting, the run is refused before it ever starts.");
  await spotlight(page, null);

  const start = card.getByTestId("demo-start-sts-fail-closed");
  await expect(start, "the sts demo's Start should stay ENABLED — the refusal IS the demo").toBeEnabled({
    timeout: 60_000,
  });
  await act(page, start, "Press Start, and the refusal doesn't wait for a mint.");

  // No sandbox — the 422 renders on the card itself (demo-create-refused), not
  // a toast, and marks the demo complete.
  const refused = card.getByTestId("demo-create-refused");
  await expect(refused, "the create refusal never rendered on the card — expected a 422 from run-create").toBeVisible({
    timeout: 60_000,
  });
  // The 422 means no run was ever created: record the ABSENCE for the grader.
  await noteDemoRun(page, "sts-fail-closed");
  await centerInFrame(refused);
  await spotlight(page, refused);
  await caption(page, "It fires the moment the run is created — look at the state: no sandbox was ever built.");
  await beat(page, PACE.read);
  await caption(page, "Read which gate refused — it's named right here on the card.");
  await beat(page, PACE.read);
  await caption(page, "That's what fail-closed buys: the credential was never reachable, not reached and then refused.");
  await beat(page, PACE.read + 400);
  await spotlight(page, null);
});

// ===========================================================================
// Conclusion — the ladder's payoff, then back to the core path (04). The recap
// line moved here verbatim from the 03 conclusion: the fifth kind lands HERE,
// so the core episode can no longer claim it.
// ===========================================================================
test("V03d conclusion", async () => {
  test.setTimeout(120_000);
  const page = stage();

  await chapter(page, "Back to the main path", "");
  await caption(page, "That's the last credential kind — five in all, each with its own boundary, because the protocol decides what safe use can even look like.");
  await beat(page, PACE.read);
  await caption(page, "One we tried from inside, one we could only explain, and one refused before it started — that refusal was the demonstration.");
  await beat(page, BEAT_SHORT);
  await caption(page, "Next: a workspace — something real for a run to work on.");
  await beat(page, BEAT_SHORT);
  await caption(page, "");
  await silentCard(page, "Next — 04: Add a workspace");
});
