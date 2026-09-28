/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

/*
 * THE LIVE AWS SSO WALK, PART THREE — two people at once (#697).
 *
 * Runs in the same invocation and against the same cluster as sso-member and
 * sso-member-recovery (scripts/kind-sso-walk.sh), after them. It inherits
 * nothing it does not re-establish: both members are made actionable by a pin
 * flip before they sign in, and every observation is scoped to runs this file
 * launched.
 *
 * What it proves that one principal at a time cannot:
 *
 *   A. two members signing in to AWS at the same moment each end up holding
 *      their OWN capture — neither sign-in rotates the other's session away;
 *   C. three runs each, launched together, spend only their OWNER's session:
 *      every GetRoleCredentials a run's proxy made, and every bedrock call it
 *      signed, lands under one fake session per person, and the two people's
 *      sessions never share a run;
 *   B. one member opening two sign-ins at once gets ONE live sign-in sandbox:
 *      the per-person lock (harnesscred_supersede.go) ends the older launch,
 *      and the capture that lands is the other one's.
 *
 * C runs before B because B flips the pin again, which leaves the second
 * member's capture contradicting it — a run of theirs would then be refused.
 *
 * HOW A RUN IS TIED TO A SESSION. The fake (test/awsssofake) gives every
 * sign-in its own session and lists, per session, the peers that called
 * GetRoleCredentials and the peers whose bedrock calls were signed with that
 * session's key (/_seen `sessions`). On this cluster the peer is the run's own
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
import { expect, test, type Browser, type Page } from "@playwright/test";
import { TERMINAL_RUN_STATES, type RunState } from "../../src/app/lib/types/runs";
import {
  LOGIN_DONE,
  MEMBER_EMAIL,
  SANDBOX_UP,
  SSO_START_URL,
  dexSignIn,
  makeMemberActionable,
  modelAccess,
  openLoginPane,
  ownAWSRow,
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

async function runState(page: Page, id: string): Promise<string> {
  return page.evaluate(async (runID: string) => {
    const r = await fetch(`/api/v1/runs/${runID}`, { credentials: "include" });
    return ((await r.json().catch(() => ({}))) as { state?: string }).state ?? "";
  }, id);
}

let memberPage: Page;
let member2Page: Page;

test("A: two members sign in to AWS at the same moment and each holds their own capture", async ({ browser, request }) => {
  // Both actionable first: the member is `live` from the earlier files and has
  // no door until the pin contradicts their capture; the second member has
  // never signed in, which is actionable under any pin.
  await makeMemberActionable(request);
  memberPage = await signedIn(browser, MEMBER_EMAIL);
  member2Page = await signedIn(browser, MEMBER2_EMAIL);
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

test("C: three runs each, launched together, spend only their owner's session", async () => {
  const owners = [
    { name: "member", page: memberPage, runs: [] as string[] },
    { name: "member2", page: member2Page, runs: [] as string[] },
  ];
  await Promise.all(
    owners.map(async (o) => {
      for (let i = 1; i <= RUNS_PER_MEMBER; i++) {
        o.runs.push(await launchRun(o.page, `concurrency ${o.name} ${i}`));
      }
    }),
  );

  // A run is its proxy pod's IP, read while the run is alive: a finished
  // run's pod is deleted, and the IP with it.
  const ipOf = new Map<string, string>();
  const all = owners.flatMap((o) => o.runs);
  await expect
    .poll(
      () => {
        for (const id of all) {
          if (!ipOf.has(id)) {
            const ip = proxyPodIP(id);
            if (ip) ipOf.set(id, ip);
          }
        }
        return ipOf.size;
      },
      { timeout: SANDBOX_UP * 2, intervals: [2_000] },
    )
    .toBe(all.length);
  expect(new Set(ipOf.values()).size, "two runs reported the same proxy pod IP; attribution would be ambiguous").toBe(all.length);

  // Every run both minted role credentials and spent them on bedrock.
  const sessionsOf = async (ip: string, field: "role_cred_callers" | "bedrock_callers") =>
    (await seen()).sessions.filter((s) => (s[field][ip] ?? 0) > 0).map((s) => s.session);
  for (const id of all) {
    const ip = ipOf.get(id) ?? "";
    await expect.poll(async () => (await sessionsOf(ip, "role_cred_callers")).length, { timeout: LOGIN_DONE }).toBeGreaterThan(0);
    await expect.poll(async () => (await sessionsOf(ip, "bedrock_callers")).length, { timeout: LOGIN_DONE }).toBeGreaterThan(0);
  }

  const sessionOfOwner: number[] = [];
  for (const o of owners) {
    const used = new Set<number>();
    for (const id of o.runs) {
      const ip = ipOf.get(id) ?? "";
      for (const field of ["role_cred_callers", "bedrock_callers"] as const) {
        for (const s of await sessionsOf(ip, field)) used.add(s);
      }
    }
    expect([...used], `${o.name}'s runs spent sessions ${[...used]}; want exactly one — their own`).toHaveLength(1);
    sessionOfOwner.push([...used][0]);
  }
  expect(sessionOfOwner[0], "both members' runs spent the SAME session").not.toBe(sessionOfOwner[1]);
});

test("B: one member opening two sign-ins at once gets one live sign-in sandbox", async ({ request }) => {
  await makeMemberActionable(request);
  await expect.poll(async () => (await modelAccess(memberPage)).state, { timeout: 120_000 }).toBe("expired_signin");
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
