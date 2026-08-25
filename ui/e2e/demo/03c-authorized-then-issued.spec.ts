/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

/*
 * Episode 03c of the series — "Authorized, then issued". OPTIONAL DETOUR.
 *
 * WHERE IT SITS. A detour off 03a (the core "What it stops"), watched after it,
 * handing straight back to 04 — nothing in the core path depends on it. Driven
 * by `scripts/record-demo.sh --video 03c`, which globs this exact filename.
 * 03a taught the two secrets basics (you can't read a secret back; a run can use
 * one it never holds); this episode and 03d are the ladder underneath that:
 * header-injected -> piped -> resident -> re-originated -> refused.
 *
 * WHAT THIS FILMS — two credential kinds (api_key twice, then git_pat): the
 * shapes that CAN travel as a header or a pipe, each driven live:
 *   ACT 1 authorized-not-issued — approval-gated, single-use: the first mint is
 *     refused pending review, the approved one returns an injection RULE with no
 *     value in it, the third is refused because it already spent itself.
 *   ACT 2 rest-api-token — the same law wired the way you'd actually write it:
 *     a real Authorization: Bearer header, stitched on at the boundary.
 *   ACT 3 pat-stdout-only — a PAT minted into git's pipe behind a caller-auth
 *     gate, refusal first, because git-over-HTTPS has no header to inject.
 *
 * PROVENANCE. Split from 03-what-it-stops.spec.ts 2026-08-24 (acts 7/8/9),
 * lines moved verbatim — including the "The same law, five ways" chapter card,
 * which moved from act 8 to open the file. The opener and the close are
 * [OWNER SLOT — drafted] in local/episode-03-mega-proposal.md;
 * local/episode-03-stanza-check.py fails if a spec string and a stanza drift.
 *
 * STAGING THIS FILE OWNS (off camera): sweepStaleState(), then wardyn-demo-key,
 * wardyn-demo-api-token and wardyn-demo-pat, because all three of this
 * episode's cards carry `needsSecret` and stepOrder(status) DROPS a demo whose
 * secret is missing — a `?step=` to a dropped card silently re-corrects to a
 * neighbour and films the wrong thing. wardyn-demo-key's MASKED ENTRY is taught
 * once on camera in 03a; staging it here off camera is the same lesson, not a
 * hidden one — and it is what lets this episode re-take alone, in any order.
 *
 * This is NOT a test. It asserts only enough to stay honest and to know when to
 * advance; a failure here means the recording is wrong, not the product.
 */

import { test, expect } from "@playwright/test";
import { act, beat, caption, centerInFrame, chapter, PACE, spotlight, typeInTerminal } from "./overlay";
// Importing stage.ts is what registers this file's own recorded context.
import { stage } from "./stage";
import { decide } from "./funnel";
import { sweepStaleState } from "./sweep";
import {
  API_TOKEN,
  API_TOKEN_VALUE,
  BEAT_SHORT,
  DEMO_KEY,
  DEMO_KEY_VALUE,
  openDemo,
  openEpisode,
  PAT_SECRET,
  PAT_VALUE,
  pillCmd,
  pollScreen,
  putSecret,
  silentCard,
  startAndBoot,
  walkPolicyKey,
  frameRun,
} from "./demos";

test.skip(!process.env.WARDYN_DEMO, "demo recording — run via `make record-demo` (exports WARDYN_DEMO=1)");

test.describe.configure({ mode: "serial" });


test.beforeAll(async () => {
  if (!process.env.WARDYN_DEMO) return;
  const page = stage();
  // S6: deny stale pending approvals / kill stale runs first — otherwise the
  // Approvals badge carries a prior take's number through the whole video.
  await sweepStaleState();
  // All three cards carry `needsSecret`. wardyn-demo-key's masked entry is
  // taught ON CAMERA in 03a; this episode never films a write, so it stages the
  // key off camera like the other two — which is what lets 03c re-take alone.
  await putSecret(page, DEMO_KEY, DEMO_KEY_VALUE);
  await putSecret(page, API_TOKEN, API_TOKEN_VALUE);
  await putSecret(page, PAT_SECRET, PAT_VALUE);
});

// ---------------------------------------------------------------------------
// Act 1 — authorized, not issued (authorized-not-issued). Lifted verbatim from
// old-07's beat 8: an approval-gated, single-use mint that returns a RULE, not
// a value. The mint command comes off the card's own copy pill (its {grant_id}
// is substituted from the live run). As the FIRST demo of its episode it walks
// the whole eligible_grants block, not just the two lines that changed — a
// viewer who started here saw no baseline to diff against.
// ---------------------------------------------------------------------------
test("V03c act 1 — authorized, not issued", async () => {
  test.setTimeout(900_000);
  const page = stage();

  const card = await openEpisode(page, "authorized-not-issued", "Authorized, not issued");

  await chapter(page, "The same law, five ways", "How a credential is kept depends on the protocol");
  // [OWNER SLOT — drafted] The detour's own opener — the card above moved here
  // from the rest-api-token act, so the ladder is named before its first rung.
  await caption(page, "You've seen a key ride in a header the run never held. Here that grant gets an approval gate and a time limit, then a real third-party call — and then a second way in entirely: a Git token issued straight into a pipe. The three kinds that can use neither are the next detour.");
  await beat(page, PACE.read);

  await caption(page, "Same permission, one field different.");
  await beat(page, BEAT_SHORT);
  await caption(page, "This one requires approval before that one-time use can be authorized.");
  await beat(page, PACE.read);
  await walkPolicyKey(page, card, "authorized-not-issued", "eligible_grants",
    "The same eligible grants section as the core.");
  await walkPolicyKey(page, card, "authorized-not-issued", "kind",
    "Same kind — an API key, in a header.");
  await walkPolicyKey(page, card, "authorized-not-issued", "scope",
    "Same scope — that host, that header, and the secret's name; never its value.");
  await walkPolicyKey(page, card, "authorized-not-issued", "requires_approval",
    "Two lines changed in the grant. Requires approval is now true — a human decides at the moment it's minted, that is, issued. And one yes is one mint: single-use comes with the gate, not from a third line.");
  await walkPolicyKey(page, card, "authorized-not-issued", "ttl_seconds",
    "And a time to live: once minted, the proxy's injection rule lasts five minutes, then expires on its own. Spent means one mint — a third ask is refused as already minted — and the time limit caps how long that one mint stays usable.");
  await spotlight(page, null);
  await caption(page,
    "And one difference from the core, up front: this grant is asked for. The core's box never knew a secret was involved; this one must request the mint.");
  await beat(page, PACE.read);

  const screen = await startAndBoot(page, card, "authorized-not-issued");
  const mintCmd = await pillCmd(card, 0);

  await caption(page, "The sandbox asks to use the credential.");
  await beat(page, PACE.read);
  await typeInTerminal(page, mintCmd, card);
  await pollScreen(screen, /pending/, 'the first mint never came back pending — expected 409 {"code":"pending"}');
  await caption(page, "Refused — but an approval is now waiting.");
  await beat(page, BEAT_SHORT);
  await caption(page, "That ask didn't fail. It raised a decision.");
  await beat(page, PACE.read);

  await caption(page, "And here it is, right under the terminal we're watching.");
  await beat(page, PACE.read);
  await decide(card, "Approve", "Approve it.", "example.com");

  await typeInTerminal(page, mintCmd, card);
  await pollScreen(screen, /X-Wardyn-Demo/, "the approved mint never returned an injection rule");
  const mintText = await screen.innerText();
  expect(
    mintText.includes(DEMO_KEY_VALUE),
    "the approved mint returned the secret VALUE — the demo's entire claim is that it never does",
  ).toBe(false);
  await caption(page, "Now the same ask is authorized.");
  await beat(page, BEAT_SHORT);
  await caption(page, "And look at what came back.");
  await beat(page, BEAT_SHORT);
  await caption(
    page,
    "A host, a header, and the name of a secret — the information the proxy needs to inject it at the boundary.",
  );
  await beat(page, PACE.read);
  await caption(page, "An instruction for the proxy — not a credential for the box.");
  await beat(page, PACE.read);
  await caption(page, "Authorized, not handed to the sandbox.");
  await beat(page, BEAT_SHORT + 400);

  await typeInTerminal(page, mintCmd, card);
  await pollScreen(screen, /already_minted/, "the third mint was not refused — this grant is single-use");
  await caption(page, "Ask again, and it's spent.");
  await beat(page, BEAT_SHORT);
  await caption(page, "One approval, one use.");
  await beat(page, BEAT_SHORT + 400);

  const auditRows = card.getByTestId("demo-audit-rows");
  await expect(auditRows, "the approved mint never landed as an allow in the demo's audit panel").toContainText(
    "allow",
    { timeout: 60_000 },
  );
  await centerInFrame(auditRows);
  await spotlight(page, auditRows);
  await caption(page, "Three asks, three audit rows: refused pending approval, authorized once, then spent.");
  await beat(page, PACE.read);
  await caption(page, "The first row is the approval gate. The last is the single-use boundary working.");
  await beat(page, PACE.read + 400);
  await spotlight(page, null);
  await act(page, card.getByRole("button", { name: "End demo" }));
});

// ---------------------------------------------------------------------------
// Act 2 — a bearer token for a real API (rest-api-token). Keyless-provable,
// driven live: the realistic Authorization: Bearer shape. Same law as
// key-never, wired the way you'd actually write it; audit is the proof.
// ---------------------------------------------------------------------------
test("V03c act 2 — a bearer token for a real API", async () => {
  test.setTimeout(900_000);
  const page = stage();

  await caption(page, "That was the mechanism stripped to its bones — a made-up header on a made-up host.");
  await beat(page, PACE.read);
  await caption(page, "Here it is the way you'd actually write it — against a service the proxy can read.");
  await beat(page, BEAT_SHORT);

  const card = await openDemo(page, "rest-api-token", "A bearer token for a real API");
  await spotlight(page, page.getByTestId("demo-policy-rest-api-token"));
  await caption(page, "A plain REST call carrying the header real services expect — an internal API, a metrics endpoint, plain web traffic inside your network.");
  await beat(page, PACE.read);
  await caption(page, "It carries Authorization: Bearer, where the token is a Wardyn secret the box never holds.");
  await beat(page, PACE.read);
  await walkPolicyKey(page, card, "rest-api-token", "scope",
    "The scope changed: a real host, the Authorization header, and Bearer formatting.");
  await spotlight(page, null);

  const screen = await startAndBoot(page, card, "rest-api-token");

  const auditRows = card.getByTestId("demo-audit-rows");
  await expect(auditRows, "no credential.mint row — the api_key grant never minted (member clamp, or secret missing)")
    .toContainText("credential.mint", { timeout: 60_000 });
  await expect(auditRows).toContainText("secret.read");
  await centerInFrame(auditRows);
  await spotlight(page, auditRows);
  await caption(page, "The record shows the secret being used outside the box before you type.");
  await beat(page, PACE.read + 400);
  await spotlight(page, null);

  // Cockpit re-pin: the audit beat above centered the record and pushed the
  // policy off the top; put policy + terminal + audit back in one frame before typing.
  await frameRun(page, "rest-api-token");
  await typeInTerminal(page, await pillCmd(card, 0), card);
  await pollScreen(screen, /HTTP\/[\d.]+ \d\d\d/, "the third-party call never got a response line");
  await caption(page, "The request left this box without an Authorization header; the proxy stitched one on at the boundary, after the sandbox had already sent it.");
  await beat(page, PACE.read);
  // [OWNER SLOT — drafted]
  await caption(page, "It can do that because this request is plain, unencrypted web traffic — the proxy reads it and edits it. Encrypted traffic Wardyn opens for two kinds of host only: the model providers, and a corporate mirror you configured with its own token. Every other encrypted connection is a tunnel the proxy can't read. An encrypted third-party API — a Stripe, a Slack — is in that last set: there's no header to edit. Which is exactly why the next credential doesn't use a header at all.");
  await beat(page, PACE.read);

  await typeInTerminal(page, await pillCmd(card, 1), card);
  await pollScreen(screen, /WARDYN_PROXY_URL=/, "printenv never echoed inside the demo sandbox");
  const envText = await screen.innerText();
  expect(envText.includes(API_TOKEN_VALUE), "the API token's VALUE printed inside the sandbox").toBe(false);
  await caption(page, "No token in the environment — no variable to end up in a log line or a crash dump.");
  await beat(page, PACE.read);

  await typeInTerminal(page, await pillCmd(card, 2), card);
  await beat(page, PACE.read);
  await caption(page, "Nothing resident on disk either — not the value, not the config that names it.");
  await beat(page, PACE.read);
  await act(page, card.getByRole("button", { name: "End demo" }));
});

// ---------------------------------------------------------------------------
// Act 3 — a PAT that only ever exists in a pipe (pat-stdout-only). Keyless-
// provable, driven live, REFUSAL-FIRST: git-over-HTTPS is an opaque tunnel with
// no header to inject, so the PAT is minted into git's pipe behind a caller-
// auth gate. Commands come off the card's pills (the refuse/success pair is the
// same command with the gate token exported in between).
// ---------------------------------------------------------------------------
test("V03c act 3 — a PAT that only ever exists in a pipe", async () => {
  test.setTimeout(900_000);
  const page = stage();

  const card = await openDemo(page, "pat-stdout-only", "A PAT that only ever exists in a pipe");
  await spotlight(page, page.getByTestId("demo-policy-pat-stdout-only"));
  await caption(page, "A Git clone is different: the proxy can't inspect the credential exchange inside the Git connection.");
  await beat(page, PACE.read);
  await caption(page, "So the personal access token — the PAT — is issued only when Git asks: straight into Git's pipe, and nowhere else.");
  await beat(page, PACE.read);
  await caption(page, "And first, Git's credential helper makes the caller prove who it is.");
  await beat(page, BEAT_SHORT + 400);
  await walkPolicyKey(page, card, "pat-stdout-only", "kind",
    "The kind is different now: git PAT, not API key.",
    "No header to inject — Git's exchange is opaque — so the token goes straight into Git's pipe instead.");
  await spotlight(page, null);

  const screen = await startAndBoot(page, card, "pat-stdout-only");

  // Step 0: the configured helper call WITHOUT the gate token — refused.
  await caption(page, "Watch it refuse.");
  await beat(page, BEAT_SHORT);
  await typeInTerminal(page, await pillCmd(card, 0), card);
  await pollScreen(
    screen,
    /WARDYN_GIT_HELPER_SECRET|refusing/,
    "the ungated helper call did not refuse — expected 'caller did not present WARDYN_GIT_HELPER_SECRET'",
  );
  await caption(page, "The attach shell never inherited the per-run secret, so nothing is emitted.");
  await beat(page, PACE.read);
  await caption(page, "That's the gate deciding — not an error.");
  await beat(page, BEAT_SHORT);
  // [OWNER SLOT — drafted]
  await caption(page, "The gate stops a caller that doesn't hold the run's own secret — it isn't a boundary against the run; the run is who the grant is for. Anything in the box that can read that secret can mint, as often as it likes. What bounds it is the token's own repo scope — and a row in the record for every mint.");
  await beat(page, PACE.read);

  // Step 1: present the gate token.
  await typeInTerminal(page, await pillCmd(card, 1), card);
  await beat(page, PACE.read);
  await caption(page, "Present the secret, and ask again.");
  await beat(page, BEAT_SHORT);

  // Step 2: same call, now the PAT comes back on stdout.
  await typeInTerminal(page, await pillCmd(card, 2), card);
  await pollScreen(screen, /password=/, "the gated helper call never emitted the PAT on stdout");
  await caption(page, "Now the credential helper returns Git's credential lines on standard output — the only place Wardyn ever puts them.");
  await beat(page, PACE.read);
  await caption(page, "In a real clone git reads them straight off this pipe, and they're gone.");
  await beat(page, PACE.read);

  // Step 3: printenv — the gate token is here, the PAT is not. Scope the check
  // to the printenv output ALONE: step 2 legitimately printed the PAT on stdout
  // (the pipe IS the point), and that line is still in the xterm scrollback —
  // innerText() over the whole buffer would see it. Slice from the printenv
  // command echo onward so "not in the environment" means exactly that.
  const printenvCmd = await pillCmd(card, 3);
  await typeInTerminal(page, printenvCmd, card);
  await pollScreen(screen, /WARDYN_GIT_HELPER_SECRET=/, "printenv never echoed the gate token");
  const fullText = await screen.innerText();
  const envIdx = fullText.lastIndexOf(printenvCmd);
  const envText = envIdx >= 0 ? fullText.slice(envIdx + printenvCmd.length) : fullText;
  expect(envText.includes(PAT_VALUE), "the PAT's VALUE printed in the environment").toBe(false);
  await caption(page, "The PAT isn't here.");
  await beat(page, BEAT_SHORT);
  await caption(page, "The gate token you exported is — it lets you ask; it isn't the credential.");
  await beat(page, PACE.read);

  // Step 4: grep — nothing on disk.
  await typeInTerminal(page, await pillCmd(card, 4), card);
  await beat(page, PACE.read);
  await caption(page, "Nothing on disk. The mint went to a pipe; no file was ever written.");
  await beat(page, PACE.read);
  await caption(page, "And the mint was stamped when you asked — not at startup. That's the difference from an injected key.");
  await beat(page, PACE.read + 400);
  await act(page, card.getByRole("button", { name: "End demo" }));
});

// ===========================================================================
// Conclusion — the detour closes and hands back to the core path (04).
// ===========================================================================
test("V03c conclusion", async () => {
  test.setTimeout(120_000);
  const page = stage();

  await chapter(page, "Back to the main path", "");
  // [OWNER SLOT — drafted]
  await caption(page, "Two ways — a header the run never sees, and a pipe it reads once. Three kinds can use neither; that's the next detour, if you want it. Otherwise the main path picks up at episode four: a workspace.");
  await beat(page, PACE.read);
  await caption(page, "");
  await silentCard(page, "Next — 04: Add a workspace");
});
