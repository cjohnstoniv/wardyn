/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

/*
 * THE LIVE AWS SSO WALK — two real principals, a real cluster, no AWS tenant.
 *
 * Driven by scripts/kind-sso-walk.sh, which owns the four preconditions this
 * file assumes (read its header; the fourth one — site-config `internal_hosts`
 * seeded with the fake's SERVICE host and the Service CIDR — is the one that
 * fails first if forgotten, and it fails as a deny inside a sandbox that looks
 * exactly like the fake being down).
 *
 * What it proves that nothing hermetic can:
 *
 *   - a MEMBER, signed in through Dex on a k8s install, can complete the
 *     containerized AWS SSO login FROM THEIR OWN SEAT and reach the terminal;
 *   - the credential that login captured is THEIRS: `/setup/status` reads
 *     model_access `live` for the member and `not_configured` for the admin on
 *     the same install, at the same moment;
 *   - the identity real botocore asked the portal to mint is the MEMBER's
 *     PINNED account/role — read from the fake's own `/_seen`, which is the one
 *     observation in this file that is not Wardyn asserting about itself;
 *   - something actually SPENT it: the bedrock-runtime stub was hit.
 *
 * Self-skips without WARDYN_TEST_K8S=1, so a bare `pnpm e2e` can never point a
 * browser at somebody's live cluster.
 *
 * ── NO TESTS ARE SKIPPED HERE (W5) ──────────────────────────────────────────
 * Two of these cases were written in W0-P against the PLAN's stated end state
 * and marked `test.fixme("<lane>")` while their lanes were unmerged. Both lanes
 * (`sso-pin-dispatch`, `member-mode`) are on feat/v0.7.4, so both are live
 * tests. Where a case quoted a DRAFT sentence the lane then shipped
 * differently, the assertion was re-pointed at the MERGED constant and says so
 * in place — never loosened to match both spellings.
 *
 * ── 0.7.5: THIS FILE IS HALF THE WALK ───────────────────────────────────────
 * The walk now runs `sso-member sso-member-recovery` in ONE invocation, against
 * ONE cluster (scripts/kind-sso-walk.sh). The shared inputs, the two Dex
 * sessions and the read/write helpers moved to ui/e2e/live/helpers.ts so both
 * files use the same ones; this file's own order and assertions are unchanged
 * apart from the two 0.7.5 edits marked in place. THIS FILE RUNS FIRST and
 * leaves the member `live` under the CONTRADICTING pair — the recovery file
 * depends on both facts and says so in its header.
 */

import { expect, test } from "@playwright/test";
import { MEMBER_MODE } from "../../src/app/components/wardyn/member-mode-banner";
import { MEMBER_GETTING_STARTED, YOUR_MODEL_KEY } from "../../src/app/components/wardyn/copy";
// 0.7.6 lanes ui-model-access-door and ui-new-run-model-access, handed over by
// constant name in local/v076/canon/*-docs.md. Both modules are plain constant
// tables with no CSS import — the rule ui/e2e/live/helpers.ts states for
// SELFRUN_MARKER, and what keeps `playwright test --project=live --list` green.
import { MODEL_ACCESS_BANNER, RAIL_MODEL_ACCESS } from "../../src/app/components/wardyn/model-access-copy";
import { AGENTS } from "../../src/app/lib/workspace-providers-copy";
import {
  ADMIN_EMAIL,
  ADMIN_TOKEN,
  MEMBER_EMAIL,
  PIN_ACCOUNT,
  PIN_ROLE,
  dexSignIn,
  dexSignOut,
  launchAgentRun,
  me,
  modelAccess,
  openLoginPane,
  putRoster,
  seen,
  signInThroughPane,
} from "./helpers";

test.skip(process.env.WARDYN_TEST_K8S !== "1", "live cluster walk: set WARDYN_TEST_K8S=1 (scripts/kind-sso-walk.sh)");
test.describe.configure({ mode: "serial" });

// ── the walk ────────────────────────────────────────────────────────────────

test("the admin declares the per-user Bedrock SSO lane and pins the account", async ({ page, request }) => {
  expect(ADMIN_TOKEN, "WARDYN_LIVE_ADMIN_TOKEN is unset — run this through scripts/kind-sso-walk.sh").not.toBe("");

  await dexSignIn(page, ADMIN_EMAIL);
  const who = await me(page);
  expect(who.email, "the session Dex handed back is not the admin's").toBe(ADMIN_EMAIL);
  expect(who.operator, "admin@wardyn.local must resolve as an operator (WARDYN_OIDC_ROLE_MAP)").toBe(true);

  await putRoster(request);

  // The admin declared the lane; declaring it signs NOBODY in, the admin
  // included. This is the assertion the whole per_user design rests on.
  const adminAccess = await modelAccess(page);
  expect(adminAccess.state, "the admin who declared the lane must not inherit a credential").toBe("not_configured");
  await dexSignOut(page);
});

// ── I — the strip, in the ONE window a member has never signed in ───────────

test("I (model-access-banner): a never-signed-in member is told on every screen, not only on Getting Started", async ({
  page,
}) => {
  // Finding 2, live, and THIS IS THE ONLY SEAT ON THE WALK THAT CAN PROVE IT.
  //
  // The plan hands case I to the recovery file, and the interactive half of it
  // (open the door from the strip, sign in, the strip clears without a reload)
  // lives there. The `not_configured` half cannot: the whole of that file runs
  // AFTER the member's first capture, and nothing in the product deletes a
  // member's stored session — makeMemberActionable() reaches `expired_signin`
  // by contradicting the pin, which is a DIFFERENT sentence
  // (MODEL_ACCESS_BANNER.EXPIRED). DELETE /setup/harness-credential is
  // operator-only and scoped to the CALLER's own subject (harnesscred.go's
  // handleHarnessDisconnect), so not even the walk's admin token can put the
  // member back. So the first-run strip is asserted HERE, in the four seconds
  // between the admin declaring the lane and the member signing in, and this
  // case must stay BEFORE the capture and must not make one.
  //
  // It is read-only for that reason: the door is opened and DISMISSED. Under
  // startURLManaged the pane opens in phase `intro` and only "Start login"
  // sends POST /setup/harness-login (see the recovery file's case F), so
  // nothing is launched and the member is still `not_configured` for the case
  // below.
  await dexSignIn(page, MEMBER_EMAIL);
  expect((await modelAccess(page)).state, "case I must run before the member's first capture").toBe("not_configured");

  // (1) THE RUNS BOARD — a screen that has never mentioned model access. The
  // strip is a LAZY chunk behind a Suspense fallback of null (app-shell.tsx),
  // and the door says nothing at all until /me has resolved the viewer, so this
  // is awaited rather than read on the first frame.
  await page.goto("/runs");
  await expect(page.getByText(MODEL_ACCESS_BANNER.NOT_SIGNED_IN)).toBeVisible({ timeout: 60_000 });
  // EXACT, and that is the finding-1 negative in miniature: the rail's own
  // control carries the same LABEL under a different accessible name
  // (RAIL_MODEL_ACCESS.SIGN_IN_ARIA), and Playwright's default name match is a
  // substring — so a loose locator here would pass for the wrong control on the
  // New Run screen below.
  await expect(page.getByRole("button", { name: AGENTS.SIGN_IN_AWS, exact: true })).toBeVisible();
  // The first-run state is the one a person may set aside (it is not an error);
  // never CLICKED here — the dismissal is per browser context and per subject,
  // and swallowing it would make every later assertion in this case vacuous.
  await expect(page.getByRole("button", { name: MODEL_ACCESS_BANNER.NOT_NOW })).toBeVisible();

  // (2) IT IS THE SHELL'S BAND, NOT A SCREEN'S. Navigated client-side (what a
  // <NavLink> click does) rather than with a second full load: a full load
  // re-mounts the whole console and would prove only that the strip renders
  // twice, not that it rides the shell across a navigation.
  await page.evaluate(() => {
    window.history.pushState({}, "", "/workspaces");
    window.dispatchEvent(new PopStateEvent("popstate"));
  });
  await expect(page).toHaveURL(/\/workspaces$/);
  await expect(page.getByText(MODEL_ACCESS_BANNER.NOT_SIGNED_IN)).toBeVisible({ timeout: 60_000 });

  // (3) THE DOOR OPENS IN PLACE — the sign-in itself, on the screen they were
  // on, with no navigation.
  await page.getByRole("button", { name: AGENTS.SIGN_IN_AWS, exact: true }).click();
  await expect(page.getByRole("heading", { name: MODEL_ACCESS_BANNER.DIALOG_TITLE })).toBeVisible({ timeout: 60_000 });
  await expect(page.getByTestId("harness-login-pane")).toBeVisible({ timeout: 60_000 });
  await expect(page).toHaveURL(/\/workspaces$/);
  // …and out again, WITHOUT launching. Escape routes through the pane's own
  // handle (model-access-banner.tsx's onOpenChange), which is a no-op on a pane
  // that never got a run id.
  await page.keyboard.press("Escape");
  await expect(page.getByTestId("harness-login-pane")).toHaveCount(0);

  // (4) NEW RUN — finding 1, in the state the rail was silent for. The rail
  // CLAIMS the door while it renders its own control, so the strip keeps its
  // sentence and drops its button: two controls named the same thing on one
  // screen is the defect U-13 already fixed once.
  await page.goto("/runs/new");
  await page.getByRole("radio", { name: /^Autonomous/ }).click();
  await expect(page.getByText(RAIL_MODEL_ACCESS.NOT_SIGNED_IN)).toBeVisible({ timeout: 60_000 });
  await expect(page.getByRole("button", { name: RAIL_MODEL_ACCESS.SIGN_IN_ARIA })).toBeVisible();
  await expect(page.getByRole("button", { name: AGENTS.SIGN_IN_AWS, exact: true })).toHaveCount(0);
  // THE FINDING-1 NEGATIVE IS *NOT* ASSERTED HERE, AND THAT IS A REPORTED
  // FINDING, NOT AN OMISSION (lane e2e-sso-path, W3 REPORT — for the coordinator
  // and the W6 copy lens).
  //
  // The plan asserts `RAIL_MODEL_ACCESS.NO_PROVIDER` has count 0 in this state,
  // on the premise that "`llm_ready` becomes true the moment the ADMIN saves the
  // roster row, before any member has signed in, so showModelWarning is false
  // for every member". THE CLUSTER SAYS OTHERWISE, and the source agrees:
  // `setupBedrock` reads the AWS SSO blob through the caller's OWN per-user
  // scope (`readAWSSSOBlob(ctx, sso)`), so for a member who has never signed in
  // there is no blob, `SSOPresent` is false, `Ready` is false, `llmDetail` is
  // "", `computeLLMReady` is false — and `showModelWarning = isAgent &&
  // llmReady === false` is TRUE. On 0.7.6 this member therefore reads BOTH
  // sentences, stacked: the new true one (asserted above) and the old
  // deployment-level one, which on this deployment is false.
  //
  // Neither assertion is honest yet: `toHaveCount(0)` reds a walk over a
  // pre-existing sentence no 0.7.6 lane changed, and `toHaveCount(1)` would pin
  // the contradiction as intended. So the case asserts the FIX and leaves the
  // ruling to the coordinator; the negative IS asserted in
  // sso-member-recovery.spec.ts's A(rail)+, where the member has a capture and
  // the plan's premise holds.

  // (5) THE ONE SUPPRESSION A MEMBER GETS, and the one they deliberately do
  // NOT. Getting Started IS the door, so the strip is withheld there — the card
  // below is what says it instead.
  await page.goto("/setup");
  await expect(page.getByText(YOUR_MODEL_KEY.NOT_SIGNED_IN_CHIP).first()).toBeVisible({ timeout: 60_000 });
  await expect(page.getByText(MODEL_ACCESS_BANNER.NOT_SIGNED_IN)).toHaveCount(0);
  // …and on /settings a MEMBER keeps it, deliberately: that card's AWS button
  // is admin-only, so hiding the strip there would strand exactly the person a
  // refusal sends to the page (the suppression is operator-only —
  // model-access-banner.tsx's `suppressed`).
  await page.goto("/settings");
  await expect(page.getByText(MODEL_ACCESS_BANNER.NOT_SIGNED_IN)).toBeVisible({ timeout: 60_000 });
  await dexSignOut(page);
});

test("the member signs in to AWS from their own seat and the capture is theirs", async ({ page }) => {
  await dexSignIn(page, MEMBER_EMAIL);
  const who = await me(page);
  expect(who.email, "the session Dex handed back is not the member's").toBe(MEMBER_EMAIL);
  expect(who.operator).toBe(false);

  // Before: the member has a real action to take, not a dead end.
  const before = await modelAccess(page);
  expect(before.state).toBe("not_configured");
  expect(before.action).toBe("Sign in to AWS");

  // The member's own Getting Started carries the CTA (P3 / lane
  // member-cold-load: the member's page must not call an admin-only endpoint).
  //
  // 0.7.5 BUILD 0(a), lane ui-member-model-key (merged): the "Your model key"
  // card no longer tells a per_user member their model access is already done.
  // This is the one live seat that can read the NOT-SIGNED-IN half — it needs a
  // real second identity under a real per_user roster row, which the lane's own
  // vitest matrix cannot produce. Asserted THROUGH the merged constants.
  await page.goto("/setup");
  await expect(page.getByText(YOUR_MODEL_KEY.NOT_SIGNED_IN_CHIP).first()).toBeVisible({ timeout: 60_000 });
  await expect(page.getByText(YOUR_MODEL_KEY.NOT_SIGNED_IN_BODY)).toBeVisible();
  // …and the page's own lede names whose sign-in it is (finding 2b). The old
  // SETUP_SUMMARY_HELPER — "shared credentials … your runs inherit them" — is
  // the sentence this deployment contradicts, so its ABSENCE is the assertion
  // that the branch actually took.
  await expect(page.getByText(MEMBER_GETTING_STARTED.SETUP_SUMMARY_HELPER_PER_USER)).toBeVisible();
  await expect(page.getByText(MEMBER_GETTING_STARTED.SETUP_SUMMARY_HELPER)).toHaveCount(0);
  // P1 + P5: the member must REACH their own sign-in from their own seat, and
  // must not block on a cold image pull.
  //
  // 0.7.5 BUILD 0(b), lane login-sandbox-selfrun (merged): the SANDBOX runs the
  // chained command itself, so neither the console nor this file types anything
  // — the 60 s poll for the echoed argv and its TYPING fallback are gone (see
  // awaitSelfRunStarted() for the double-run they caused once the image
  // self-ran). And P1 is no longer witnessed by asserting the terminal NODE is
  // on screen: the pane now unmounts it within half a second of the capture,
  // which a boot-time sign-in against a pre-approving fake can reach before the
  // assertion's first poll. signInThroughPane() takes the sandbox's own banner
  // or the server's moved capture instead — see there.
  await signInThroughPane(page, openLoginPane);

  // THE MEMBER'S OWN STATUS, from the member's own session.
  await page.goto("/setup");
  await expect.poll(async () => (await modelAccess(page)).state, { timeout: 120_000 }).toBe("live");
  // …and the card now reads the signed-in half of the same pair of constants.
  await expect(page.getByText(YOUR_MODEL_KEY.SIGNED_IN_CHIP).first()).toBeVisible({ timeout: 60_000 });
});

test("the capture belongs to the member alone", async ({ page }) => {
  // Same install, same moment, the other principal: still not_configured. A
  // shared credential would read `live` here, which is exactly the failure
  // per_user exists to prevent.
  await dexSignIn(page, ADMIN_EMAIL);
  const adminAccess = await modelAccess(page);
  expect(adminAccess.state, "the admin inherited the member's captured credential").toBe("not_configured");
  await dexSignOut(page);
});

test("the member's run gets the member's PINNED identity, and something spends it", async ({ page }) => {
  await dexSignIn(page, MEMBER_EMAIL);

  await launchAgentRun(page, "bedrock via my own AWS SSO session");

  // /_seen is the observation that is not Wardyn asserting about itself: it is
  // what the AWS SDK actually asked the portal to mint. Index 0 of the fixture
  // is a DIFFERENT account, so naming the pin here is a real answer.
  await expect
    .poll(async () => (await seen()).account_id, { timeout: 180_000 })
    .toBe(PIN_ACCOUNT);
  expect((await seen()).role_name).toBe(PIN_ROLE);

  // …and the minted credential was SPENT: the bedrock-runtime stub was hit, on
  // the model ARN this deployment configured.
  await expect.poll(async () => (await seen()).bedrock_calls, { timeout: 180_000 }).toBeGreaterThan(0);
  // THE SET, not the last one. A claude-code run is not a single model call:
  // the CLI drives a small fast model of its own alongside the configured one
  // (observed: us.anthropic.claude-haiku-4-5), so `bedrock_model` — which is
  // last-write-wins — is whichever happened to land last, and asserting the
  // operator's ARN against it was reading a coin flip. It passed, then failed
  // on the very next run with the same code and the same cluster.
  await expect
    .poll(async () => (await seen()).bedrock_models.some((m) => m.includes(PIN_ACCOUNT)), { timeout: 180_000 })
    .toBe(true);
});

test("sso-pin-dispatch: a pin changed after capture warns, refuses the run, and heals on re-sign-in", async ({
  page,
  request,
}) => {
  // P4, lane `sso-pin-dispatch`, merged — flipped from test.fixme in W5.
  //
  // Setting a pin that contradicts a STORED capture must (a) grade the member's
  // own model access `expired_signin` so the repair button is offered at all
  // (modelaccess.go's awsSSOPinContradiction arm), (b) REFUSE the run rather
  // than silently spending the wrong identity, and (c) heal once the roster and
  // the stored capture agree again.
  //
  // THE CONTRADICTING PIN IS 111111111111/DevPower, NOT AN INVENTED ACCOUNT.
  // It is index 0 of the fake's entitlement fixture
  // (deploy/kind/sso/awsssofake.yaml) — a real, entitled account that is NOT
  // the one the member captured. An account the fake does not serve would make
  // the heal below unprovable: cmd/wardyn-aws-sso can only ever capture a pair
  // the portal actually mints, so `ListAccountRoles` would have nothing to
  // return and the re-sign-in would fail for a reason that has nothing to do
  // with P4.
  const CONTRA_ACCOUNT = "111111111111";
  const CONTRA_ROLE = "DevPower";
  await putRoster(request, CONTRA_ACCOUNT, CONTRA_ROLE);

  await dexSignIn(page, MEMBER_EMAIL);
  await page.goto("/setup");
  await expect.poll(async () => (await modelAccess(page)).state, { timeout: 120_000 }).toBe("expired_signin");
  await expect(page.getByRole("button", { name: "Sign in to AWS" }).first()).toBeVisible({ timeout: 60_000 });

  // The refusal is enforceCreateLLMMechanism's 422 (runs_dispatch_llm_mechanism.go:
  // pinContradictionRefusal, checked BEFORE the mechanism fold), so no run row
  // is ever created and no sandbox is scheduled — which is why a Terminal run is
  // enough here and why this assertion is seconds rather than minutes.
  //
  // The sentence asserted is llmMechanismPinContradictedSentence's, NOT
  // ssoTokenAccountPinRefusal's: they are two different refusals with two
  // different spellings ("this agent NOW PINS AWS sign-ins TO account" vs
  // "pins AWS sign-ins FOR THIS AGENT TO account"), and the one a launch meets
  // is the dispatch/create one.
  await page.goto("/runs/new");
  await page.getByRole("combobox", { name: "Title" }).fill("a run under a contradicted pin");
  await page.getByRole("radio", { name: /^Terminal/ }).click();
  await page.getByRole("button", { name: /^Launch/ }).click();
  await expect(page.getByText(/now pins AWS sign-ins to account/)).toBeVisible({ timeout: 60_000 });

  // ── the heal ──────────────────────────────────────────────────────────────
  // Signing in again IS the repair: a new login run stamps the CURRENT pin and
  // its capture REPLACES the stored blob (there is no server-side invalidation
  // anywhere — modelaccess.go says so). So the member repeats exactly what they
  // did in the second test, under the new pin, and their own status comes back
  // to `live` on the contradicting pair.
  // 0.7.5 BUILD 0(b) again — the SAME poll-then-type pair lived here too, and a
  // fix applied only to the first copy would have left this second sign-in
  // double-running. Same helper as the second test, for the same reason.
  await signInThroughPane(page, openLoginPane);

  await page.goto("/setup");
  await expect.poll(async () => (await modelAccess(page)).state, { timeout: 120_000 }).toBe("live");

  // …and the healed capture is genuinely the NEW pin's, proven the only way it
  // can be: by SPENDING it.
  //
  // `/_seen` reports what GetRoleCredentials was last asked to mint, and a
  // capture never calls GetRoleCredentials — signing in reads the portal
  // (ListAccounts / ListAccountRoles, which is how verifyPin checks the pin is
  // reachable) and stops there. Only DISPATCH mints. So asserting /_seen
  // straight after the re-sign-in read the pair the previous test's run had
  // minted, and "111111111111 != 222222222222" was the assertion catching its
  // own staleness rather than anything about the heal.
  await launchAgentRun(page, "a run after the pin moved");
  await expect
    .poll(async () => (await seen()).account_id, { timeout: 180_000 })
    .toBe(CONTRA_ACCOUNT);
  expect((await seen()).role_name).toBe(CONTRA_ROLE);
});

test("member-mode: an admin drops to member mode, is refused, and comes back", async ({ page }) => {
  // Owner ask (ii) / P2, lane `member-mode`, merged — flipped from test.fixme in
  // W5. A session flag an ADMIN sets on themselves, so operator authority is
  // genuinely gone for the duration — not a UI pretence.
  //
  // The three strings are the merged lane's own DRAFT constants, imported
  // rather than quoted: the W6 member lens made the banner tier-neutral after
  // this case was written, and a quoted copy asserted the retired wording. A
  // regex loose enough to match both drafts would have asserted nothing.
  const MENU_ITEM = MEMBER_MODE.MENU;
  const BANNER = MEMBER_MODE.BANNER;
  const EXIT = MEMBER_MODE.EXIT;

  await dexSignIn(page, ADMIN_EMAIL);
  const adminWho = await me(page);
  expect(adminWho.operator).toBe(true);

  // Entering reloads at the root (MemberModeMenuItem's default onEntered): the
  // session cookie changed and every screen's cached data was fetched as an
  // admin. Wait for that navigation rather than racing it.
  await page.locator("header").getByRole("button").last().click();
  await page.getByRole("menuitem", { name: MENU_ITEM }).click();
  // The banner FIRST, not /me: it is only on the page the reload produced, so
  // waiting for it is what makes every `page.evaluate` below run against the
  // reloaded document instead of racing the navigation that is tearing the old
  // one down. It is also the assertion that matters most — the mode is VISIBLE
  // from inside it, which is the one thing that makes it not a trap.
  await expect(page.getByText(BANNER)).toBeVisible({ timeout: 60_000 });

  await expect.poll(async () => (await me(page)).operator, { timeout: 30_000 }).toBe(false);
  expect((await me(page)).member_mode).toBe(true);

  // The flag is enforced SERVER-SIDE: writing a secret into ANOTHER principal's
  // namespace is an operator act, and this session no longer has that authority.
  //
  // PUT /api/v1/secrets/{name}?owner=… is the real shape (routes.go) — ?owner=
  // is a QUERY parameter that secretOwnerParam gates ("?owner= is admin-only",
  // 403), not a body field, and there is no POST /secrets at all. The earlier
  // draft of this case sent a POST with `owner` in the body, which this
  // deployment would have answered 405 — a red that says nothing about the mode.
  // The value is ≥ secretmask.MinLen so a 400 can never be mistaken for the 403.
  //
  // ?owner= NAMES THE ADMIN'S OWN SUBJECT, not an email and not the member's.
  // resolveSecretOwner maps the value onto a namespace and answers 422 ("names
  // an email address that no principal on this deployment is known by … name the
  // subject instead") for an email it cannot fold onto a known principal — the
  // member is known by their Dex sub, so `member@wardyn.local` legitimately 422s
  // for an ADMIN, which is product behaviour and not this case's subject.
  // Using the caller's own sub takes owner-resolution out of the experiment
  // entirely: the URL is identical on both probes, so the ONLY variable between
  // the 403 and the success is the mode itself. A non-operator is refused for
  // naming ?owner= AT ALL, whatever value it carries.
  const probe = async (owner: string) =>
    page.evaluate(async (o: string) => {
      const r = await fetch(`/api/v1/secrets/member-mode-probe?owner=${encodeURIComponent(o)}`, {
        method: "PUT",
        credentials: "include",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ value: "member-mode-probe-value" }),
      });
      return r.status;
    }, owner);
  expect(
    await probe(adminWho.principal ?? ""),
    "member mode did not bind server-side — the flag is decoration",
  ).toBe(403);

  // The way OUT is the banner's own button, on every screen — not the account
  // menu, which correctly stops offering the control once the mode is on
  // (both of MemberModeMenuItem's predicates are false from inside).
  await page.getByRole("button", { name: EXIT }).click();
  // Same reason as the entry: exiting reloads, so wait for the banner to be
  // GONE before asking /me anything.
  await expect(page.getByText(BANNER)).toBeHidden({ timeout: 60_000 });
  await expect.poll(async () => (await me(page)).operator, { timeout: 30_000 }).toBe(true);

  // …and the authority genuinely came back: the same probe now succeeds. A mode
  // that could not be left would pass every assertion above.
  expect(
    await probe(adminWho.principal ?? ""),
    "the admin did not get their operator authority back on exit",
  ).toBe(204);
});
