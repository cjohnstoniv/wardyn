/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

/*
 * V02c — One command to a cluster. The BROWSER half (beats 5+): the terminal
 * half (scripts/demo-beats/02c-one-command-to-a-cluster.sh) built the cluster
 * and flipped it to SSO; this half signs in through Dex ON CAMERA, lands in
 * the FORCED Getting Started (the 0.7 gate: an un-onboarded install turns
 * every door into the funnel), walks the essentials — barrier, people,
 * network, secrets — finishes setup, and proves the unlock by running two
 * egress demos as real pods.
 *
 *     WARDYN_DEMO_SKIP_MODEL=1 WARDYN_DEMO_BASE_URL=http://localhost:8280 \
 *       scripts/record-demo.sh --video 02c \
 *       --terminal-script scripts/demo-beats/02c-one-command-to-a-cluster.sh
 *
 * STATE CONTRACT. This is a 02-class episode: the install is FRESH and
 * un-onboarded — that is the story — so there is deliberately NO
 * sweepStaleState() here and nothing marks the install onboarded off camera;
 * the on-camera "Finish setup" click is what does it, and the two demo pods
 * are the first runs the cluster ever schedules.
 *
 * MODEL ACCESS is named, honestly, as optional — nothing here connects one
 * (WARDYN_DEMO_SKIP_MODEL=1: this is not the operator's compose stack, and
 * both demos run plain curl in a Fence pod).
 */

import { test, expect } from "@playwright/test";
import { act, beat, caption, chapter, PACE, spotlight } from "./overlay";
import { stage } from "./stage";
import { advance } from "./funnel";
import { openDemo, openEpisode, pollScreen, startAndBoot } from "./demos";

test.skip(!process.env.WARDYN_DEMO, "demo recording — run via `make record-demo` (exports WARDYN_DEMO=1)");

test.describe.configure({ mode: "serial" });

const ADMIN_EMAIL = "admin@wardyn.local";
const ADMIN_PASSWORD = "password"; // deploy/kind/sso/README.md's demo literal

// ---------------------------------------------------------------------------
// Act 1 — sign in like a person, land where the install insists
// ---------------------------------------------------------------------------
test("V02c act 1 — sign in, and the install refuses to let you wander", async () => {
  test.setTimeout(180_000);
  const page = stage();
  await page.goto("/");
  await page.bringToFront();

  await chapter(page, "Now the console", "First sign-in, and a funnel that will not be skipped");

  // The token box is gone — this install trusts the identity provider now.
  await caption(page, "Sign in again. The sign-in page has changed — no token box; this install trusts your identity provider now.");
  await beat(page, PACE.read);
  await act(page, page.getByRole("link", { name: "Sign in with SSO" }).or(page.getByRole("button", { name: "Sign in with SSO" })).first(), "One click — the role map decides who is an admin.");

  // Dex's demo login form. Filmed, not hidden: this is the multi-user story.
  await page.locator('input[type="password"]').waitFor({ timeout: 30_000 });
  await caption(page, "This form is not Wardyn — look at the address: it is the identity provider's. It does the asking; Wardyn never sees this password.");
  await page.locator('input[type="text"], input[name="login"]').first().fill(ADMIN_EMAIL);
  await page.locator('input[type="password"]').fill(ADMIN_PASSWORD);
  await beat(page, PACE.read);
  await page.getByRole("button", { name: /log ?in/i }).click();

  // The 0.7 gate: an un-onboarded install force-lands EVERY access here.
  await page.waitForURL(/\/setup/, { timeout: 60_000 });
  // "Every door" is pinned by ui/e2e/setup-gate.spec.ts (route-by-route); the
  // film shows one honest forced landing — a second mid-take goto proved
  // flaky under recording load and taught nothing the suite doesn't.
  await caption(page, "And this is deliberate: until this install is set up, every door leads here.");
  await beat(page, PACE.read);
  await caption(page, "Not a wall — a checklist. Let's clear it.");
  await beat(page, PACE.read);
});

// ---------------------------------------------------------------------------
// Act 2 — the essentials: barrier, people, network, secrets
// ---------------------------------------------------------------------------
test("V02c act 2 — the essentials, on a cluster", async () => {
  test.setTimeout(420_000);
  const page = stage();

  // The welcome hero: a fresh browser meets the install's front door — with
  // this path's episode catalog (Shape C) in frame for one beat.
  await caption(page, "The front door: what this install is, and the episodes for exactly this path.");
  await beat(page, PACE.read);
  await act(page, page.getByRole("button", { name: /^Get started/ }), "Get started drops us into the funnel.");

  // Environment: the substrate is the cluster the terminal half just built.
  await expect(page.getByRole("heading", { name: "Pick your barrier" })).toBeVisible({ timeout: 60_000 });
  await spotlight(page, page.getByText(/Runner/i).first());
  await caption(page, "The runner is Kubernetes, so every run here is a pod — a small box the cluster schedules.");
  await beat(page, PACE.read);
  await caption(page, "And the row beneath it is the canary: the cluster's network policy is enforced, and this install checked.");
  await beat(page, PACE.read);
  await spotlight(page, null);
  await advance("Environment settled — next, who can sign in.");

  // People: multi-user + SSO, read off the live install.
  await expect(page.getByRole("heading", { name: "Who can sign in" })).toBeVisible({ timeout: 30_000 });
  await spotlight(page, page.getByText("Multi-user").first());
  await caption(page, "Multi-user, via SSO. Admins configure; members run inside the rails.");
  await beat(page, PACE.read);
  await spotlight(page, null);
  await caption(page, "Who may do what gets its own episode — for now, the map is set.");
  await beat(page, PACE.read);
  await advance("On to the network.");

  // Network: the egress posture — and the PROOF, on camera. The gate
  // (steps.ts corpNetworkGate) requires a real probe before Next unlocks;
  // on this cluster that probe is a pod launched just to try the network.
  await expect(page.getByRole("heading", { name: /^Network$/ }).first()).toBeVisible({ timeout: 30_000 });
  await caption(page, "Every sandbox inherits what this step learns about the network. So the install tests it for real instead of asking you.");
  await beat(page, PACE.read);
  await act(page, page.getByRole("button", { name: /^test connectivity$/i }), "The proof is a live probe — a sandbox launched just to try the network.");
  // advance()'s own Next-enabled wait (120s) is the probe wait: the gate
  // unlocks only when the probe reports.
  await expect(page.getByText(/reached/i).first()).toBeVisible({ timeout: 120_000 });
  await advance("It reports back — reached. Next only unlocks on a real answer. On to secrets.");

  // Secrets: lanes named; model access honestly optional.
  await expect(page.getByRole("heading", { name: /^Secrets$/ }).first()).toBeVisible({ timeout: 30_000 });
  await caption(page, "Keys, tokens, model credentials — however they arrive, they all land in the same governed store.");
  await beat(page, PACE.read);
  await caption(page, "A model is just one more secret, and entirely optional — Wardyn governs agentless runs the same way.");
  await beat(page, PACE.read);
});

// ---------------------------------------------------------------------------
// Act 3 — finish setup: the click that unlocks the install
// ---------------------------------------------------------------------------
test("V02c act 3 — finish setup, and the doors open", async () => {
  test.setTimeout(300_000);
  const page = stage();

  // SAME document as act 2 — the egress probe's proof is session state, and a
  // person doesn't reload mid-funnel. The rail is a map: jump to the last stop
  // (demo sub-steps and workspaces are their own episodes).
  await act(page, page.getByRole("button", { name: /^Review/ }), "The rail is a map — straight to the last stop.");
  await expect(page.getByRole("heading", { name: /review readiness/i })).toBeVisible({ timeout: 30_000 });
  await caption(page, "Review is the install's own readiness, grouped and honest.");
  await beat(page, PACE.read);

  await act(page, page.getByRole("button", { name: "Finish setup" }), "One click, and the install is marked set up — server-side, so the next browser lands past the funnel too.");
  await page.waitForURL((u) => !/\/setup/.test(u.pathname), { timeout: 30_000 }).catch(() => {});
  await page.goto("/");
  await expect(page).toHaveURL(/\/runs/, { timeout: 30_000 });
  await caption(page, "Same door as before. No funnel — the install remembers it is set up now.");
  await beat(page, PACE.read);
});

// ---------------------------------------------------------------------------
// Act 4 — proof: two egress demos, scheduled as pods
// ---------------------------------------------------------------------------
test("V02c act 4 — two demos, two pods, one boundary", async () => {
  test.setTimeout(900_000);
  const page = stage();

  await chapter(page, "Prove it", "Two sandboxes, one boundary");

  // Demo 1 — the sealed box: default-deny, no prompt, no wait.
  const sealed = await openEpisode(page, "sealed-box", "The sealed box");
  const screen1 = await startAndBoot(page, sealed, "sealed-box");
  await caption(page, "Same terminal as every other episode — underneath, this one is a pod on the cluster you watched build.");
  await beat(page, PACE.read);
  await screen1.click();
  await page.keyboard.type("curl -sSI https://example.com\n");
  await pollScreen(screen1, /403|CONNECT tunnel failed/i, "the sealed box refuses the tunnel");
  await caption(page, "Ask for an ordinary website, with an empty allowlist. Refused at the proxy — no prompt, no wait; this policy never asks.");
  await beat(page, PACE.read);
  await expect(sealed.getByTestId("demo-audit-panel")).toContainText(/example\.com/, { timeout: 30_000 });
  await spotlight(page, sealed.getByTestId("demo-audit-panel"));
  await caption(page, "And there is the record: the proxy's own row for the host it refused — policy denied.");
  await beat(page, PACE.read);
  await spotlight(page, null);

  // Demo 2 — denied, however you spell it: the dodge that does not work.
  const dodge = await openDemo(page, "denied-however-spelled", "Denied, however you spell it");
  const screen2 = await startAndBoot(page, dodge, "denied-however-spelled");
  await screen2.click();
  await page.keyboard.type("curl -sSI https://example.org\n");
  await pollScreen(screen2, /HTTP\/2 200|HTTP\/1\.1 200/i, "allow-all reaches the public internet");
  await caption(page, "Egress wide open — except the one host this policy denies.");
  await beat(page, PACE.read);
  await page.keyboard.type("curl -sSI --max-time 5 https://example.com\n");
  await pollScreen(screen2, /403|CONNECT tunnel failed/i, "the plain spelling is denied");
  await caption(page, "The one denied host: refused, even though everything else is allowed.");
  await beat(page, PACE.read);
  await page.keyboard.type("curl -sSI --max-time 5 https://example.com.\n");
  await pollScreen(screen2, /403|CONNECT tunnel failed/i, "the trailing-dot spelling meets the same 403");
  await caption(page, "Now the dodge: a trailing dot is a legal spelling of the same host — it used to slip past naive deny-lists.");
  await beat(page, PACE.read);
  await caption(page, "Refused identically. Wardyn checks the real name, not the spelling you typed.");
  await beat(page, PACE.read);
  await expect(dodge.getByTestId("demo-audit-panel")).toContainText(/example\.com/, { timeout: 30_000 });
  await spotlight(page, dodge.getByTestId("demo-audit-panel"));
  await caption(page, "And the record names the reason: policy denied — and it names the real host, not the spelling we typed.");
  await beat(page, PACE.read);
  await spotlight(page, null);

  await caption(page, "Install. Identity. A funnel that will not be skipped. And a boundary that holds.");
  await beat(page, PACE.read);
  await caption(page, "Next on the core path: what the boundary actually stops.");
  await beat(page, PACE.read);
  await caption(page, "Who may do what, a member's own workspace, and admin operations each get their own episode later.");
  await beat(page, PACE.read);
});
