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
 * local/episode-03-stanza-check.py fails if a spec string and a stanza drift.
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
  await caption(page, "Three credential kinds that can't be injected as a header — and what the proxy does instead.");
  await beat(page, PACE.read);

  await spotlight(page, page.getByTestId("demo-policy-ssh-briefly-resident"));
  await caption(page, "One kind can't be kept out of the box at all — the ssh client reads its key from a file.");
  await beat(page, PACE.read);
  await caption(page, "So Wardyn narrows the window instead of pretending it isn't there: written just before the clone, shredded right after.");
  await beat(page, PACE.read);
  await walkPolicyKey(page, card, "ssh-briefly-resident", "min_confinement_class",
    "The barrier: CC1, Fence — the lightest sandbox tier, the wall between this run and your host.");
  await walkPolicyKey(page, card, "ssh-briefly-resident", "auto_stop_after_sec",
    "A dead-man's switch: if nothing else stops it, the run halts itself after fifteen minutes.");
  await walkPolicyKey(page, card, "ssh-briefly-resident", "eligible_grants",
    "A new section appears: eligible_grants. This is where a run is authorized to USE a secret.");
  await walkPolicyKey(page, card, "ssh-briefly-resident", "kind",
    "kind: ssh_key — the one exception that touches disk.",
    "Every other kind stays out of the box entirely; this one is written 0400 and shredded, on the narrowest window Wardyn can hold.");
  await spotlight(page, null);

  const screen = await startAndBoot(page, card, "ssh-briefly-resident");

  // Step 0: ls ~/.ssh — already empty. The window closed at startup.
  await typeInTerminal(page, await pillCmd(card, 0), card);
  await beat(page, PACE.read);
  await caption(page, "This is a re-enactment, and narrated as one.");
  await beat(page, BEAT_SHORT);
  await caption(page, "On a real run the whole window opens and closes at startup, before you can attach.");
  await beat(page, PACE.read);
  await caption(page, "The key was here. Startup minted it, wrote it, and shredded it. You're looking at the after.");
  await beat(page, PACE.read);

  const auditRows = card.getByTestId("demo-audit-rows");
  await expect(auditRows, "no startup credential.mint row for the ssh key").toContainText("credential.mint", {
    timeout: 60_000,
  });
  await centerInFrame(auditRows);
  await spotlight(page, auditRows);
  await caption(page, "That startup mint is the window you could never have watched.");
  await beat(page, PACE.read);
  await caption(page, "From here, it's replayed by hand — the same local route, the same file.");
  await beat(page, PACE.read + 400);
  await spotlight(page, null);

  // Step 2 (pill index 1): the node re-mint → 0400 file.
  await typeInTerminal(page, await pillCmd(card, 1), card);
  await beat(page, PACE.read);
  await caption(page, "This kind of grant can be requested again, so the same route can provide another temporary key.");
  await beat(page, PACE.read);

  // Step 3 (pill index 2): ls -l — the resident 0400 file.
  await typeInTerminal(page, await pillCmd(card, 2), card);
  await pollScreen(screen, /id_wardyn_demo/, "the re-minted key file never appeared");
  await caption(page, "There it is: read-only, agent-owned.");
  await beat(page, BEAT_SHORT);
  await caption(page, "For the length of a clone, and only then, a private key is a real file. Wardyn doesn't claim otherwise.");
  await beat(page, PACE.read);

  // Step 4 (pill index 3): the ssh attempt — refused (fake key), narrow reach.
  await typeInTerminal(page, await pillCmd(card, 3), card);
  await beat(page, PACE.read + 800);
  await caption(page, "The key is fake, so the host rejects it.");
  await beat(page, BEAT_SHORT);
  await caption(page, "But notice what the connection was even allowed to reach: one SSH host, on 443, and nothing else.");
  await beat(page, PACE.read);

  // Step 5 (pill index 4): shred → gone.
  await typeInTerminal(page, await pillCmd(card, 4), card);
  await pollScreen(screen, /\.ssh/, "the shred step never echoed the empty listing");
  await caption(page, "And gone.");
  await beat(page, BEAT_SHORT);
  await caption(page, "The exception is bounded, documented, and the only one there is.");
  await beat(page, PACE.read + 400);
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
  await caption(page, "One step further out. The PAT is delivered through Git's pipe; this token never enters the sandbox at all.");
  await beat(page, PACE.read);

  await spotlight(page, card.getByTestId("demo-policy-github-app-broker"));
  await caption(page, "git is pointed at a broker route on the proxy.");
  await beat(page, BEAT_SHORT);
  await caption(page, "It mints a short-lived, repo-scoped token from the live GitHub API and attaches it on its own outbound leg.");
  await beat(page, PACE.read);
  await caption(page, "Ask for it from inside, and you're refused by name.");
  await beat(page, PACE.read);
  await walkPolicyKey(page, card, "github-app-broker", "kind",
    "kind: github_token — scoped to a repo and a permission.",
    "One step past the PAT: this token never enters the box even as a pipe. It's attached on the proxy's outbound leg alone.");
  await spotlight(page, null);

  // The gate is the demo here: Start is disabled and the gate copy explains
  // why. On a stack with a GitHub App configured this would run instead; the
  // recording stack has none, so we film the honest closed door.
  await expect(
    card.getByTestId("demo-needs-github-app"),
    "the github-app gate copy never rendered — is a GitHub App configured on this stack?",
  ).toBeVisible({ timeout: 30_000 });
  await spotlight(page, card.getByTestId("demo-needs-github-app"));
  await caption(page, "And because that token comes from the live GitHub API, there's nothing to fake.");
  await beat(page, PACE.read);
  await caption(page, "Without a configured App, Start stays closed — and the card teaches the lane instead of pretending.");
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
  await caption(page, "The last rung is the one where nothing is handed out at all.");
  await beat(page, PACE.read);
  await spotlight(page, page.getByTestId("demo-policy-sts-fail-closed"));
  await caption(page, "The last kind uses a cloud identity to request short-lived credentials from STS.");
  await beat(page, PACE.read);
  await caption(page, "So it only means anything when something is actually attesting.");
  await beat(page, PACE.read);
  await walkPolicyKey(page, card, "sts-fail-closed", "kind",
    "kind: cloud_sts — and the scope is empty, because there's no identity here to fill it.",
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
  await centerInFrame(refused);
  await spotlight(page, refused);
  await caption(page, "It fires at run-create. No sandbox is ever built.");
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
  test.setTimeout(60_000);
  const page = stage();

  await chapter(page, "Back to the main path", "");
  await caption(page, "Five credential types, five different boundaries — because the protocol determines what safe use looks like.");
  await beat(page, PACE.read);
  await caption(page, "");
  await silentCard(page, "Next — 04: Add a workspace");
});
