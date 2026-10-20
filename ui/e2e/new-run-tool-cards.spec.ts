/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// New Run, step 5: every tool card says what it brings in the same four rows (Network, Credentials, Files and
// drives, Other), and Details opens the whole of it. The kubectl card is Coming soon until the server's
// `features.kubectl_cli` is on, and Review names a cluster's service account and what the run may do there.
//
// The preview is the real server's with its `access` block (and, for the GitHub CLI, its `components`) replaced; the
// preflight, the catalogue and the setup status are stubbed from the same fixtures the unit tests use.
import type { Page } from "@playwright/test";
import { test, expect, gotoConsole, navToRoute } from "./fixtures";
import { NR_DOCUMENT } from "../src/app/components/wardyn/copy/new-run/document";
import { NR_REVIEW } from "../src/app/components/wardyn/copy/new-run/review";
import { NR_SHARED } from "../src/app/components/wardyn/copy/new-run/shared";
import { NR_START } from "../src/app/components/wardyn/copy/new-run/start";
import { NR_TOOLS } from "../src/app/components/wardyn/copy/new-run/tools";
import { MY_COMPONENTS, ORG_GITHUB, ORG_KUBECTL, ORG_LINEAR } from "../src/app/lib/new-run-fixtures/scenarios/catalogue";
import { SCENARIO_HARNESSES, SCENARIO_HARNESS_SETS } from "../src/app/lib/new-run-fixtures/scenarios/harnesses";
import { SCENARIO_PREFLIGHT } from "../src/app/lib/new-run-fixtures/scenarios/preflight";
import { SCENARIO_ACCESS } from "../src/app/lib/new-run-fixtures/scenarios/tools";
import type { AccessFacts, AccessHost } from "../src/app/lib/types/new-run-access";

const DOCUMENT = "api_version: wardyn/v1\nkind: Run\nintent:\n  shape: application\n  experience: background\n  workload:\n    kind: command\n    command: npm test\n  no_repositories_or_drives: true\n";
const press = (page: Page, name: string) => page.getByRole("button", { name, exact: true }).click();

const CLUSTER = "staging";
const catalogue = {
  ...MY_COMPONENTS,
  org: [...MY_COMPONENTS.org, ORG_KUBECTL, ORG_GITHUB],
  clis: MY_COMPONENTS.clis.map((c) => (c.kind === "kubectl" ? { ...c, state: "available", rows: [ORG_KUBECTL.id] } : c.kind === "github" ? { ...c, state: "available", rows: [ORG_GITHUB.id] } : c)),
};
const GITHUB_FACT = { kind: "git_provider", provider: "github", lane: "app", id: "git_provider:github:app:acme", reason: "workspace", status: "ready", requirements: [], repos: ["github.com/acme/web-app", "github.com/acme/api"] };
const KUBE_CREDENTIAL = { id: `${ORG_KUBECTL.id}:kube_api`, row: ORG_KUBECTL.id, kind: "kube_api", owner: "shared", label: "Your organisation's cluster account", delivery: "proxy", renewed: true, after_end: "gone" };
// The server adds the row's API server as an injected host whose source is the row.
const KUBE_HOST: AccessHost = { host: "api.staging.example.com", via: "injected", rows: [ORG_KUBECTL.id], verdict: "allowed", source: { kind: "component", id: ORG_KUBECTL.id, name: ORG_KUBECTL.name } };
const withKubectl: AccessFacts = {
  ...SCENARIO_ACCESS.member,
  hosts: [...SCENARIO_ACCESS.member.hosts, KUBE_HOST],
  rows: [...SCENARIO_ACCESS.member.rows, { id: ORG_KUBECTL.id, kind: "cli", from: "added", name: ORG_KUBECTL.name, status: "ready", hosts: [], credentials: [KUBE_CREDENTIAL.id], source: { kind: "component", id: ORG_KUBECTL.id, name: ORG_KUBECTL.name } }],
  credentials: [...SCENARIO_ACCESS.member.credentials, KUBE_CREDENTIAL],
};

/** The tools step with the switch as given, then back to it from Review's Access group. */
async function openTools(page: Page, kubectlOn: boolean, access: AccessFacts = SCENARIO_ACCESS.member) {
  await page.route("**/api/v1/runs/policy-preview", async (route) => {
    const response = await route.fetch();
    await route.fulfill({ response, json: { ...(await response.json()), access, components: [GITHUB_FACT] } });
  });
  await page.route("**/api/v1/runs/preflight", (route) => route.fulfill({ json: SCENARIO_PREFLIGHT.member }));
  await page.route("**/api/v1/me/components", (route) => (route.request().method() === "GET" ? route.fulfill({ json: catalogue }) : route.fallback()));
  let status: Record<string, unknown> | null = null;
  await page.route("**/api/v1/setup/status*", async (route) => {
    status ??= {
      ...((await (await route.fetch()).json()) as Record<string, unknown>),
      harnesses: SCENARIO_HARNESSES.full,
      harness_sets: SCENARIO_HARNESS_SETS.full,
      features: { kubectl_cli: kubectlOn },
    };
    await route.fulfill({ json: status });
  });
  await gotoConsole(page);
  await navToRoute(page, "/runs/new");
  await press(page, NR_START.DOC_YAML);
  await page.getByRole("textbox", { name: NR_DOCUMENT.EDITOR_ARIA("YAML") }).fill(DOCUMENT);
  await press(page, NR_DOCUMENT.CONTINUE_REVIEW);
  await expect(page.getByRole("heading", { name: NR_REVIEW.TITLE })).toBeVisible();
  await press(page, `${NR_REVIEW.CHANGE} ${NR_REVIEW.GROUP_ACCESS}`);
  await press(page, NR_REVIEW.EDIT_TOOLS_ACCESS);
  await expect(page.getByRole("heading", { name: NR_TOOLS.TITLE })).toBeVisible();
}
const card = (page: Page, name: string) => page.getByRole("listitem").filter({ has: page.getByRole("heading", { name, exact: true, level: 3 }) });

test.describe("New Run, step 5: what each tool card brings", () => {
  test("the kubectl card shows the cluster, its service account and the namespaces; Details has all of it", async ({ page }) => {
    await openTools(page, true);
    const kube = card(page, ORG_KUBECTL.name);
    const dl = kube.locator("dl");
    await expect(dl.locator("dt")).toHaveText([NR_TOOLS.NETWORK_TITLE, NR_TOOLS.CREDENTIALS_TITLE, NR_TOOLS.DETAIL_FILES, NR_TOOLS.DETAIL_OTHER]);
    await expect(dl.locator("dd").nth(0)).toHaveText("api.staging.example.com");
    await expect(dl.locator("dd").nth(1)).toHaveText(`${NR_TOOLS.KUBE_CREDENTIAL(CLUSTER)}${NR_TOOLS.DELIVERY_PROXY}`);
    await expect(dl.locator("dd").nth(2)).toHaveText(NR_TOOLS.DETAIL_NONE);
    await expect(dl.locator("dd").nth(3)).toHaveText(`${NR_TOOLS.KUBE_NAMESPACES("web, jobs")} · ${NR_TOOLS.KUBE_READ_ONLY} · ${NR_TOOLS.KUBE_NO_EXEC}`);

    await kube.getByRole("button", { name: NR_TOOLS.DETAILS_FOR(ORG_KUBECTL.name) }).click();
    const dialog = page.getByRole("dialog", { name: ORG_KUBECTL.name });
    await expect(dialog.getByRole("heading", { level: 3 })).toHaveText([NR_TOOLS.NETWORK_TITLE, NR_TOOLS.CREDENTIALS_TITLE, NR_TOOLS.DETAIL_FILES, NR_TOOLS.DETAIL_OTHER]);
    await expect(dialog.getByText(NR_TOOLS.KUBE_CREDENTIAL_NOTE)).toBeVisible();
    await expect(dialog.getByText(NR_TOOLS.KUBE_READ_DETAIL)).toBeVisible();
    await expect(dialog.getByText(NR_TOOLS.KUBE_EXEC_DETAIL)).toBeVisible();
    await dialog.getByRole("button", { name: NR_REVIEW.CLOSE, exact: true }).last().click();
    await expect(dialog).toBeHidden();
  });

  test("the GitHub CLI card acts on the run's repositories through the Wardyn GitHub App", async ({ page }) => {
    await openTools(page, true);
    const gh = card(page, ORG_GITHUB.name);
    await expect(gh.locator("dl dd").nth(0)).toHaveText("api.github.com");
    await expect(gh.locator("dl dd").nth(1)).toHaveText(`${NR_TOOLS.GITHUB_APP}${NR_TOOLS.DELIVERY_PROXY}`);
    await expect(gh.locator("dl dd").nth(3)).toHaveText(NR_TOOLS.GITHUB_REPOS("acme/web-app, acme/api"));
    await gh.getByRole("button", { name: NR_TOOLS.DETAILS_FOR(ORG_GITHUB.name) }).click();
    const dialog = page.getByRole("dialog", { name: ORG_GITHUB.name });
    await expect(dialog.getByText(NR_TOOLS.DELIVERY_GITHUB)).toBeVisible();
    await expect(dialog.getByText(NR_TOOLS.GITHUB_REPOS_LABEL)).toBeVisible();
    await expect(dialog.getByText("acme/api", { exact: true })).toBeVisible();
  });

  test("a remote MCP server card names its host, delivery and kind; Details has the server URL", async ({ page }) => {
    await openTools(page, true);
    const linear = card(page, ORG_LINEAR.name);
    await expect(linear.locator("dl dd").nth(0)).toHaveText("mcp.linear.app");
    await expect(linear.locator("dl dd").nth(3)).toHaveText(`${NR_TOOLS.FROM_MCP} · ${NR_TOOLS.MCP_REMOTE}`);
    await linear.getByRole("button", { name: NR_TOOLS.DETAILS_FOR(ORG_LINEAR.name) }).click();
    const dialog = page.getByRole("dialog", { name: ORG_LINEAR.name });
    await expect(dialog.getByText(ORG_LINEAR.mcp!.url)).toBeVisible();
    await expect(dialog.getByText(NR_TOOLS.DETAIL_MCP_BODY)).toBeVisible();
  });

  test("with the switch off the kubectl card is Coming soon and brings nothing", async ({ page }) => {
    await openTools(page, false);
    const kube = card(page, "kubectl");
    await expect(kube.getByText(NR_SHARED.COMING_SOON)).toBeVisible();
    await expect(kube.locator("dl")).toHaveCount(0);
    await expect(page.getByRole("heading", { name: ORG_KUBECTL.name })).toHaveCount(0);
  });

  test("Review names the cluster's service account, the namespaces and what the run may do there", async ({ page }) => {
    await openTools(page, true, withKubectl);
    await press(page, NR_TOOLS.REVIEW_RUN);
    await expect(page.getByRole("heading", { name: NR_REVIEW.GROUP_ACCESS, level: 3 }).locator("xpath=..").locator("strong")).toHaveText(NR_REVIEW.POLICY_HOSTS(SCENARIO_ACCESS.member.hosts.length + 1));
    await press(page, `${NR_REVIEW.CHANGE} ${NR_REVIEW.GROUP_ACCESS}`);
    const group = page.getByRole("group", { name: NR_REVIEW.GROUP_ACCESS });
    await expect(group.locator("div").filter({ has: page.locator("code", { hasText: KUBE_HOST.host }) }).getByText(ORG_KUBECTL.name, { exact: true })).toBeVisible();
    await expect(group.getByText(NR_TOOLS.KUBE_CREDENTIAL(CLUSTER))).toBeVisible();
    await expect(group.getByText(NR_TOOLS.FOR_ROW(ORG_KUBECTL.name))).toBeVisible();
    await expect(group.getByText(`${NR_TOOLS.KUBE_NAMESPACES("web, jobs")} · ${NR_TOOLS.KUBE_READ_ONLY} · ${NR_TOOLS.KUBE_NO_EXEC}`)).toBeVisible();
  });
});
