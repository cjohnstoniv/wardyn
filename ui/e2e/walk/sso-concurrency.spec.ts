/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

/*
 * THE LIVE AWS SSO WALK, PART THREE — two people at once (#697).
 *
 * Runs in the same invocation and against the same cluster as sso-member and
 * sso-member-recovery (scripts/kind-sso-walk.sh), after them. It inherits the
 * roster pin and the member's stored capture from wherever those files
 * stopped, which may be mid-flip (a case that moved the pin and never landed
 * its capture), so A puts the walk's own pin back and makes sure the member is
 * offered a sign-in under it (actionableUnderWalkPin), and B flips until they
 * are (ensureActionable); both name the walk state when they cannot. Every run
 * observation is a delta over this file's own runs.
 *
 * What it proves that one principal at a time cannot:
 *
 *   A. two members signing in to AWS at the same moment each end up holding
 *      their OWN capture — neither sign-in rotates the other's session away;
 *   C. three runs each, taking turns between the two members while both
 *      sessions are live, spend only their OWNER's session: every
 *      GetRoleCredentials a run's proxy made, and every bedrock call it signed,
 *      lands under one fake session per person, and the two people's sessions
 *      never share a run. Turns, not all at once: the walk's node is one
 *      4-vCPU kind node, which holds one agent run (2 vCPU plus its proxy);
 *      a run that cannot schedule fails its sandbox rather than waiting;
 *   B. one member opening two sign-ins at once gets ONE live sign-in sandbox:
 *      the per-person lock (harnesscred_supersede.go) ends the older launch,
 *      and the capture that lands is the other one's.
 *
 * Run order is A, B, C. B flips the pin to make the member actionable, so C
 * first puts the walk pin back and signs the member in under it again.
 *
 * HOW A RUN IS TIED TO A SESSION. The fake (test/awsssofake) gives every
 * sign-in its own session and lists, per session, the peers that called
 * GetRoleCredentials and the peers whose bedrock calls were signed with that
 * session's key or, on this walk, carried its bearer (/_seen `sessions`): the
 * stub shares the portal's host, which the proxy terminates to set the SSO
 * bearer, so a bedrock call arrives with the member's session token instead
 * of its SigV4 signature. On this cluster the peer is the run's own
 * proxy pod — the sandbox's only way out — so a run is its proxy pod's IP,
 * read with kubectl while the run is alive. Nothing here needs to know which
 * session is whose in advance: the assertion is that each person's runs sit
 * under exactly one session, and that it is not the other person's.
 *
 * NOT covered, named: the governance quota under this load (the walk seeds no
 * per-member max_concurrent_runs to hit), and the live Entra/CloudTrail
 * variant (owner hardware).
 */

import { execFileSync } from "node:child_process";
import { expect, test, type APIRequestContext, type Browser, type Page } from "@playwright/test";
import { TERMINAL_RUN_STATES, type RunState } from "../../src/app/lib/types/runs";
import {
  ADMIN_TOKEN,
  LOGIN_DONE,
  MEMBER_EMAIL,
  OTHER_ACCOUNT,
  OTHER_ROLE,
  SANDBOX_UP,
  SSO_START_URL,
  dexSignIn,
  makeMemberActionable,
  modelAccess,
  openLoginPane,
  ownAWSRow,
  putRoster,
  seen,
  signInThroughPane,
} from "./helpers";

test.skip(process.env.WARDYN_TEST_K8S !== "1", "live cluster walk: set WARDYN_TEST_K8S=1 (scripts/kind-sso-walk.sh)");
test.describe.configure({ mode: "serial" });

const MEMBER2_EMAIL = "member2@wardyn.local";
const RUNS_PER_MEMBER = 3;
const KUBE_CONTEXT = process.env.WARDYN_WALK_KUBE_CONTEXT || "kind-wardyn-quickstart";
/** The RUNS namespace: a run's proxy pod lands in k8s.runsNamespace. */
const KUBE_RUNS_NAMESPACE = process.env.WARDYN_WALK_KUBE_NAMESPACE || "wardyn-runs";

/** A signed-in page per member, each in its own browser context (its own cookie jar). */
async function signedIn(browser: Browser, email: string): Promise<Page> {
  const page = await (await browser.newContext()).newPage();
  await dexSignIn(page, email);
  return page;
}

/** The IP of runID's proxy pod, or "" while it has none (not scheduled yet, or gone). */
function proxyPodIP(runID: string): string {
  try {
    return execFileSync(
      "kubectl",
      [
        "--context", KUBE_CONTEXT, "-n", KUBE_RUNS_NAMESPACE, "get", "pods",
        "-l", `wardyn.run-id=${runID},wardyn.component=proxy`,
        "-o", "jsonpath={.items[0].status.podIP}",
      ],
      { encoding: "utf8", stdio: "pipe" },
    ).trim();
  } catch {
    return "";
  }
}

/** Launch an autonomous run from the New Run form and return its id as soon
 *  as the launch has navigated — without waiting for it to run, so a member's
 *  three runs are in flight together. */
async function launchRun(page: Page, title: string): Promise<string> {
  await page.goto("/runs/new");
  await page.getByRole("combobox", { name: "Title" }).fill(title);
  await page.getByRole("radio", { name: /^Autonomous/ }).click();
  await page.locator("#nr-task").fill("Reply with the single word: ready.");
  await page.getByRole("button", { name: /^Launch/ }).click();
  await expect(page).toHaveURL(/\/runs\/[0-9a-f-]{36}$/, { timeout: SANDBOX_UP });
  return new URL(page.url()).pathname.split("/").pop() ?? "";
}

/** POST /setup/harness-login from the page's own session, as the pane does. */
async function launchSignIn(page: Page): Promise<{ status: number; run_id?: string }> {
  return page.evaluate(async (startURL: string) => {
    const r = await fetch("/api/v1/setup/harness-login", {
      method: "POST",
      credentials: "include",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ provider: "aws", sso_start_url: startURL }),
    });
    const body = (await r.json().catch(() => ({}))) as { run_id?: string };
    return { status: r.status, run_id: body.run_id };
  }, SSO_START_URL);
}

async function killRun(page: Page, id: string): Promise<void> {
  const status = await page.evaluate(async (runID: string) => {
    const r = await fetch(`/api/v1/runs/${runID}/kill`, { method: "POST", credentials: "include" });
    return r.status;
  }, id);
  // 409 is a run that already ended on its own (an autonomous run finishes its
  // task), which is what this call wants too.
  expect(status === 409 || status < 400, `killing run ${id}: ${status}`).toBe(true);
}

/** End every run still alive from the earlier files, so this file's sandboxes
 *  can schedule on the walk's one node (nightly 36420610869: a run the
 *  recovery file left running held it, and A's sign-in sandbox never got a
 *  proxy pod IP). */
async function endLeftoverRuns(request: APIRequestContext): Promise<void> {
  const headers = { Authorization: `Bearer ${ADMIN_TOKEN}` };
  const active = async () => {
    const res = await request.get("/api/v1/runs?status=active", { headers });
    expect(res.status(), `GET /runs?status=active: ${await res.text()}`).toBe(200);
    return (await res.json()) as Array<{ id: string }>;
  };
  for (const run of await active()) {
    const res = await request.post(`/api/v1/runs/${run.id}/kill`, { headers });
    expect(res.status(), `killing leftover run ${run.id}: ${await res.text()}`).toBeLessThan(500);
  }
  await expect.poll(async () => (await active()).length, { timeout: SANDBOX_UP }).toBe(0);
}

async function runState(page: Page, id: string): Promise<string> {
  return page.evaluate(async (runID: string) => {
    const r = await fetch(`/api/v1/runs/${runID}`, { credentials: "include" });
    return ((await r.json().catch(() => ({}))) as { state?: string }).state ?? "";
  }, id);
}

/** The model-access states that offer "Sign in to AWS" (MODEL_ACCESS_ACTIONABLE). */
const ACTIONABLE = ["not_configured", "expired_signin", "expiring"];

/** Make the member actionable, whatever pin the previous spec left. One flip
 *  contradicts the member's capture, unless the previous spec stopped between
 *  its own flip and its capture: then the flip lands back on the pin the
 *  capture was taken under, and the member still reads `live`. A second flip
 *  then contradicts it. */
async function ensureActionable(request: APIRequestContext, page: Page): Promise<void> {
  for (let flip = 1; flip <= 2; flip++) {
    await makeMemberActionable(request);
    if (await becomesActionable(page)) return;
  }
  const state = (await modelAccess(page)).state;
  throw new Error(
    `the member still reads '${state}' after two pin flips, so no sign-in is offered: walk state, not this file's subject`,
  );
}

/** Polls up to 30 s for the member to be offered a sign-in. */
async function becomesActionable(page: Page): Promise<boolean> {
  const deadline = Date.now() + 30_000;
  while (Date.now() < deadline) {
    if (ACTIONABLE.includes((await modelAccess(page)).state ?? "")) return true;
    await page.waitForTimeout(1_000);
  }
  return false;
}

/** Leave the roster on the walk's own pin with the member actionable, so A's
 *  captures are under the pair C's runs can spend: WARDYN_BEDROCK_MODEL is an
 *  ARN in PIN_ACCOUNT (nightly 36428749715: A's flip landed on the other pair,
 *  and C's first run minted role credentials for it but never reached
 *  Bedrock). A member already live under the walk pin is signed in under the
 *  other pair first, so the walk pin contradicts their capture. */
async function actionableUnderWalkPin(request: APIRequestContext, page: Page): Promise<void> {
  await putRoster(request);
  if (await becomesActionable(page)) return;
  await putRoster(request, OTHER_ACCOUNT, OTHER_ROLE);
  if (!(await becomesActionable(page))) {
    throw new Error(`the member reads '${(await modelAccess(page)).state}' under both pairs: walk state, not this file's subject`);
  }
  await signInThroughPane(page, openLoginPane);
  await putRoster(request);
  if (!(await becomesActionable(page))) {
    throw new Error(`the member reads '${(await modelAccess(page)).state}' after re-pinning to the walk pair`);
  }
}

/** Put the walk pin back and make sure page's member holds a capture under it,
 *  signing them in again when theirs is under the other pair. */
async function liveUnderWalkPin(request: APIRequestContext, page: Page): Promise<void> {
  await putRoster(request);
  if ((await modelAccess(page)).state === "live" && !(await becomesActionable(page))) return;
  await signInThroughPane(page, openLoginPane);
  await expect.poll(async () => (await modelAccess(page)).state, { timeout: 120_000 }).toBe("live");
}

let memberPage: Page;
let member2Page: Page;

test("A: two members sign in to AWS at the same moment and each holds their own capture", async ({ browser, request }) => {
  // Both actionable first: the member has no door until the pin contradicts
  // their capture; the second member has never signed in, which is actionable
  // under any pin.
  await endLeftoverRuns(request);
  memberPage = await signedIn(browser, MEMBER_EMAIL);
  member2Page = await signedIn(browser, MEMBER2_EMAIL);
  await actionableUnderWalkPin(request, memberPage);
  expect(ACTIONABLE, "the second member is offered no sign-in").toContain((await modelAccess(member2Page)).state);
  const before = [(await ownAWSRow(memberPage)).source_run_id ?? "", (await ownAWSRow(member2Page)).source_run_id ?? ""];

  await Promise.all([signInThroughPane(memberPage, openLoginPane), signInThroughPane(member2Page, openLoginPane)]);

  for (const page of [memberPage, member2Page]) {
    await expect.poll(async () => (await modelAccess(page)).state, { timeout: 120_000 }).toBe("live");
  }
  const after = [(await ownAWSRow(memberPage)).source_run_id ?? "", (await ownAWSRow(member2Page)).source_run_id ?? ""];
  expect(after[0], "the member's capture did not move").not.toBe(before[0]);
  expect(after[1], "the second member's capture did not move").not.toBe(before[1]);
  expect(after[0], "both members' captures name the same sign-in run").not.toBe(after[1]);
});

test("B: one member opening two sign-ins at once gets one live sign-in sandbox", async ({ request }) => {
  await ensureActionable(request, memberPage);
  const before = (await ownAWSRow(memberPage)).source_run_id ?? "";

  // Two launches from the same session, together. The per-person lock
  // serializes them: the later one ends the earlier (its own run, before its
  // pod ever boots), or answers 503 if the lock could not be taken in time.
  const second = await memberPage.context().newPage();
  await second.goto("/runs");
  const launches = await Promise.all([launchSignIn(memberPage), launchSignIn(second)]);
  const ok = launches.filter((l) => l.status === 200 && l.run_id).map((l) => l.run_id as string);
  for (const l of launches) {
    expect([200, 503], `a sign-in launch answered ${l.status}`).toContain(l.status);
  }
  expect(ok.length, "neither parallel sign-in launched").toBeGreaterThan(0);

  // Never two live at once, sampled until the capture lands. Recorded rather
  // than asserted inside the poll: a throw there is retried, so a moment with
  // two alive would be forgotten by the next sample.
  const bothAlive: string[][] = [];
  await expect
    .poll(
      async () => {
        const states = await Promise.all(ok.map((id) => runState(memberPage, id)));
        if (states.filter((s) => s && !TERMINAL_RUN_STATES.includes(s as RunState)).length > 1) bothAlive.push(states);
        const now = (await ownAWSRow(memberPage)).source_run_id ?? "";
        return now !== before && ok.includes(now);
      },
      { timeout: LOGIN_DONE, intervals: [1_000] },
    )
    .toBe(true);
  expect(bothAlive, "two of the member's sign-in sandboxes were alive at once").toEqual([]);

  const captured = (await ownAWSRow(memberPage)).source_run_id ?? "";
  for (const id of ok.filter((id) => id !== captured)) {
    await expect.poll(async () => runState(memberPage, id), { timeout: 120_000 }).toBe("KILLED");
  }
  await expect.poll(async () => (await modelAccess(memberPage)).state, { timeout: 120_000 }).toBe("live");
});

test("C: three runs each, taking turns, spend only their owner's session", async ({ request }) => {
  // B left the member live under the fixture's other pair; the second member
  // is live under the walk pin from A. C's runs spend on WARDYN_BEDROCK_MODEL,
  // an ARN in the walk pin's account, so both must hold a capture under it.
  for (const page of [memberPage, member2Page]) await liveUnderWalkPin(request, page);
  const owners = [
    { name: "member", page: memberPage, sessions: new Set<number>() },
    { name: "member2", page: member2Page, sessions: new Set<number>() },
  ];
  type Field = "role_cred_callers" | "bedrock_callers";
  for (let i = 1; i <= RUNS_PER_MEMBER; i++) {
    for (const o of owners) {
      // One run at a time: the node holds one agent run. Its calls are the
      // delta over its own window at its own proxy pod's IP, so an IP an
      // earlier run held cannot lend it another session's counts.
      const before = (await seen()).sessions;
      const id = await launchRun(o.page, `concurrency ${o.name} ${i}`);
      let ip = "";
      await expect
        .poll(() => (ip = ip || proxyPodIP(id)), { timeout: SANDBOX_UP, intervals: [2_000] })
        .not.toBe("");
      const grew = async (field: Field) =>
        (await seen()).sessions
          .filter((s) => (s[field][ip] ?? 0) > (before.find((b) => b.session === s.session)?.[field][ip] ?? 0))
          .map((s) => s.session);
      await expect
        .poll(async () => (await grew("role_cred_callers")).length > 0 && (await grew("bedrock_callers")).length > 0, {
          timeout: LOGIN_DONE,
          message: `${o.name}'s run ${id} never both minted role credentials and signed a bedrock call`,
        })
        .toBe(true);
      for (const field of ["role_cred_callers", "bedrock_callers"] as const) {
        for (const s of await grew(field)) o.sessions.add(s);
      }
      await killRun(o.page, id);
      await expect
        .poll(async () => TERMINAL_RUN_STATES.includes((await runState(o.page, id)) as RunState), { timeout: SANDBOX_UP })
        .toBe(true);
    }
  }
  for (const o of owners) {
    expect([...o.sessions], `${o.name}'s runs spent sessions ${[...o.sessions]}; want exactly one — their own`).toHaveLength(1);
  }
  expect([...owners[0].sessions][0], "both members' runs spent the SAME session").not.toBe([...owners[1].sessions][0]);
});
