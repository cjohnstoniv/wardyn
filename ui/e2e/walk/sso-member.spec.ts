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
 *   - the credential that login captured is THEIRS: `/setup/status` reads the
 *     walk provider's access `live` for the member and `not_configured` for the
 *     admin on the same install, at the same moment;
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
 * sessions and the read/write helpers moved to ui/e2e/walk/helpers.ts so both
 * files use the same ones; this file's own order and assertions are unchanged
 * apart from the two 0.7.5 edits marked in place. THIS FILE RUNS FIRST and
 * leaves the member `live` under the CONTRADICTING pair — the recovery file
 * depends on both facts and says so in its header.
 */

import { expect, test } from "@playwright/test";
import { CONSOLE_VIEW } from "../../src/app/components/wardyn/copy/console-view";
import { BANNER, CONNECTIONS } from "../../src/app/components/wardyn/copy/door";
// Plain constant tables with no CSS import — the rule ui/e2e/walk/helpers.ts
// states for SELFRUN_MARKER, and what keeps `playwright test --project=walk
// --list` green.
import { MODEL_ACCESS_BANNER, RAIL_MODEL_ACCESS } from "../../src/app/components/wardyn/model-access-copy";
import { RAIL_CREDENTIAL, RAIL_PROVIDER } from "../../src/app/components/wardyn/copy/new-run-rail";
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
  cardSignInAws,
  openModelConnections,
  openLoginPaneAssertingColdPull,
  putProvider,
  seen,
  signInThroughPane,
  WALK_PROVIDER_NAME,
} from "./helpers";

// The shell strip's sentence for a member who has never signed in: claude-code
// runs use the walk's provider, whose default it is.
const STRIP_NOT_SIGNED_IN = BANNER.B1("Claude Code", WALK_PROVIDER_NAME);

test.skip(process.env.WARDYN_TEST_K8S !== "1", "live cluster walk: set WARDYN_TEST_K8S=1 (scripts/kind-sso-walk.sh)");
test.describe.configure({ mode: "serial" });

// ── the walk ────────────────────────────────────────────────────────────────

test("the admin declares the Bedrock SSO provider and pins the account", async ({ page, request }) => {
  expect(ADMIN_TOKEN, "WARDYN_WALK_ADMIN_TOKEN is unset — run this through scripts/kind-sso-walk.sh").not.toBe("");

  await dexSignIn(page, ADMIN_EMAIL);
  const who = await me(page);
  expect(who.email, "the session Dex handed back is not the admin's").toBe(ADMIN_EMAIL);
  expect(who.operator, "admin@wardyn.local must resolve as an operator (WARDYN_OIDC_ROLE_MAP)").toBe(true);

  await putProvider(request);

  // The admin declared the provider; declaring it signs NOBODY in, the admin
  // included. This is the assertion the whole per-person design rests on.
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
  // (an expired sign-in, a DIFFERENT sentence). A member's own credential is
  // theirs to remove and nobody else's, so not even the walk's admin token can
  // put the member back. So the first-run strip is asserted HERE, in the four
  // seconds between the admin declaring the provider and the member signing
  // in, and this case must stay BEFORE the capture and must not make one.
  //
  // It is read-only for that reason: the door is never OPENED here. The
  // provider's sign-in pane launches its login sandbox the moment it opens, so
  // opening it would start the very capture this case must not make.
  await dexSignIn(page, MEMBER_EMAIL);
  expect((await modelAccess(page)).state, "case I must run before the member's first capture").toBe("not_configured");

  // (1) THE RUNS BOARD — a screen that has never mentioned model access. The
  // strip is a LAZY chunk behind a Suspense fallback of null (app-shell.tsx),
  // and the door says nothing at all until /me has resolved the viewer, so this
  // is awaited rather than read on the first frame.
  await page.goto("/runs");
  await expect(page.getByText(STRIP_NOT_SIGNED_IN)).toBeVisible({ timeout: 60_000 });
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
  await expect(page.getByText(STRIP_NOT_SIGNED_IN)).toBeVisible({ timeout: 60_000 });
  await expect(page.getByRole("button", { name: AGENTS.SIGN_IN_AWS, exact: true })).toBeVisible();

  // (3) NEW RUN — the rail names the one provider this run would use and
  // where its credential lives, with no click; the deployment-wide "No model
  // provider is connected" is false here (the provider exists) and must not
  // speak over it. The strip, which the rail does not claim, keeps its button.
  await page.goto("/runs/new");
  await page.getByRole("radio", { name: /^Autonomous/ }).click();
  await expect(page.getByText(RAIL_PROVIDER.STATIC(WALK_PROVIDER_NAME))).toBeVisible({ timeout: 60_000 });
  await expect(page.getByText(RAIL_CREDENTIAL.SANDBOX_BEDROCK)).toBeVisible();
  await expect(page.getByText(RAIL_MODEL_ACCESS.NO_PROVIDER)).toHaveCount(0);
  await expect(page.getByText(STRIP_NOT_SIGNED_IN)).toBeVisible();

  // (5) THE ONE SUPPRESSION A MEMBER GETS, and the one they deliberately do
  // NOT. Getting Started IS the door, so the strip is withheld there — the
  // card below is what says it instead.
  //
  // The provider's connection row reads the SAME not_configured state as the
  // strip, and the page's own summary chip says Needs you.
  // Getting Started keeps only the summary chip (packet MP-D, #548); the row
  // itself is on Your account.
  await page.goto("/setup");
  await expect(page.getByText(CONNECTIONS.SUMMARY_NEEDS_YOU)).toBeVisible({ timeout: 60_000 });
  await expect(page.getByText(STRIP_NOT_SIGNED_IN)).toHaveCount(0);
  // …and on /account the strip keeps its sentence (the connections card there
  // claims the door only while it is expanded); opened, the row reads it too.
  await page.goto("/account");
  await expect(page.getByText(STRIP_NOT_SIGNED_IN)).toBeVisible({ timeout: 60_000 });
  await openModelConnections(page);
  await expect(page.getByText(CONNECTIONS.NOT_SIGNED_IN).first()).toBeVisible({ timeout: 60_000 });
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
  // The provider's connection row reads the NOT-SIGNED-IN half — the one live
  // seat that can, with a real second identity — and the page's lede names
  // the sign-in as the person's own (finding 2b).
  await openModelConnections(page);
  await expect(page.getByText(CONNECTIONS.NOT_SIGNED_IN).first()).toBeVisible({ timeout: 60_000 });
  await expect(page.getByText(CONNECTIONS.LEDE)).toBeVisible();
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
  //
  // #891: this is the walk's ONE genuinely cold pull — scripts/kind-sso-walk.sh
  // now serves the aws-sso image from a local registry under a fresh tag every
  // run instead of `kind load`ing it, so the door's download step must light
  // here. The re-sign-in later in this file (the heal, after a pin contradiction)
  // reuses the plain openLoginPane: by then the node already pulled this image.
  await signInThroughPane(page, openLoginPaneAssertingColdPull);

  // THE MEMBER'S OWN STATUS, from the member's own session.
  await openModelConnections(page);
  await expect.poll(async () => (await modelAccess(page)).state, { timeout: 120_000 }).toBe("live");
  // …and the row now reads the signed-in half of the same pair of constants.
  await expect(page.getByText(CONNECTIONS.SIGNED_IN).first()).toBeVisible({ timeout: 60_000 });
});

test("the capture belongs to the member alone", async ({ page }) => {
  // Same install, same moment, the other principal: still not_configured. A
  // shared credential would read `live` here, which is exactly the failure a
  // per-person provider exists to prevent.
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
  // own access to the provider `expired_signin` so the repair button is offered
  // at all (providerPinContradiction), (b) REFUSE the run rather than silently
  // spending the wrong identity, and (c) heal once the provider's pin and the
  // stored capture agree again.
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
  await putProvider(request, CONTRA_ACCOUNT, CONTRA_ROLE);

  await dexSignIn(page, MEMBER_EMAIL);
  await openModelConnections(page);
  await expect.poll(async () => (await modelAccess(page)).state, { timeout: 120_000 }).toBe("expired_signin");
  await expect(cardSignInAws(page)).toBeVisible({ timeout: 60_000 });

  // The refusal is the provider's own (providerBedrockRefusal, mpBRPinned):
  // the run does not spend the wrong identity, and the sentence names both
  // pairs so the member knows which to pick.
  await page.goto("/runs/new");
  await page.getByRole("combobox", { name: "Title" }).fill("a run under a contradicted pin");
  await page.getByRole("radio", { name: /^Terminal/ }).click();
  await page.getByRole("button", { name: /^Launch/ }).click();
  await expect(page.getByText(new RegExp(`and it pins account ${CONTRA_ACCOUNT}`)).first()).toBeVisible({ timeout: 180_000 });

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

  await openModelConnections(page);
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

test("the view switch: an admin drops to the User view, is refused, and comes back", async ({ page }) => {
  // M-2 (#634): the switch is the 0.7.4 member-mode clamp, promoted from the
  // avatar menu to the top bar. A session flag an ADMIN sets on themselves, so
  // operator authority is genuinely gone for the duration — not a UI pretence.
  const views = () => page.getByRole("group", { name: CONSOLE_VIEW.GROUP });
  const segment = (name: string) => views().getByRole("button", { name });

  await dexSignIn(page, ADMIN_EMAIL);
  const adminWho = await me(page);
  expect(adminWho.operator).toBe(true);
  // D2: a fresh session lands in the Admin view.
  await expect(segment(CONSOLE_VIEW.ADMIN)).toHaveAttribute("aria-pressed", "true");

  // Switching reloads the console into the User view's home: the session cookie
  // changed and every screen's cached data was fetched as an admin. The pressed
  // segment is only on the page the reload produced, so waiting for it is what
  // makes every `page.evaluate` below run against the reloaded document.
  await segment(CONSOLE_VIEW.USER).click();
  await expect(page).toHaveURL(/\/runs$/, { timeout: 60_000 });
  await expect(segment(CONSOLE_VIEW.USER)).toHaveAttribute("aria-pressed", "true", { timeout: 60_000 });

  await expect.poll(async () => (await me(page)).operator, { timeout: 30_000 }).toBe(false);
  expect((await me(page)).user_view).toBe(true);

  // The flag is enforced SERVER-SIDE: reading ANOTHER principal's secret
  // namespace is an operator act, and this session no longer has that authority.
  //
  // GET /api/v1/secrets?owner=… is the probe — ?owner= is a QUERY parameter that
  // secretOwnerParam gates ("?owner= is admin-only", 403). Not the PUT: since 0.8
  // a PUT refuses ?owner= for everyone (a credential is set only by its owner),
  // so it could no longer show the authority coming back.
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
      const r = await fetch(`/api/v1/secrets?owner=${encodeURIComponent(o)}`, {
        method: "GET",
        credentials: "include",
      });
      return r.status;
    }, owner);
  expect(
    await probe(adminWho.principal ?? ""),
    "the User view did not bind server-side — the flag is decoration",
  ).toBe(403);

  // The way back is the other segment, on every screen.
  await segment(CONSOLE_VIEW.ADMIN).click();
  await expect(page).toHaveURL(/\/admin\//, { timeout: 60_000 });
  await expect.poll(async () => (await me(page)).operator, { timeout: 30_000 }).toBe(true);

  // …and the authority genuinely came back: the same probe now succeeds. A mode
  // that could not be left would pass every assertion above.
  expect(
    await probe(adminWho.principal ?? ""),
    "the admin did not get their operator authority back on exit",
  ).toBe(200);
});

test("S1: an Admin-view session starts no run; the User view and a bearer still reach the doors", async ({ page, request }) => {
  // M-8 (#639). The probes send no agent, so a door that lets the caller
  // through answers its own validation error and no sandbox is ever started.
  const launch = async (path: string) =>
    page.evaluate(async (p: string) => {
      const r = await fetch(p, {
        method: "POST",
        credentials: "include",
        headers: { "Content-Type": "application/json" },
        body: "{}",
      });
      return { status: r.status, body: await r.json().catch(() => ({})) };
    }, path);
  const doors = ["/api/v1/runs", "/api/v1/runs/preflight"];
  const segment = (name: string) => page.getByRole("group", { name: CONSOLE_VIEW.GROUP }).getByRole("button", { name });

  await dexSignIn(page, ADMIN_EMAIL);
  await expect(segment(CONSOLE_VIEW.ADMIN)).toHaveAttribute("aria-pressed", "true");
  for (const door of doors) {
    const got = await launch(door);
    expect(got.status, `${door} from the Admin view`).toBe(409);
    expect(got.body).toEqual({
      error: "Runs start in the user view. Use User view at the top of the console to start one.",
      reason: "admin_view",
    });
  }

  await segment(CONSOLE_VIEW.USER).click();
  await expect(segment(CONSOLE_VIEW.USER)).toHaveAttribute("aria-pressed", "true", { timeout: 60_000 });
  for (const door of doors) {
    const got = await launch(door);
    expect(got.status, `${door} from the User view`).not.toBe(409);
    expect(got.body.reason ?? "").not.toBe("admin_view");
  }

  // The CLI's lane never carried the session, so the view does not reach it.
  for (const door of doors) {
    const r = await request.post(door, { headers: { Authorization: `Bearer ${ADMIN_TOKEN}` }, data: {} });
    expect(r.status(), `${door} with the admin token`).not.toBe(409);
  }

  await segment(CONSOLE_VIEW.ADMIN).click();
  await expect(page).toHaveURL(/\/admin\//, { timeout: 60_000 });
});

test("the view switch: another tab follows the session into the same view", async ({ page, context }) => {
  // §2.4: the view is the session's, so a second tab must not keep painting the
  // other one. The BroadcastChannel is the fast path; focus, the minute poll and
  // any 403 are the backstops.
  const segment = (p: typeof page, name: string) =>
    p.getByRole("group", { name: CONSOLE_VIEW.GROUP }).getByRole("button", { name });

  await dexSignIn(page, ADMIN_EMAIL);
  const other = await context.newPage();
  await other.goto("/admin/runs");
  await expect(segment(other, CONSOLE_VIEW.ADMIN)).toHaveAttribute("aria-pressed", "true", { timeout: 60_000 });

  await segment(page, CONSOLE_VIEW.USER).click();
  // /admin/runs has a twin, so the other tab lands on the same object.
  await expect(other).toHaveURL(/\/runs$/, { timeout: 60_000 });
  await expect(other).not.toHaveURL(/\/admin\//);
  await expect(segment(other, CONSOLE_VIEW.USER)).toHaveAttribute("aria-pressed", "true", { timeout: 60_000 });

  await segment(other, CONSOLE_VIEW.ADMIN).click();
  await expect(page).toHaveURL(/\/admin\//, { timeout: 60_000 });
});
