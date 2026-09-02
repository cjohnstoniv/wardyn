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
 * finishes the ladder 03c started: header-injected -> piped -> RESIDENT ->
 * RE-ORIGINATED -> REFUSED.
 *
 * WHAT THIS FILMS — the three credential kinds a header cannot carry:
 *   ACT 1 ssh-briefly-resident — the documented exception, RE-ENACTED and
 *     narrated as one: git's SSH transport has no credential-helper seam, so the
 *     key is written 0400 and shredded. The real window opens and closes at
 *     startup before attach is allowed, so nobody can watch it live.
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
 * the close are [OWNER SLOT — drafted] in local/episode-03-mega-proposal.md;
 * local/episode-03-stanza-check.py (untracked) fails if a spec string and a stanza drift.
 *
 * STAGING THIS FILE OWNS (off camera): sweepStaleState(), then
 * wardyn-demo-ssh-key, because ssh-briefly-resident carries `needsSecret` and
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
  // ssh-briefly-resident carries `needsSecret`. Its masked entry is the same
  // lesson 03a teaches once on camera with wardyn-demo-key, so staging it here
  // off camera is what lets 03d re-take alone, in any order.
  await putSecret(page, SSH_SECRET, SSH_VALUE);
});

// ---------------------------------------------------------------------------
// Act 1 — the one that touches disk, briefly (ssh-briefly-resident). git's SSH
// transport has no credential-helper seam, so the key must become resident.
// The real window opens and closes at startup before attach, so this is an
// honest RE-ENACTMENT — narrated as one. Commands off the card's pills (the
// node re-mint carries a {grant_id}); the stored key is fake, so ssh is
// rejected — the load-bearing proof is the 0400 file appearing then gone. As the
// FIRST demo of its episode it walks the whole policy, not just the line that
// changed — a viewer who started here saw no baseline to diff against.
// ---------------------------------------------------------------------------
test("V03d act 1 — the one that touches disk", async () => {
  test.setTimeout(900_000);
  const page = stage();

  const card = await openEpisode(page, "ssh-briefly-resident", "The one that touches disk — briefly");

  await chapter(page, "The kinds that can't use a header", "Resident briefly, brokered, or refused");
  // [OWNER SLOT — drafted] The detour's own opener.
  await caption(page, "Three credential kinds that can't be injected as a header — and what Wardyn does instead.");
  await beat(page, PACE.read);
  await caption(page, "One we can replay here; one stays gated until you configure a GitHub App; one is refused before a sandbox exists — and that refusal is the lesson.");
  await beat(page, BEAT_SHORT);

  await spotlight(page, page.getByTestId("demo-policy-ssh-briefly-resident"));
  await caption(page, "One kind can't be kept out of the box at all — the ssh client reads its key from a file.");
  await beat(page, PACE.read);
  await caption(page, "So Wardyn narrows the window instead of pretending it isn't there: written just before the clone, removed right after.");
  await beat(page, PACE.read);
  await walkPolicyKey(page, card, "ssh-briefly-resident", "min_confinement_class",
    "Same barrier as the core — Fence.");
  await walkPolicyKey(page, card, "ssh-briefly-resident", "auto_stop_after_sec",
    "Same fifteen-minute idle stop.");
  await walkPolicyKey(page, card, "ssh-briefly-resident", "eligible_grants",
    "The eligible grants section is back; the kind is what's different.");
  await walkPolicyKey(page, card, "ssh-briefly-resident", "kind",
    "kind: SSH key — the one kind that is ever written to a file.",
    "No other kind is ever written down. This one is written owner-read-only — mode 0400, agent-owned — and removed the moment the clone is done.");
  await spotlight(page, null);

  const screen = await startAndBoot(page, card, "ssh-briefly-resident");

  // Step 0: ls ~/.ssh — already empty. The window closed at startup.
  await typeInTerminal(page, await pillCmd(card, 0), card);
  await beat(page, PACE.read);
  await caption(page, "This is a re-enactment, and narrated as one.");
  await beat(page, BEAT_SHORT);
  await caption(page, "On a real run the whole window opens and closes at startup, before you can attach.");
  await beat(page, PACE.read);
  await caption(page, "This listing is empty on purpose. At startup Wardyn issued a key, wrote it here, and removed it — before anyone could attach. You're looking at the after.");
  await beat(page, PACE.read);

  const auditRows = card.getByTestId("demo-audit-rows");
  await expect(auditRows, "no startup credential.mint row for the ssh key").toContainText("credential.mint", {
    timeout: 60_000,
  });
  await centerInFrame(auditRows);
  await spotlight(page, auditRows);
  await caption(page, "That startup mint is the window you could never have watched.");
  await beat(page, PACE.read);
  await caption(page, "From here, it's replayed by hand — the same local route, the same kind of file, in the same place.");
  await beat(page, PACE.read + 400);
  await spotlight(page, null);

  // Cockpit re-pin: the audit beat above centered the record and pushed the
  // policy off the top; put policy + terminal + audit back in one frame before typing.
  await frameRun(page, "ssh-briefly-resident");
  // Step 2 (pill index 1): the node re-mint → 0400 file.
  await typeInTerminal(page, await pillCmd(card, 1), card);
  await beat(page, PACE.read);
  await caption(page, "This kind of grant can be requested again — there's no cap on that.");
  await beat(page, PACE.read);
  await caption(page, "So the real bound isn't the window: anything running as the agent can ask for another key — and each ask costs it a row in the record.");
  await beat(page, BEAT_SHORT);

  // Step 3 (pill index 2): ls -l — the resident 0400 file.
  await typeInTerminal(page, await pillCmd(card, 2), card);
  await pollScreen(screen, /id_wardyn_demo/, "the re-minted key file never appeared");
  await caption(page, "There it is: read-only, agent-owned.");
  await beat(page, BEAT_SHORT);
  await caption(page, "For the length of a clone, a private key is a real file — and code running as the agent can read it. Wardyn doesn't claim otherwise.");
  await beat(page, PACE.read);

  // Step 4 (pill index 3): the ssh attempt — refused (fake key), narrow reach.
  await typeInTerminal(page, await pillCmd(card, 3), card);
  await beat(page, PACE.read + 800);
  await caption(page, "Now try the clone. The key is fake, so the host rejects it.");
  await beat(page, BEAT_SHORT);
  await caption(page, "But notice what the connection was even allowed to reach: one SSH host, on 443.");
  await beat(page, PACE.read);

  // Step 5 (pill index 4): shred → gone.
  await typeInTerminal(page, await pillCmd(card, 4), card);
  await pollScreen(screen, /\.ssh/, "the shred step never echoed the empty listing");
  await caption(page, "And gone — removed, with a cleanup that runs even if the clone dies halfway.");
  await beat(page, BEAT_SHORT);
  await caption(page, "And honestly: the removal isn't a row in the record. The window is bounded by the clone, not by an audit entry.");
  await beat(page, BEAT_SHORT);
  await caption(page, "This is the one exception to 'nothing enters the box', and it's documented.");
  await beat(page, PACE.read + 400);
  await caption(page, "What you actually get: nothing persists between clones, the network is one SSH host on 443, and every ask is a row.");
  await beat(page, BEAT_SHORT);
  await caption(page, "Abuse is detectable, not impossible.");
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
  await caption(page, "One we replayed, one we could only explain, and one refused before it started — that refusal was the demonstration.");
  await beat(page, BEAT_SHORT);
  await caption(page, "Next: a workspace — something real for a run to work on.");
  await beat(page, BEAT_SHORT);
  await caption(page, "");
  await silentCard(page, "Next — 04: Add a workspace");
});
