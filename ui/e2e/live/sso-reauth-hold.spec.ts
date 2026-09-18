/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

/*
 * THE LIVE AWS SSO WALK, PART THREE — the mid-run hold (field-report finding 4).
 *
 * Runs in the SAME invocation and against the SAME cluster as
 * ui/e2e/live/sso-member.spec.ts and ui/e2e/live/sso-member-recovery.spec.ts
 * (scripts/kind-sso-walk.sh runs `run-ui-e2e.sh sso-member sso-member-recovery
 * sso-reauth-hold`), LAST, because case K spends about ten minutes of wall clock
 * and because every case here makes its OWN capture — nothing after it should
 * depend on which session the member is holding.
 *
 * ── WHAT IS SIMULATED HERE, AND WHAT IS NOT ─────────────────────────────────
 * Corrected 2026-09-18 (W6-I SHOULD-3): the first version of this header said
 * the walk's injection "rides the cleartext lane" and that the production path
 * is "proven by the docker-gated test, whose fake serves TLS". Both are false.
 * The SDK CONNECTs, so the walk exercises the MITM lane — terminate, strip,
 * inject, re-originate — and `test/awsssofake` serves PLAIN HTTP
 * (`httptest.NewServer`); the docker-gated tests point the agent straight at it
 * with no proxy at all, deliberately, because what they measure is the SDK's
 * patience. Nothing in them touches mitm.go.
 *
 * What is SIMULATED is exactly one thing: the timeout BODY. Over plain HTTP the
 * sandbox's SDK still sees the fake's own 401 rather than the hold's sentence,
 * so case K is a SIMULATED check of that. The terminate → strip → inject →
 * re-originate path is pinned by internal/egress/proxy's own
 * TestMITMConnect_PlaintextOriginIsReachedThroughTheTunnel, which drives a real
 * CONNECT through a real proxy listener and a real TLS handshake against the
 * Wardyn leaf into a real plain-HTTP origin — and by this walk, end to end.
 *
 * What IS real here, and is proven nowhere else: a real member, a real k8s
 * sandbox, a real agent process, a real session retired at the portal mid-run,
 * the control plane raising a real approval row, the console showing it on the
 * cockpit, and the SAME run continuing after a real device-flow capture.
 *
 * ── THE TIMELINE IS THE FAKE'S TWO TTLs, AND THE WALK SETS THEM ─────────────
 * scripts/kind-sso-walk.sh exports AWSSSOFAKE_TOKEN_TTL=12m and
 * AWSSSOFAKE_ROLE_CRED_TTL=3m onto the fake's Deployment. Both numbers are
 * load-bearing and neither is arbitrary:
 *
 *   - the hold is reachable only when the INJECTOR re-resolves, i.e. inside
 *     injectRefreshMargin (5 min) of the blob's ExpiresAt. With a 12-minute
 *     token that window opens at T+7 and not one second earlier, which is why
 *     case K cannot be made shorter without changing the fake's TTL;
 *   - the SDK re-fetches role credentials only when the ones it holds have
 *     expired. Three minutes is short enough that a prompt typed at T+7.5
 *     forces a fetch, and long enough that the run's first turn does not spend
 *     its whole budget re-fetching.
 *
 * Self-skips without WARDYN_TEST_K8S=1, same as its two siblings.
 */

import { expect, test, type APIRequestContext, type Page } from "@playwright/test";
import { execFileSync } from "node:child_process";
import {
  REAUTH_ROW,
  REAUTH_HEADING,
  REAUTH_SIGNED_IN_TOAST,
  MODEL_ACCESS_BANNER,
  MODEL_ACCESS_RUN_DOOR,
  waitingReauth,
} from "../../src/app/components/wardyn/model-access-copy";
import { AGENTS } from "../../src/app/lib/workspace-providers-copy";
import {
  ADMIN_EMAIL,
  LOGIN_DONE,
  MEMBER_EMAIL,
  SANDBOX_UP,
  SEEN_URL,
  dexSignIn,
  makeMemberActionable,
  me,
  modelAccess,
  openLoginPane,
  runIDFromURL,
  seen,
  signInThroughPane,
} from "./helpers";

test.skip(process.env.WARDYN_TEST_K8S !== "1", "live cluster walk: set WARDYN_TEST_K8S=1 (scripts/kind-sso-walk.sh)");
test.describe.configure({ mode: "serial" });

/** The fake's runtime control, on the same server /_seen is on — the harness
 *  already reaches it through the walk's read-only port-forward, and the
 *  control endpoint takes no bearer (test/awsssofake/server.go's
 *  handleReauthControl). `after=1` retires the session on the NEXT refresh
 *  redemption, which is the only shape a real mid-run death has; `after=0` puts
 *  it back, so one walk can kill and restore without restarting the pod. */
const REAUTH_CONTROL_URL = SEEN_URL.replace("/_seen", "/_control/reauth");

async function setReauthAfter(n: number): Promise<void> {
  // FIRST through the walk's own read-only port-forward, which is there for
  // /_seen and costs nothing to reuse.
  try {
    const res = await fetch(`${REAUTH_CONTROL_URL}?after=${n}`, { method: "POST" });
    if (res.ok) return;
    throw new Error(`POST ${REAUTH_CONTROL_URL}?after=${n}: ${res.status}`);
  } catch {
    // …AND THEN THROUGH ONE OF OUR OWN, because two cases below RESTART the
    // fake to re-time its session, and a `kubectl port-forward` picks its pod
    // once: the walk's forward dies with that pod and never re-targets, so
    // every later control call gets ECONNREFUSED on 127.0.0.1. That is not a
    // flake, it is this file cutting its own lifeline — and the case that does
    // it needs the control endpoint precisely AFTER the restart.
    //
    // A short-lived forward on a port of its own, opened and closed inside one
    // shell, keeps the whole thing synchronous and leaves nothing running.
    execFileSync(
      "bash",
      [
        "-c",
        `set -e
         kubectl --context ${KUBE_CONTEXT} -n ${KUBE_RELEASE_NAMESPACE} port-forward svc/${KUBE_FAKE} ${CONTROL_PORT}:8090 >/dev/null 2>&1 &
         PF=$!; trap "kill $PF 2>/dev/null" EXIT
         for _ in $(seq 1 30); do curl -sf http://127.0.0.1:${CONTROL_PORT}/_seen >/dev/null 2>&1 && break; sleep 1; done
         curl -sf -X POST "http://127.0.0.1:${CONTROL_PORT}/_control/reauth?after=${n}" >/dev/null`,
      ],
      { encoding: "utf8", stdio: "pipe" },
    );
  }
}

/** A port for this file's own short-lived forwards — never the walk's 8390. */
const CONTROL_PORT = process.env.WARDYN_LIVE_FAKE_CONTROL_PORT || "8399";

/** The RELEASE namespace and deployment — the hold's timeout knob is boot env
 *  on wardynd, forwarded into every proxy sidecar (runner.ProxySidecarEnvKnobs),
 *  so a case that wants a short hold edits it there. Defaulted rather than
 *  exported by the walk, and overridable so a renamed install reds a case
 *  instead of making it vacuous. */
const KUBE_CONTEXT = process.env.WARDYN_LIVE_KUBE_CONTEXT || "kind-wardyn-quickstart";
const KUBE_RELEASE_NAMESPACE = process.env.WARDYN_LIVE_KUBE_RELEASE_NAMESPACE || "wardyn";
const KUBE_RELEASE = process.env.WARDYN_LIVE_KUBE_RELEASE || "wardyn";

function kubectl(...args: string[]): string {
  return execFileSync("kubectl", ["--context", KUBE_CONTEXT, ...args], { encoding: "utf8", stdio: "pipe" }).trim();
}

/** Set (or clear, with "") the hold's budget on the daemon and WAIT for the new
 *  pod to serve. `kubectl set env VAR-` removes the var; the sidecar then reads
 *  nothing and credentialReauthBudget() keeps its 600 s default. */
function setReauthTimeout(value: string): void {
  const arg = value === "" ? "WARDYN_CREDENTIAL_REAUTH_TIMEOUT-" : `WARDYN_CREDENTIAL_REAUTH_TIMEOUT=${value}`;
  kubectl("-n", KUBE_RELEASE_NAMESPACE, "set", "env", `deployment/${KUBE_RELEASE}`, arg);
  kubectl("-n", KUBE_RELEASE_NAMESPACE, "rollout", "status", `deployment/${KUBE_RELEASE}`, "--timeout=300s");
}

/** The fake's own Deployment — the two negatives below re-time the SESSION, not
 *  the hold, and the fake's TTLs are boot env on it. */
const KUBE_FAKE = process.env.WARDYN_LIVE_KUBE_FAKE || "wardyn-awsssofake";

/** Re-time the fake's session and WAIT for the new pod.
 *
 *  THE RESTART WIPES THE FAKE'S SESSION, and that is not a detail: its access
 *  and refresh tokens live in memory, so every stored blob's refresh token is
 *  unknown to the new pod and would be answered `invalid_grant` — "spent" —
 *  on the next renewal. Every caller therefore takes a FRESH capture
 *  immediately after, and the restore in its `finally` is followed by whatever
 *  the next case's own freshCapture() does. */
function setFakeSessionTTLs(tokenTTL: string, roleCredTTL: string): void {
  kubectl(
    "-n",
    KUBE_RELEASE_NAMESPACE,
    "set",
    "env",
    `deployment/${KUBE_FAKE}`,
    `AWSSSOFAKE_TOKEN_TTL=${tokenTTL}`,
    `AWSSSOFAKE_ROLE_CRED_TTL=${roleCredTTL}`,
  );
  kubectl("-n", KUBE_RELEASE_NAMESPACE, "rollout", "status", `deployment/${KUBE_FAKE}`, "--timeout=180s");
}

/** The walk's own TTLs, restored (scripts/kind-sso-walk.sh sets these). */
const WALK_TOKEN_TTL = process.env.WARDYN_KIND_SSO_TOKEN_TTL || "12m";
const WALK_ROLE_CRED_TTL = process.env.WARDYN_KIND_SSO_ROLE_CRED_TTL || "3m";

/** The approval states, in the WIRE's own spelling — UPPERCASE
 *  (`internal/types/types.go`'s ApprovalState constants). Named here because
 *  walk-6 spent eight minutes raising a real hold and then failed on
 *  `toBe("pending")` against a row that said `PENDING`: the mechanism was
 *  perfect and the assertion was shouting the wrong case. One definition, so
 *  the next reader cannot re-derive it wrong. */
const APPROVAL = { pending: "PENDING", approved: "APPROVED", cancelled: "CANCELLED" } as const;

type RunRow = { id: string; state?: string; task?: string; created_at?: string };
type ApprovalRow = { id: string; kind?: string; state?: string; requested_scope?: Record<string, unknown> };
/** The audit row as the WIRE spells it (types.AuditEvent): `time`, not
 *  `created_at`; `outcome`, not `result`. Both were wrong in the first draft —
 *  the timestamp one made every "after the hold" filter empty (caught by the
 *  guard below rather than passing vacuously), and the outcome one would have
 *  made case J's search for the run.create FAILURE row match nothing at all. */
type AuditRow = { id?: string; action?: string; outcome?: string; time?: string; data?: Record<string, unknown> };

async function runRow(page: Page, id: string): Promise<RunRow> {
  return page.evaluate(async (rid: string) => {
    const r = await fetch(`/api/v1/runs/${rid}`, { credentials: "include" });
    if (!r.ok) throw new Error(`GET /runs/${rid}: ${r.status}`);
    return (await r.json()) as RunRow;
  }, id);
}

/** The run's own approvals, as the run's OWNER reads them. */
async function approvalsFor(page: Page, id: string): Promise<ApprovalRow[]> {
  return page.evaluate(async (rid: string) => {
    const r = await fetch(`/api/v1/approvals?run_id=${encodeURIComponent(rid)}`, { credentials: "include" });
    if (!r.ok) throw new Error(`GET /approvals?run_id=: ${r.status}`);
    return (await r.json()) as ApprovalRow[];
  }, id);
}

/** The run's own audit trail. A member may read it for a run they created
 *  (internal/api/audit.go's auditScope); a foreign run answers an empty list
 *  rather than a 403, so an empty answer here is never proof of anything and
 *  every assertion below is about a row that must be PRESENT or ABSENT among
 *  rows we know are this run's. */
async function auditFor(page: Page, id: string): Promise<AuditRow[]> {
  return page.evaluate(async (rid: string) => {
    const r = await fetch(`/api/v1/audit?run_id=${encodeURIComponent(rid)}`, { credentials: "include" });
    if (!r.ok) throw new Error(`GET /audit?run_id=: ${r.status}`);
    const body = (await r.json()) as AuditRow[] | { items?: AuditRow[] };
    return Array.isArray(body) ? body : (body.items ?? []);
  }, id);
}

/** A FRESH capture, and the moment it landed — every case's T.
 *
 *  makeMemberActionable() FLIPS the roster pin, so it is called only when the
 *  member is `live`: on an already-contradicted member it would HEAL them and
 *  take the CTA away. */
async function freshCapture(page: Page, request: APIRequestContext): Promise<number> {
  if ((await modelAccess(page)).state === "live") await makeMemberActionable(request);
  await expect.poll(async () => (await modelAccess(page)).state, { timeout: 120_000 }).not.toBe("live");
  await signInThroughPane(page, openLoginPane);
  await expect.poll(async () => (await modelAccess(page)).state, { timeout: LOGIN_DONE }).toBe("live");
  return Date.now();
}

/** The ADMIN's own AWS sign-in, which is NOT on /setup — and NEVER a bare
 *  `page.goto("/providers")`.
 *
 *  App.tsx's RequireSetup bounces the FIRST gated-route render of every full
 *  document load into /setup while any setup check grades fail or warn, which a
 *  fresh kind install always does, and only ONCE per load. So navigate
 *  CLIENT-SIDE (pushState + popstate, what a <NavLink> click does) and let the
 *  page say when it took: if the one bounce landed on top of this navigation,
 *  the retry cannot be bounced again. Same reasoning, same shape, as
 *  sso-member-recovery.spec.ts's gotoAgentsTab — 0.7.5's first green-looking
 *  walk sat thirty minutes on the welcome page for exactly this.
 *
 *  The button itself is gated on MODEL_ACCESS_ACTIONABLE (agents-tab.tsx): a
 *  LIVE admin is offered no sign-in at all, which is correct and is why every
 *  caller below checks the state first rather than assuming the control. */
async function openAdminLoginPane(page: Page): Promise<void> {
  await page.goto("/runs");
  await expect(async () => {
    if (!/\/providers$/.test(page.url())) {
      await page.evaluate((path) => {
        window.history.pushState({}, "", path);
        window.dispatchEvent(new PopStateEvent("popstate"));
      }, "/providers");
    }
    await expect(page.getByRole("button", { name: AGENTS.AGENTS_TITLE })).toBeVisible({ timeout: 5_000 });
  }).toPass({ timeout: 90_000 });
  await page.getByRole("button", { name: AGENTS.AGENTS_TITLE }).click();
  await page.getByRole("button", { name: AGENTS.SIGN_IN_AWS }).first().click();
  const start = page.getByRole("button", { name: "Start login" });
  if (await start.isVisible().catch(() => false)) await start.click();
}

/** Case H's recipe, which this file needs for the one reason an autonomous run
 *  cannot serve: the hold is unreachable before T+7 (see the header), and an
 *  autonomous `claude -p` run is finished long before that. An INTERACTIVE run
 *  is a live agent process this test can make a model call with, on demand, at
 *  the minute the injector's window is open.
 *
 *  The step list is the W0 spike's, pre-declared (see the recovery file's case
 *  H): with `hasCompletedOnboarding` seeded the only screen left is the
 *  workspace-trust one, whose default option is already the accepting one, so
 *  ONE bare Enter clears it. */
async function startInteractiveRun(page: Page, title: string): Promise<{ id: string; screen: ReturnType<Page["locator"]> }> {
  await page.goto("/runs/new");
  await page.getByRole("combobox", { name: "Title" }).fill(title);
  await page.getByRole("radio", { name: /^Interactive/ }).click();
  await page.getByRole("button", { name: /^Launch/ }).click();
  await page.getByRole("button", { name: "Open run" }).click();

  const screen = page.locator(".xterm-screen").first();
  await expect(screen).toBeVisible({ timeout: SANDBOX_UP });
  await expect
    .poll(async () => (await screen.innerText({ timeout: 1_000 }).catch(() => "")), { timeout: SANDBOX_UP })
    .toContain("Accessing workspace:");
  await screen.click();
  await page.keyboard.press("Enter");
  await expect
    .poll(async () => (await screen.innerText({ timeout: 1_000 }).catch(() => "")), { timeout: 120_000 })
    .toContain("Amazon Bedrock");
  return { id: runIDFromURL(page), screen };
}

/** Type one prompt into a live CLI and send it. The ONE place this file types:
 *  it is the agent's own prompt, never a sign-in pane (the recovery file's case
 *  C explains why that distinction is a negative control and not a detail). */
async function askTheModel(page: Page, screen: ReturnType<Page["locator"]>, prompt: string): Promise<void> {
  await screen.click();
  await page.keyboard.type(prompt);
  await page.keyboard.press("Enter");
}

/** Sleep until `at` ms since the epoch — the walk's clock, not a poll. Used
 *  only where the product's own timing is the thing under test (the injector's
 *  5-minute re-resolve margin, dispatch's 10-minute refresh skew). */
async function waitUntil(page: Page, at: number, why: string): Promise<void> {
  const ms = at - Date.now();
  if (ms <= 0) return;
  test.info().annotations.push({ type: "wait", description: `${Math.round(ms / 1000)}s — ${why}` });
  await page.waitForTimeout(ms);
}

const MINUTE = 60_000;

/** Case K's run id, handed to K(resume). Module state, which serial mode makes
 *  honest: K(resume) cannot run unless K passed, and "the SAME run" is the
 *  assertion, so the id must cross the test boundary rather than be looked up
 *  again by a heuristic that could find a different row. */
let heldRunID = "";
/** WHEN the hold opened, as the RAISE ROW itself stamps it, and the grant ids
 *  the run had at that moment. K(resume)'s "no new run, no new grant" claims
 *  are about what arrived after that instant.
 *
 *  A TIMESTAMP, not a row count: the first version sliced the list by the
 *  difference in length and assumed the newest rows were at the front. They are
 *  not — GET /audit?run_id= answers oldest-first — so it read the run's own
 *  creation rows and reported "a second run.create landed on the held run"
 *  about the FIRST one. A slice by position cannot state this claim; an instant
 *  can. */
let holdOpenedAt = 0;
let grantIDsAtHold: string[] = [];

// ── K — a member's session dies mid-run and the run is HELD, not killed ─────

test("K (credential-reauth-hold): a session retired mid-run HOLDS the model call and asks its owner to sign in", async ({
  page,
  request,
}) => {
  // ~10 minutes of wall clock, and the header says why it cannot be shorter.
  test.setTimeout(22 * MINUTE);

  await dexSignIn(page, MEMBER_EMAIL);
  const T = await freshCapture(page, request);

  // T+1 — a live agent process on the member's own session. Launched BEFORE
  // T+2 deliberately: dispatch renews any token within awsSSORefreshSkew (10
  // min) of its expiry, so a launch after T+2 would mint a NEW 12-minute token
  // and push the injector's window out with it. (That renewal is case J's
  // subject, from the other side.)
  const before = (await seen()).bedrock_calls;
  // Measured at the LAUNCH, not after the first turn: what must happen before
  // T+2 is the DISPATCH, and a slow sandbox boot afterwards changes nothing
  // about which token this run was dispatched on.
  const launchedAt = Date.now();
  expect(launchedAt - T, "the run must be launched inside the first two minutes — see the comment above").toBeLessThan(
    2 * MINUTE,
  );
  const run = await startInteractiveRun(page, "a run whose session dies under it");
  heldRunID = run.id;
  await askTheModel(page, run.screen, "Reply with the single word: ready.");
  await expect.poll(async () => (await seen()).bedrock_calls, { timeout: 180_000 }).toBeGreaterThan(before);

  // T+4 — the session dies AT THE PORTAL, which is the only place a real one
  // dies. The fake answers invalid_grant on the next refresh redemption, the
  // shape awsSSOErrorIsSpent reads as "spent".
  await waitUntil(page, T + 4 * MINUTE, "the fake retires the session at T+4 (canon timeline)");
  await setReauthAfter(1);

  try {
    // T+7.5 — one prompt, and it is the whole mechanism: the role credentials
    // from the first turn expired at ~T+4.5, so this turn must fetch new ones;
    // the proxy injects the access token and the injector re-resolves, because
    // T+7.5 is inside injectRefreshMargin (5 min) of the 12-minute token's
    // T+12 expiry; the refresh redemption gets invalid_grant; the session is
    // marked spent and the hold opens.
    await waitUntil(page, T + 7.5 * MINUTE, "the injector's re-resolve window opens at T+7 (12m token, 5m margin)");
    await askTheModel(page, run.screen, "Reply with the single word: again.");

    // The request, on the wire first — the console's rendering of it is the
    // NEXT assertion, not this one.
    await expect
      .poll(async () => (await approvalsFor(page, run.id)).filter((a) => a.kind === "credential_reauth").length, {
        timeout: 4 * MINUTE,
      })
      .toBe(1);
    const held = (await approvalsFor(page, run.id)).find((a) => a.kind === "credential_reauth")!;
    expect(held.state, "a raised re-auth request must be PENDING").toBe(APPROVAL.pending);

    // THE RUN IS NOT DEAD. This is the whole of finding 4: the model call is
    // parked, the run is not.
    expect((await runRow(page, run.id)).state, "the run was killed instead of held").toBe("RUNNING");

    // The cockpit, where the person already is.
    await page.goto(`/runs/${run.id}`);
    await expect(page.getByText(waitingReauth(1))).toBeVisible({ timeout: 2 * MINUTE });
    await expect(page.getByText(REAUTH_HEADING)).toBeVisible();
    const row = page.getByTestId("live-approval-row").filter({ hasText: REAUTH_ROW.label });
    await expect(row).toBeVisible();
    await expect(row.getByText(REAUTH_ROW.hint)).toBeVisible();
    // NO DECISION. The kind is resolved by signing in; an Approve or a Deny
    // would change nothing, and offering either is the defect the kind exists
    // to avoid (credentialReauthNotDecidableBody says the same on the wire).
    await expect(row.getByRole("button", { name: /^Approve/i })).toHaveCount(0);
    await expect(row.getByRole("button", { name: /^Deny/i })).toHaveCount(0);
    await expect(row.getByRole("button", { name: REAUTH_ROW.ariaLabel })).toBeVisible();

    // The audit row is the only place the RAISE explains itself, and it carries
    // whose credential it was and which lane raised it.
    const auditAtHold = await auditFor(page, run.id);
    const raised = auditAtHold.find((e) => e.action === "credential.reauth.requested");
    expect(raised, "no credential.reauth.requested row on the held run").toBeTruthy();
    holdOpenedAt = Date.parse(raised?.time ?? "");
    expect(Number.isFinite(holdOpenedAt), "the raise row carries no readable timestamp").toBe(true);
    grantIDsAtHold = auditAtHold
      .filter((e) => e.action === "credential.mint")
      .map((e) => String((e.data ?? {}).grant_id ?? ""));
    expect(String(raised?.data?.credential_source)).toBe("per_user");
    expect(String(raised?.data?.owner), "the hold names the MEMBER as the owner").toBe((await me(page)).principal);
    expect(String(raised?.data?.provider)).toBe("aws");

    // ── I2 (negative): a capture by the WRONG user does not resolve it ──────
    // The admin signing in mints a session in the ADMIN's own namespace. The
    // member's run keeps waiting, because the hold is about the run's own
    // owner's credential and nothing else.
    const adminPage = await page.context().browser()!.newPage();
    try {
      await dexSignIn(adminPage, ADMIN_EMAIL);
      // THE ADMIN DISCONNECTS THEIR OWN CREDENTIAL, and that is the only lever
      // that makes them actionable deterministically here.
      //
      // The obvious one — the roster pin — is forbidden twice over. A hold must
      // never move the roster: a change mid-run is precisely the I3 scope-drift
      // refusal (credentialReauthScopeChangedRefusal), so flipping the pin to
      // give the admin a CTA would destroy the very hold this case is testing.
      // And it does not even work: the pin OSCILLATES between the fixture's two
      // valid pairs, so whether a flip leaves the ADMIN contradicted or matching
      // is a question of parity with whichever pair they last captured under —
      // walk-6 left them contradicted, walk-7 left them `live`, and this case
      // waited sixty seconds for a button agents-tab.tsx correctly refuses to a
      // live admin.
      //
      // DELETE /setup/harness-credential/aws is operator-only AND scoped to the
      // CALLER's own subject (harnesscred.go's handleHarnessDisconnect), so the
      // admin can only ever delete their own — the member's capture, the hold
      // and the roster are all untouched. It is the same disconnect the Agents
      // tab offers, and it leaves the admin `not_configured`: actionable, with
      // a CTA, every time.
      const disconnect = await adminPage.evaluate(async () => {
        const r = await fetch("/api/v1/setup/harness-credential/aws", {
          method: "DELETE",
          credentials: "include",
        });
        return r.status;
      });
      expect(disconnect, "the admin could not disconnect their own AWS credential").toBe(200);
      await expect
        .poll(async () => (await modelAccess(adminPage)).state, { timeout: 60_000 })
        .toBe("not_configured");
      await signInThroughPane(adminPage, openAdminLoginPane);
      // …and the member's request is exactly where it was. Read twice, a poll
      // apart: "still pending" measured once is a snapshot, and the resolve
      // path this denies runs on the proxy's own 2 s poll.
      for (let i = 0; i < 3; i++) {
        const now = (await approvalsFor(page, run.id)).find((a) => a.kind === "credential_reauth");
        expect(now?.state, "the ADMIN's capture resolved the MEMBER's hold (I2)").toBe(APPROVAL.pending);
        await page.waitForTimeout(2_000);
      }
    } finally {
      await adminPage.close();
    }
  } finally {
    // The session control is GLOBAL to the fake, so it never outlives the case
    // that set it: every later refresh redemption would otherwise fail too.
    await setReauthAfter(0);
  }
});

// ── K(resume) — the SAME run, on the new session ────────────────────────────

test("K(resume) (credential-reauth-hold): the member signs in and the SAME run continues", async ({ page }) => {
  test.setTimeout(15 * MINUTE);
  expect(heldRunID, "case K did not leave a held run").not.toBe("");

  await dexSignIn(page, MEMBER_EMAIL);
  await page.goto(`/runs/${heldRunID}`);
  const row = page.getByTestId("live-approval-row").filter({ hasText: REAUTH_ROW.label });
  await expect(row).toBeVisible({ timeout: 2 * MINUTE });

  // The door, opened from the row itself — queried by the row's OWN accessible
  // name, because the cockpit can carry more than one sign-in control and three
  // identical names is three controls a screen-reader user cannot tell apart.
  await signInThroughPane(page, async (p) => {
    await p.getByRole("button", { name: REAUTH_ROW.ariaLabel }).click();
    await expect(p.getByRole("heading", { name: MODEL_ACCESS_BANNER.DIALOG_TITLE })).toBeVisible({ timeout: 60_000 });
    await expect(p.getByTestId("harness-login-pane")).toBeVisible({ timeout: 60_000 });
    const start = p.getByRole("button", { name: "Start login" });
    if (await start.isVisible().catch(() => false)) await start.click();
  });

  // The row leaves PENDING as APPROVED — resolved by a sign-in, never by a
  // decision — and the toast is the person's only "it worked" moment, since the
  // row itself vanishes on the next poll.
  await expect
    .poll(async () => (await approvalsFor(page, heldRunID)).find((a) => a.kind === "credential_reauth")?.state, {
      timeout: 4 * MINUTE,
    })
    .toBe(APPROVAL.approved);
  await expect(page.getByText(REAUTH_SIGNED_IN_TOAST)).toBeVisible({ timeout: 60_000 });
  await expect(page.getByText(waitingReauth(1))).toHaveCount(0, { timeout: 2 * MINUTE });

  // THE SAME RUN, and every clause of that.
  expect(runIDFromURL(page), "the console navigated to a different run").toBe(heldRunID);
  const after = await runRow(page, heldRunID);
  expect(after.state, "the held run did not survive its own hold").toBe("RUNNING");

  // …and the parked call really did go through: the agent's turn lands.
  const seenAfter = await seen();
  expect(seenAfter.bedrock_calls, "no model call completed after the hold released").toBeGreaterThan(0);

  // NO NEW RUN, NO NEW GRANT, NO SECOND DISPATCH — measured as rows stamped
  // AFTER the raise row, which is the only honest form of "nothing new
  // happened": the run's own create and grant rows are all before it.
  const now = await auditFor(page, heldRunID);
  const fresh = now.filter((e) => Date.parse(e.time ?? "") > holdOpenedAt);
  const freshActions = fresh.map((e) => e.action ?? "");
  expect(fresh.length, "no audit row at all arrived after the hold opened — the filter is wrong").toBeGreaterThan(0);
  expect(freshActions, "a second run.create landed on the held run").not.toContain("run.create");
  // A NEW GRANT is the claim, not a new ROW. The final resolve re-resolves the
  // injection exactly once when the answer arrives (credhold.go's own comment),
  // and that write is allowed to audit — what must not happen is a grant id
  // this run did not already hold, which is what "no new grant, no policy
  // relaxation" means.
  const freshGrantIDs = fresh
    .filter((e) => e.action === "credential.mint")
    .map((e) => String((e.data ?? {}).grant_id ?? ""));
  expect(
    freshGrantIDs.filter((g) => !grantIDsAtHold.includes(g)),
    "the resume minted a grant this run did not already hold",
  ).toEqual([]);
  expect(freshActions, "the resolve is audited").toContain("credential.reauth.resolved");

  // …and the console is back to an ordinary cockpit: no strip, because the
  // member's session is live again.
  await expect(page.getByText(MODEL_ACCESS_BANNER.EXPIRED)).toHaveCount(0);
  await expect(page.getByText(MODEL_ACCESS_BANNER.NOT_SIGNED_IN)).toHaveCount(0);
});

// ── J — the run door, on a run dispatch refused over the credential ─────────

test("J (run-credential-door): a run refused over the model credential carries the sign-in where the person is", async ({
  page,
  request,
}) => {
  test.setTimeout(15 * MINUTE);

  // THE REFUSAL IS FORCED, not waited for, and the lever is dispatch's own
  // renewal. `needsRefresh` is true for any token within awsSSORefreshSkew (10
  // min) of its expiry, so from T+2 onward EVERY dispatch of this 12-minute
  // token renews it first. With the session retired at the portal, that renewal
  // is the thing that fails — and it fails at DISPATCH, where the run has
  // already been created, which is exactly the shape the run door exists for.
  //
  // Create still passes: the spent mark is written by the dispatch attempt, so
  // at create time model_access still grades live. That ordering is the case.
  await dexSignIn(page, MEMBER_EMAIL);
  const T = await freshCapture(page, request);
  await setReauthAfter(1);

  try {
    await waitUntil(page, T + 2.2 * MINUTE, "dispatch renews inside awsSSORefreshSkew (10 min) of a 12-minute token");

    await page.goto("/runs/new");
    await page.getByRole("combobox", { name: "Title" }).fill("a run whose credential cannot be renewed");
    await page.getByRole("radio", { name: /^Autonomous/ }).click();
    await page.locator("#nr-task").fill("Reply with the single word: ready.");
    await page.getByRole("button", { name: /^Launch/ }).click();
    // The 201 is the point: the console offers the run, and the refusal is on
    // the run's own page rather than in the launcher's error line.
    await page.getByRole("button", { name: "Open run" }).click();
    const runID = runIDFromURL(page);

    await expect.poll(async () => (await runRow(page, runID)).state, { timeout: 5 * MINUTE }).toBe("FAILED");

    // The server's CLASS, on the wire: the console grades the ending from this
    // key, never from the sentence.
    const trail = await auditFor(page, runID);
    const refusal = trail.find((e) => e.action === "run.create" && e.outcome === "failure");
    expect(refusal, "no run.create failure row on the refused run").toBeTruthy();
    expect(String(refusal?.data?.reason), "the refusal is not classed as a model-credential one").toBe(
      "model_credential",
    );
    expect(String(refusal?.data?.mechanism)).toBe("bedrock_sso");

    // THE SERVER'S OWN SENTENCE, read off the audit row and then found on the
    // page — never retyped here. Asserting a literal would pin a second
    // spelling of a string lane run-credential-door owns.
    const sentence = String(refusal?.data?.error ?? "");
    expect(sentence, "the refusal row carries no sentence").not.toBe("");
    // SCOPED TO THE FAILURE BLOCK, because the sentence is on this page TWICE —
    // the run header's summary line carries it too (0.7.6's header states a
    // terminal run's reason), and an unscoped locator is a strict-mode
    // violation rather than a passing assertion. The failure block is the
    // surface this case is about: it is the one that carries the door.
    const failure = page.getByTestId("run-failure-block");
    await expect(failure.getByText(sentence.slice(0, 80))).toBeVisible({ timeout: 2 * MINUTE });
    // …and the header says it as well, which is the other half of "the refusal
    // is where the person is" — asserted, not merely tolerated.
    await expect(page.getByTestId("run-summary-header").getByText(sentence.slice(0, 40))).toBeVisible();

    // …and the door, under its own accessible name — distinct from the strip's
    // and the rail's, because this page can carry more than one.
    const door = failure.getByRole("button", { name: MODEL_ACCESS_RUN_DOOR.SIGN_IN_ARIA });
    await expect(door).toBeVisible({ timeout: 2 * MINUTE });
    await expect(failure.getByText(MODEL_ACCESS_RUN_DOOR.NOTE)).toBeVisible();
    // The claim it makes about itself is the one the block must not break: this
    // run stays failed. No relaunch happens here.
    await door.click();
    await expect(page.getByRole("heading", { name: MODEL_ACCESS_BANNER.DIALOG_TITLE })).toBeVisible({ timeout: 60_000 });
    await expect(page.getByTestId("harness-login-pane")).toBeVisible({ timeout: 60_000 });
    await page.keyboard.press("Escape");
    expect((await runRow(page, runID)).state, "opening the door restarted the run").toBe("FAILED");
  } finally {
    await setReauthAfter(0);
  }
});

// ── the negative every strip case needs: an admin with a LIVE credential ────

test("negative (model-access-banner): an admin whose own session is live sees no strip anywhere", async ({ page }) => {
  // The other half of case I. The strip is per-PERSON, so the proof that it is
  // not merely always-on is a second principal, on the same install, at the
  // same moment, with a live session of their own — which the admin has by now
  // (the recovery file's case F signs them in, and case K's I2 does it again).
  await dexSignIn(page, ADMIN_EMAIL);
  expect((await me(page)).operator).toBe(true);
  // SELF-SUFFICIENT, because "the admin is live" is not something this file can
  // inherit: every case above flips the roster pin to make the MEMBER
  // actionable, and the pin is one field on one row — an admin whose capture
  // was minted under the other pair grades `expired_signin` too. So reach the
  // state this case is about rather than asserting somebody else left it.
  if ((await modelAccess(page)).state !== "live") {
    await signInThroughPane(page, openAdminLoginPane);
  }
  await expect.poll(async () => (await modelAccess(page)).state, { timeout: LOGIN_DONE }).toBe("live");

  for (const path of ["/runs", "/workspaces", "/approvals"]) {
    await page.goto(path);
    // The shell's live region is EAGER and always mounted, so its presence is
    // not the assertion — its emptiness of every strip sentence is.
    await expect(page.getByRole("status").first()).toBeAttached({ timeout: 60_000 });
    await expect(page.getByText(MODEL_ACCESS_BANNER.NOT_SIGNED_IN)).toHaveCount(0);
    await expect(page.getByText(MODEL_ACCESS_BANNER.EXPIRED)).toHaveCount(0);
    await expect(page.getByRole("button", { name: AGENTS.SIGN_IN_AWS, exact: true })).toHaveCount(0);
  }
});

// ── the two hold negatives, on a deliberately SHORT session ─────────────────
//
// Case K's timeline cannot be shortened: with the walk's 12-minute token the
// injector's re-resolve window does not open until T+7. These two cases are not
// about WHEN the hold opens, though — they are about how it ENDS — so they
// re-time the fake's session to two minutes, which opens that window
// immediately and puts a real hold on screen inside ~90 seconds. Everything
// else is the production path: a real sandbox, a real injector, a real parked
// GetRoleCredentials, the real proxy budget.
//
// Both restore the walk's own TTLs (and the fake's session control) in a
// `finally`, because every case after them signs in again through the same fake.

/** A real hold, fast: short session, live run, session retired, one prompt.
 *  Returns the run once its `credential_reauth` row is PENDING. */
async function fastHold(
  page: Page,
  request: APIRequestContext,
  title: string,
): Promise<{ id: string; screen: ReturnType<Page["locator"]> }> {
  await freshCapture(page, request);
  const run = await startInteractiveRun(page, title);
  // Flip AFTER the run is up: dispatch renews the token on the way in (the
  // 10-minute skew covers a two-minute one), and that renewal must succeed or
  // the run never starts — which would be case J's shape, not this one.
  await setReauthAfter(1);
  await askTheModel(page, run.screen, "Reply with the single word: ready.");
  await expect
    .poll(async () => (await approvalsFor(page, run.id)).filter((a) => a.kind === "credential_reauth").length, {
      timeout: 5 * MINUTE,
    })
    .toBe(1);
  return run;
}

test("negative (credential-reauth-hold): a hold nobody answers times out, and the request stays open", async ({
  page,
  request,
}) => {
  test.setTimeout(20 * MINUTE);
  // The knob is read by the PROXY SIDECAR, whose environment is authored at
  // DISPATCH (runner.ProxySidecarEnvKnobs) — so it has to be on the daemon
  // before the run below is launched, not merely before the hold opens.
  setReauthTimeout("30s");
  setFakeSessionTTLs("2m", "30s");
  try {
    await dexSignIn(page, MEMBER_EMAIL);
    const run = await fastHold(page, request, "a hold nobody answers");

    // THE DECISION IS THE ASSERTION. A spent hold is not the same fact as a
    // credential that could not be refreshed — one of them is a person's to
    // fix — so the proxy gives it its own rule_source, and that is what an
    // operator greps for.
    await expect
      .poll(
        async () =>
          (await auditFor(page, run.id)).some((e) =>
            String((e.data ?? {}).rule_source ?? "").includes("credential:reauth-timeout"),
          ),
        { timeout: 5 * MINUTE },
      )
      .toBe(true);

    // …and NOTHING ELSE WENT AWAY. The request stays open — signing in still
    // repairs the next call — the run is still the run, and the console still
    // says so. A timeout that quietly closed the request would leave the person
    // with nothing to act on and no way to know.
    const still = (await approvalsFor(page, run.id)).find((a) => a.kind === "credential_reauth");
    expect(still?.state, "the timeout closed the sign-in request").toBe(APPROVAL.pending);
    expect((await runRow(page, run.id)).state, "the timeout killed the run").toBe("RUNNING");
    await page.goto(`/runs/${run.id}`);
    await expect(page.getByTestId("live-approval-row").filter({ hasText: REAUTH_ROW.label })).toBeVisible({
      timeout: 2 * MINUTE,
    });
  } finally {
    await setReauthAfter(0);
    setFakeSessionTTLs(WALK_TOKEN_TTL, WALK_ROLE_CRED_TTL);
    setReauthTimeout("");
  }
});

test("negative (credential-reauth-hold): killing a held run cancels its sign-in request", async ({ page, request }) => {
  test.setTimeout(20 * MINUTE);
  setFakeSessionTTLs("2m", "30s");
  try {
    await dexSignIn(page, MEMBER_EMAIL);
    const run = await fastHold(page, request, "a held run somebody kills");

    // The person gives up on the run rather than signing in. The request must
    // go with it: a PENDING row on a dead run is a sign-in that repairs
    // nothing, and the sidecar's hold has to end on the next poll rather than
    // sit out its whole budget.
    await page.evaluate(async (id: string) => {
      const r = await fetch(`/api/v1/runs/${id}/kill`, { method: "POST", credentials: "include" });
      if (!r.ok && r.status !== 202) throw new Error(`POST /runs/${id}/kill: ${r.status}`);
    }, run.id);

    await expect
      .poll(async () => (await approvalsFor(page, run.id)).find((a) => a.kind === "credential_reauth")?.state, {
        timeout: 3 * MINUTE,
      })
      .toBe(APPROVAL.cancelled);
    await expect.poll(async () => (await runRow(page, run.id)).state, { timeout: 3 * MINUTE }).not.toBe("RUNNING");
    // The cockpit stops asking for a sign-in that would now repair nothing.
    await page.goto(`/runs/${run.id}`);
    await expect(page.getByText(waitingReauth(1))).toHaveCount(0, { timeout: 2 * MINUTE });
  } finally {
    await setReauthAfter(0);
    setFakeSessionTTLs(WALK_TOKEN_TTL, WALK_ROLE_CRED_TTL);
  }
});
