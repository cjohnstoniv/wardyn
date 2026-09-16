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
 * ── TESTS MARKED test.fixme ─────────────────────────────────────────────────
 * Three sibling 0.7.4 lanes land the behaviour some of these assertions
 * describe. Each such test is written against the PLAN's stated end state and
 * marked `test.fixme("<lane>")`; the coordinator flips them in W5 once the lane
 * is merged. A fixme is deliberate debt with a name on it — never a silent skip.
 */

import { execFileSync } from "node:child_process";
import { expect, test, type Page, type APIRequestContext } from "@playwright/test";

// ── the walk's inputs (scripts/kind-sso-walk.sh exports every one) ──────────
const ADMIN_TOKEN = process.env.WARDYN_LIVE_ADMIN_TOKEN || "";
const PIN_ACCOUNT = process.env.WARDYN_LIVE_PIN_ACCOUNT || "222222222222";
const PIN_ROLE = process.env.WARDYN_LIVE_PIN_ROLE || "WardynDev";
const SSO_START_URL = process.env.WARDYN_LIVE_SSO_START_URL || "https://wardyn-dev.awsapps.com/start";
const FAKE_URL = process.env.WARDYN_LIVE_FAKE_URL || "http://wardyn-awsssofake.wardyn.svc.cluster.local:8090";
const KUBE_CONTEXT = `kind-${process.env.WARDYN_QUICKSTART_CLUSTER || "wardyn-quickstart"}`;

const ADMIN_EMAIL = "admin@wardyn.local";
const MEMBER_EMAIL = "member@wardyn.local";
/** deploy/kind/sso/README.md's demo literal — a throwaway Dex, no secret. */
const DEX_PASSWORD = "password";

/** harness-login-pane.tsx's aws.doneMarker — the helper's PTY success contract. */
const SUCCESS_MARKER = "wardyn: aws sso credential captured";
/** harness-login-pane.tsx's aws.cmd — chained so the upload needs no second command. */
const CHAINED_CMD = "aws sso login --sso-session wardyn --no-browser --use-device-code && wardyn-aws-sso";

// A sandbox on a cluster is a pod: image pull, schedule, proxy sidecar, then a
// device-code flow. Generous, and bounded — an unbounded wait is how a live
// suite turns a failure into a hang.
const SANDBOX_UP = 300_000;
const LOGIN_DONE = 300_000;

test.skip(process.env.WARDYN_TEST_K8S !== "1", "live cluster walk: set WARDYN_TEST_K8S=1 (scripts/kind-sso-walk.sh)");
test.describe.configure({ mode: "serial" });

// ── helpers ─────────────────────────────────────────────────────────────────

/**
 * Sign in through Dex's static-password form.
 *
 * Deliberately a local copy of ui/e2e/demo/sso.ts's dexSignIn rather than an
 * import: that module pulls in the demo narration overlay (captions, beats, a
 * recording), none of which belongs in a test gate. The three locators are the
 * whole of it — Dex's login form is plain HTML with no accessible names.
 */
async function dexSignIn(page: Page, email: string): Promise<void> {
  await page.goto("/");
  await page
    .getByRole("link", { name: "Sign in with SSO" })
    .or(page.getByRole("button", { name: "Sign in with SSO" }))
    .first()
    .click();
  await page.locator('input[type="password"]').waitFor({ timeout: 60_000 });
  await page.locator('input[type="text"], input[name="login"]').first().fill(email);
  await page.locator('input[type="password"]').fill(DEX_PASSWORD);
  await page.getByRole("button", { name: /log ?in/i }).click();
  await expect(page.getByRole("link", { name: /^Runs/ })).toBeVisible({ timeout: 60_000 });
}

async function dexSignOut(page: Page): Promise<void> {
  await page.locator("header").getByRole("button").last().click();
  await page
    .getByRole("button", { name: "Sign out" })
    .or(page.getByRole("menuitem", { name: "Sign out" }))
    .first()
    .click();
  await expect(
    page.getByRole("link", { name: "Sign in with SSO" }).or(page.getByRole("button", { name: "Sign in with SSO" })).first(),
  ).toBeVisible({ timeout: 60_000 });
}

/** The signed-in browser session's own view of who it is. */
async function me(page: Page): Promise<{ principal?: string; operator?: boolean; member_mode?: boolean }> {
  return page.evaluate(async () => {
    const r = await fetch("/api/v1/me", { credentials: "include" });
    return (await r.json()) as Record<string, unknown>;
  });
}

/** The signed-in session's /setup/status — a MEMBER's own answer, not an admin's. */
async function modelAccess(page: Page): Promise<{ state?: string; action?: string }> {
  return page.evaluate(async () => {
    const r = await fetch("/api/v1/setup/status", { credentials: "include" });
    const body = (await r.json()) as { model_access?: { state?: string; action?: string } };
    return body.model_access ?? {};
  });
}

/**
 * The fake's `/_seen`, read from INSIDE the cluster.
 *
 * There is deliberately no host-side route to the fake: the whole fourth
 * precondition is that it is addressed by its in-cluster Service. So the read
 * goes through the control-plane pod, the same way the walk script reads it.
 */
function seen(): { account_id: string; role_name: string; bedrock_calls: number; bedrock_model: string } {
  const out = execFileSync(
    "kubectl",
    ["--context", KUBE_CONTEXT, "-n", "wardyn", "exec", "deployment/wardyn", "--", "wget", "-qO-", `${FAKE_URL}/_seen`],
    { encoding: "utf8" },
  );
  return JSON.parse(out);
}

/** The agent roster write: the pin and the per-user lane, in one PUT. */
async function putRoster(request: APIRequestContext): Promise<void> {
  const res = await request.put("/api/v1/agent-providers", {
    headers: { Authorization: `Bearer ${ADMIN_TOKEN}`, "Content-Type": "application/json" },
    data: {
      agents: {
        "claude-code": {
          mechanism: "bedrock_sso",
          credential_source: "per_user",
          sso_start_url: SSO_START_URL,
          sso_account_id: PIN_ACCOUNT,
          sso_role_name: PIN_ROLE,
        },
      },
    },
  });
  expect(res.status(), `PUT /agent-providers: ${await res.text()}`).toBe(200);
}

// ── the walk ────────────────────────────────────────────────────────────────

test("the admin declares the per-user Bedrock SSO lane and pins the account", async ({ page, request }) => {
  expect(ADMIN_TOKEN, "WARDYN_LIVE_ADMIN_TOKEN is unset — run this through scripts/kind-sso-walk.sh").not.toBe("");

  await dexSignIn(page, ADMIN_EMAIL);
  const who = await me(page);
  expect(who.principal).toContain("admin@wardyn.local");
  expect(who.operator, "admin@wardyn.local must resolve as an operator (WARDYN_OIDC_ROLE_MAP)").toBe(true);

  await putRoster(request);

  // The admin declared the lane; declaring it signs NOBODY in, the admin
  // included. This is the assertion the whole per_user design rests on.
  const adminAccess = await modelAccess(page);
  expect(adminAccess.state, "the admin who declared the lane must not inherit a credential").toBe("not_configured");
  await dexSignOut(page);
});

test("the member signs in to AWS from their own seat and the capture is theirs", async ({ page }) => {
  await dexSignIn(page, MEMBER_EMAIL);
  const who = await me(page);
  expect(who.principal).toContain("member@wardyn.local");
  expect(who.operator).toBe(false);

  // Before: the member has a real action to take, not a dead end.
  const before = await modelAccess(page);
  expect(before.state).toBe("not_configured");
  expect(before.action).toBe("Sign in to AWS");

  // The member's own Getting Started carries the CTA (P3 / lane
  // member-cold-load: the member's page must not call an admin-only endpoint).
  await page.goto("/setup");
  const cta = page.getByRole("button", { name: "Sign in to AWS" }).first();
  await expect(cta).toBeVisible({ timeout: 60_000 });
  await cta.click();

  // The pane launches the login sandbox on open. The start URL is roster-managed
  // here (the admin set sso_start_url), so the pane goes straight to "Start
  // login" rather than asking for one.
  const start = page.getByRole("button", { name: "Start login" });
  if (await start.isVisible().catch(() => false)) {
    await start.click();
  }

  // P1 + P5 (lane login-pane): the pane must REACH the terminal from a member's
  // seat and must not block on a cold image pull. Today's gate is the assertion
  // below; the lane adds the banner and the async launch.
  const screen = page.locator(".xterm-screen").first();
  await expect(screen, "the member never reached their own login sandbox's terminal (P1)").toBeVisible({
    timeout: SANDBOX_UP,
  });

  // The pane AUTO-TYPES the chained command. If it has not (a pane change, a
  // dropped keystroke), type it into the PTY ourselves — the xterm idiom from
  // ui/e2e/demo: click the screen, then page.keyboard.type with a trailing \n.
  // Assertions on xterm are expect.poll(innerText) per the series law:
  // toContainText starves on a canvas-backed buffer that repaints under load.
  await expect
    .poll(async () => (await screen.innerText()).includes("aws sso login"), { timeout: 60_000 })
    .toBe(true)
    .catch(async () => {
      await screen.click();
      await page.keyboard.type(`${CHAINED_CMD}\n`, { delay: 20 });
    });

  // The helper's own success marker — the PTY contract cmd/wardyn-aws-sso and
  // the pane share (TestSuccessMarker_UIParity pins the two spellings equal).
  await expect
    .poll(async () => (await screen.innerText()).includes(SUCCESS_MARKER), { timeout: LOGIN_DONE })
    .toBe(true);

  // THE MEMBER'S OWN STATUS, from the member's own session.
  await page.goto("/setup");
  await expect.poll(async () => (await modelAccess(page)).state, { timeout: 120_000 }).toBe("live");
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

  await page.goto("/runs/new");
  await page.getByRole("combobox", { name: "Title" }).fill("bedrock via my own AWS SSO session");
  await page.getByRole("radio", { name: /^Terminal/ }).click();
  await page.getByRole("button", { name: /^Launch/ }).click();
  await expect(page.locator(".xterm-screen").first()).toBeVisible({ timeout: SANDBOX_UP });

  // /_seen is the observation that is not Wardyn asserting about itself: it is
  // what the AWS SDK actually asked the portal to mint. Index 0 of the fixture
  // is a DIFFERENT account, so naming the pin here is a real answer.
  await expect
    .poll(() => seen().account_id, { timeout: 180_000 })
    .toBe(PIN_ACCOUNT);
  expect(seen().role_name).toBe(PIN_ROLE);

  // …and the minted credential was SPENT: the bedrock-runtime stub was hit, on
  // the model ARN this deployment configured.
  await expect.poll(() => seen().bedrock_calls, { timeout: 180_000 }).toBeGreaterThan(0);
  expect(seen().bedrock_model).toContain(PIN_ACCOUNT);
});

test.fixme("sso-pin-dispatch: a pin changed after capture warns, refuses the run, and heals on re-sign-in", async ({
  page,
  request,
}) => {
  // P4, lane `sso-pin-dispatch`. Written against the plan's stated end state:
  // setting a pin that contradicts a STORED capture must (a) turn the setup
  // rows to warn, (b) REFUSE dispatch with the named sentence rather than
  // silently running on the wrong identity, and (c) heal when the member signs
  // in again and captures the pin's pair. The coordinator flips this in W5.
  const res = await request.put("/api/v1/agent-providers", {
    headers: { Authorization: `Bearer ${ADMIN_TOKEN}`, "Content-Type": "application/json" },
    data: {
      agents: {
        "claude-code": {
          mechanism: "bedrock_sso",
          credential_source: "per_user",
          sso_start_url: SSO_START_URL,
          sso_account_id: "333333333333",
          sso_role_name: "SomeOtherRole",
        },
      },
    },
  });
  expect(res.status()).toBe(200);

  await dexSignIn(page, MEMBER_EMAIL);
  await page.goto("/setup");
  await expect.poll(async () => (await modelAccess(page)).state, { timeout: 120_000 }).toBe("expired_signin");
  await expect(page.getByRole("button", { name: "Sign in to AWS" }).first()).toBeVisible({ timeout: 60_000 });

  await page.goto("/runs/new");
  await page.getByRole("combobox", { name: "Title" }).fill("a run under a contradicted pin");
  await page.getByRole("radio", { name: /^Terminal/ }).click();
  await page.getByRole("button", { name: /^Launch/ }).click();
  await expect(page.getByText(/pins AWS sign-ins for this agent to account/)).toBeVisible({ timeout: 60_000 });
});

test.fixme("member-mode: an admin drops to member mode, is refused, and comes back", async ({ page }) => {
  // Owner ask (ii) / P2, lane `member-mode`. Written against the plan: a session
  // flag an ADMIN sets on themselves, so operator authority is genuinely gone
  // for the duration — not a UI pretence. The coordinator flips this in W5.
  await dexSignIn(page, ADMIN_EMAIL);
  expect((await me(page)).operator).toBe(true);

  await page.locator("header").getByRole("button").last().click();
  await page.getByRole("menuitem", { name: /member mode/i }).click();

  await expect.poll(async () => (await me(page)).operator, { timeout: 30_000 }).toBe(false);
  expect((await me(page)).member_mode).toBe(true);
  await expect(page.getByText(/member mode/i).first()).toBeVisible();

  // The flag is enforced SERVER-SIDE: a secret written on another owner's name
  // is an operator act, and this session no longer has that authority.
  const status = await page.evaluate(async () => {
    const r = await fetch("/api/v1/secrets", {
      method: "POST",
      credentials: "include",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ name: "member-mode-probe", value: "x", owner: "member@wardyn.local" }),
    });
    return r.status;
  });
  expect(status, "member mode did not bind server-side — the flag is decoration").toBe(403);

  await page.locator("header").getByRole("button").last().click();
  await page.getByRole("menuitem", { name: /exit member mode/i }).click();
  await expect.poll(async () => (await me(page)).operator, { timeout: 30_000 }).toBe(true);
});
