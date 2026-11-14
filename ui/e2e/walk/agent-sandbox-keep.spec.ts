/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

/*
 * KEEP AND REVIVE ON THE AGENT-SANDBOX BACKEND, THROUGH THE API, AS A SIGNED-IN PERSON.
 *
 * Driven by scripts/kind-agent-sandbox-keep-walk.sh against the kind quickstart cluster with the Dex
 * overlay, the agent-sandbox controller, k8s.agentSandbox.keep.enabled and member drives with a kept
 * main drive. member@wardyn.local signs in through Dex, and every API call is made from that page's own
 * session, because keep_work_folder refuses any caller that is not a person who owns the run.
 *
 * The cases run in order and share the member's session:
 *   1. an interactive run with keep_work_folder starts and writes a file under /home/agent/work;
 *   2. its lease ends: the API reads it as lost with reason "ended", its agent pod is held behind the
 *      gate with no node, and its proxy pod is gone;
 *   3. the member extends the end and revives it: the pod is released and running, and the file reads back;
 *   4. an interactive run without keep_work_folder is torn down at its end as before.
 */

import { execFileSync } from "node:child_process";

import { expect, test, type Page } from "@playwright/test";

import { TERMINAL_RUN_STATES, type RunState } from "../../src/app/lib/types/runs";
import { MEMBER_EMAIL, SANDBOX_UP, dexSignIn } from "./helpers";

test.skip(process.env.WARDYN_TEST_K8S !== "1", "live cluster walk: set WARDYN_TEST_K8S=1 (scripts/kind-agent-sandbox-keep-walk.sh)");
test.describe.configure({ mode: "serial" });

const KUBE_CONTEXT = process.env.WARDYN_WALK_KUBE_CONTEXT || "kind-wardyn-quickstart";
/** The RUNS namespace: a run's agent and proxy pods land in k8s.runsNamespace. */
const KUBE_RUNS_NAMESPACE = process.env.WARDYN_WALK_KUBE_NAMESPACE || "wardyn-runs";
/** The scheduling gate wardynd holds an agent-sandbox agent pod behind (internal/runner/k8s/agentsandbox.go). */
const GATE = "wardyn.dev/verify";
const PROOF_PATH = "/home/agent/work/keep-walk-proof";
/** How long the kept run lives before its lease ends; it must outlast the sandbox coming up. */
const KEPT_ENDS_AFTER_SEC = 240;
const PLAIN_ENDS_AFTER_SEC = 120;
/** Past the end the lease pass has to notice it and stop the sandbox. */
const END_GRACE_MS = 180_000;

/** The shape scripts/kind-survival-walk.sh launches its interactive runs with: no egress, never reaped
 *  while idle, and a small request so two runs fit the walk's one small node. */
const INLINE_POLICY = {
  allowed_domains: [],
  first_use_approval: "always_deny",
  min_confinement_class: "CC1",
  auto_stop_after_sec: -1,
  resources: { cpu_millis: 250, memory_mib: 256 },
};

interface Reply {
  status: number;
  body: Record<string, unknown>;
}

interface RunRow {
  state?: string;
  lost_at?: string | null;
  lost_reason?: string;
}

let page: Page;
const launched: string[] = [];

/** Call the API from the signed-in page: same origin, the session cookie, as the console does. */
async function api(method: string, path: string, body?: unknown): Promise<Reply> {
  return page.evaluate(
    async ({ method, path, body }) => {
      const r = await fetch(`/api/v1${path}`, {
        method,
        credentials: "include",
        headers: body === undefined ? {} : { "Content-Type": "application/json" },
        body: body === undefined ? undefined : JSON.stringify(body),
      });
      const text = await r.text();
      let parsed: Record<string, unknown> = {};
      try {
        parsed = JSON.parse(text) as Record<string, unknown>;
      } catch {
        parsed = { raw: text };
      }
      return { status: r.status, body: parsed };
    },
    { method, path, body },
  );
}

async function getRun(id: string): Promise<RunRow> {
  const r = await api("GET", `/runs/${id}`);
  expect(r.status, `GET /runs/${id}: ${JSON.stringify(r.body)}`).toBe(200);
  return r.body as RunRow;
}

/** One kubectl read that may legitimately have nothing to read: a missing pod exits 1, which would
 *  throw inside expect.poll and abort the poll, so it reads as "". */
function kubectlOrEmpty(...args: string[]): string {
  try {
    return execFileSync("kubectl", ["--context", KUBE_CONTEXT, "-n", KUBE_RUNS_NAMESPACE, ...args], {
      encoding: "utf8",
      stdio: "pipe",
    }).trim();
  } catch {
    return "";
  }
}

const agentPod = (id: string) => `wardyn-agent-${id}`;
const podsOfRun = (id: string) => kubectlOrEmpty("get", "pods", "-l", `wardyn.run-id=${id}`, "-o", "name");
const proxyPodsOfRun = (id: string) =>
  kubectlOrEmpty("get", "pods", "-l", `wardyn.run-id=${id},wardyn.component=proxy`, "-o", "name");
const gatesOf = (id: string) => kubectlOrEmpty("get", "pod", agentPod(id), "-o", "jsonpath={.spec.schedulingGates[*].name}");
const nodeOf = (id: string) => kubectlOrEmpty("get", "pod", agentPod(id), "-o", "jsonpath={.spec.nodeName}");
const phaseOf = (id: string) => kubectlOrEmpty("get", "pod", agentPod(id), "-o", "jsonpath={.status.phase}");

async function create(extra: Record<string, unknown>, title: string): Promise<string> {
  const r = await api("POST", "/runs", {
    agent: "claude-code",
    repo: "local:kind-agent-sandbox-keep",
    title,
    interactive: true,
    inline_policy: INLINE_POLICY,
    ...extra,
  });
  expect([200, 201], `POST /runs: ${r.status} ${JSON.stringify(r.body)}`).toContain(r.status);
  const id = String(r.body.id ?? "");
  expect(id, `POST /runs carried no id: ${JSON.stringify(r.body)}`).not.toBe("");
  launched.push(id);
  return id;
}

/** Run one command in the run through the exec route. A refusal reads as exit code -1 with the body in stderr, so
 *  a poll on the exit code retries while the agent finishes coming up. */
async function exec(id: string, command: string): Promise<{ exit_code: number; stdout?: string; stderr?: string }> {
  const r = await api("POST", `/runs/${id}/exec`, { command, timeout_sec: 60 });
  if (r.status !== 200) return { exit_code: -1, stderr: `${r.status} ${JSON.stringify(r.body)}` };
  return r.body as { exit_code: number; stdout?: string; stderr?: string };
}

async function waitRunning(id: string): Promise<void> {
  await expect
    .poll(async () => (await getRun(id)).state ?? "", { timeout: SANDBOX_UP, intervals: [2_000], message: `run ${id} never reached RUNNING` })
    .toBe("RUNNING");
}

let keptID = "";

test.beforeAll(async ({ browser }) => {
  page = await (await browser.newContext()).newPage();
  await dexSignIn(page, MEMBER_EMAIL);
});

test.afterAll(async () => {
  // Best effort: a run left alive would hold the node for whatever runs next on this cluster.
  for (const id of launched) await api("POST", `/runs/${id}/kill`).catch(() => undefined);
  await page.context().close().catch(() => undefined);
});

test("the member starts an interactive run with a kept work folder and writes a file", async () => {
  keptID = await create({ keep_work_folder: true, ends_after_sec: KEPT_ENDS_AFTER_SEC }, "keep walk: kept");
  await waitRunning(keptID);
  // Vacuous on the plain backend: the agent pod must be one an agent-sandbox Sandbox owns.
  await expect
    .poll(() => kubectlOrEmpty("get", "pod", agentPod(keptID), "-o", "jsonpath={.metadata.ownerReferences[0].kind}"), { timeout: 120_000 })
    .toBe("Sandbox");
  await expect
    .poll(async () => (await exec(keptID, `echo proof > ${PROOF_PATH}`)).exit_code, {
      timeout: 180_000,
      intervals: [3_000],
      message: "the exec route never wrote the proof file",
    })
    .toBe(0);
  const read = await exec(keptID, `cat ${PROOF_PATH}`);
  expect(read.exit_code, JSON.stringify(read)).toBe(0);
  expect((read.stdout ?? "").trim()).toBe("proof");
});

test("its end reads as kept: the agent pod is held with no node, and the proxy pod is gone", async () => {
  await expect
    .poll(async () => {
      const run = await getRun(keptID);
      return `${run.lost_reason ?? ""}${run.lost_at ? "" : " (lost_at unset)"}`;
    }, { timeout: KEPT_ENDS_AFTER_SEC * 1_000 + END_GRACE_MS, intervals: [5_000], message: `run ${keptID} never read as lost with reason ended` })
    .toBe("ended");
  // A lease-ended run stays RUNNING until the ended-run grace: kept, not torn down.
  expect((await getRun(keptID)).state).toBe("RUNNING");
  // The controller makes the pod again, held, after wardynd deletes the running one.
  await expect.poll(() => gatesOf(keptID), { timeout: 120_000, intervals: [2_000], message: "the agent pod never came back held" }).toContain(GATE);
  expect(nodeOf(keptID), "a held pod has no node").toBe("");
  expect(proxyPodsOfRun(keptID), "no proxy pod exists for a kept run").toBe("");
});

test("extending the end and reviving brings it back running, with its file", async () => {
  const extend = await api("PATCH", `/runs/${keptID}`, { ends_at: new Date(Date.now() + 3_600_000).toISOString() });
  expect(extend.status, `PATCH /runs/${keptID}: ${JSON.stringify(extend.body)}`).toBe(200);
  const revive = await api("POST", `/runs/${keptID}/revive`);
  expect(revive.status, `POST /runs/${keptID}/revive: ${JSON.stringify(revive.body)}`).toBe(200);
  await expect
    .poll(async () => {
      const run = await getRun(keptID);
      return `${run.state ?? ""}${run.lost_at ? " lost" : ""}`;
    }, { timeout: SANDBOX_UP, intervals: [2_000], message: `run ${keptID} never read as running again` })
    .toBe("RUNNING");
  await expect.poll(() => phaseOf(keptID), { timeout: 120_000, intervals: [2_000] }).toBe("Running");
  expect(gatesOf(keptID), "a revived pod is released").toBe("");
  expect(nodeOf(keptID), "a revived pod is on a node").not.toBe("");
  await expect
    .poll(async () => ((await exec(keptID, `cat ${PROOF_PATH}`)).stdout ?? "").trim(), {
      timeout: 180_000,
      intervals: [3_000],
      message: "the proof file did not read back after revive",
    })
    .toBe("proof");
});

test("a run that is not kept is torn down at its end", async () => {
  // Free the node first: the kept run is done, and killing a revived run is the ordinary teardown.
  const kill = await api("POST", `/runs/${keptID}/kill`);
  expect([200, 202, 409], `POST /runs/${keptID}/kill: ${kill.status} ${JSON.stringify(kill.body)}`).toContain(kill.status);
  await expect.poll(() => podsOfRun(keptID), { timeout: 180_000, intervals: [3_000], message: "the killed run's pods never went" }).toBe("");

  const plainID = await create({ ends_after_sec: PLAIN_ENDS_AFTER_SEC }, "keep walk: plain");
  // Its end is two minutes after the create, so it may end before it reaches RUNNING: the walk needs its pods
  // to have existed, not for it to have run.
  await expect.poll(() => podsOfRun(plainID), { timeout: SANDBOX_UP, intervals: [2_000], message: "the plain run never got a pod" }).not.toBe("");
  await expect
    .poll(async () => TERMINAL_RUN_STATES.includes(((await getRun(plainID)).state ?? "") as RunState), {
      timeout: PLAIN_ENDS_AFTER_SEC * 1_000 + END_GRACE_MS,
      intervals: [5_000],
      message: `run ${plainID} never ended`,
    })
    .toBe(true);
  // Within 120 s of its end no pod with its run label remains.
  await expect.poll(() => podsOfRun(plainID), { timeout: 120_000, intervals: [3_000], message: "the plain run's pods outlived its end" }).toBe("");
  expect((await getRun(plainID)).lost_at ?? null, "a run that is not kept is not read as lost").toBeNull();
});
