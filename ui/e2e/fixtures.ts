/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { execFileSync } from "node:child_process";
import { createHash } from "node:crypto";
import { basename } from "node:path";
import {
  test as base,
  expect,
  type Locator,
  type Page,
} from "@playwright/test";
import { RUN_DOCUMENT_KIND, TEMPLATE_API_VERSION } from "../src/app/lib/types/templates";
import { NR_DOCUMENT } from "../src/app/components/wardyn/copy/new-run/document";
import { NR_FILES } from "../src/app/components/wardyn/copy/new-run/files";
import { NR_HARNESSES } from "../src/app/components/wardyn/copy/new-run/harnesses";
import { NR_LAUNCH } from "../src/app/components/wardyn/copy/new-run/launch";
import { NR_REVIEW } from "../src/app/components/wardyn/copy/new-run/review";
import { NR_RUN } from "../src/app/components/wardyn/copy/new-run/run";
import { NR_SHAPE } from "../src/app/components/wardyn/copy/new-run/shape";
import { NR_SHARED } from "../src/app/components/wardyn/copy/new-run/shared";
import { NR_START } from "../src/app/components/wardyn/copy/new-run/start";
import { NR_TOOLS } from "../src/app/components/wardyn/copy/new-run/tools";

// Shared Playwright fixtures for the Wardyn UI e2e suite. Specs run against the
// seeded test backend booted by scripts/e2e-backend.sh (real wardynd + Postgres +
// `none` runner). See playwright.config.ts for the base URL.

// The admin token the seeded backend is started with. The app stores it under
// localStorage["wardyn_admin_token"] and probes /api/v1/runs on mount to decide
// auth; injecting it before first navigation boots the app already signed in.
export const ADMIN_TOKEN = process.env.WARDYN_E2E_TOKEN || "";
// #510-F11 — exported so no OTHER e2e file has to re-type this literal (it
// mirrors lib/api/core.ts's own private TOKEN_KEY; a rename there that this
// file's own hand-typed copy missed used to make the two-tabs case in
// attach-stub.ts sign in nowhere, failing with an unrelated-looking auth error
// instead of a clear mismatch).
export const TOKEN_KEY = "wardyn_admin_token";

// Rotate with each backend; e2e-backend.sh stores only their SHA-256 hashes.
function personToken(principal: string): string {
  return `wdn_${createHash("sha256").update(`${ADMIN_TOKEN}:${principal}`).digest("hex")}`;
}
export const MEMBER_TOKEN = personToken("e2e-member");
const SECURITY_ADMIN_TOKEN = personToken("e2e-security-admin");
const SECURITY_ADMIN_2_TOKEN = personToken("e2e-security-admin-2");
export const MEMBER_PRINCIPAL = "e2e-member";

// T-68 — page-health teardown gate. A spec whose page threw an uncaught JS
// error or tripped the CSP fails silently everywhere else: the click that
// caused it still "worked" (React error boundaries and the browser both eat
// it), so nothing but the browser's own devtools console would ever have
// shown it. Every spec importing `test` from here gets it collected and
// checked for free.
//
// Named by spec basename (no extension) — a debt list, not a convenience: a
// spec joins it only when it has no narrower way to explain ITS OWN noise.
// A spec that already asserts on the same CSP/console noise itself does NOT
// belong here even though it trips the same page-health signal — adding this
// file-wide gate on top would just double-report the same finding. That is
// why the set below does NOT include recording.spec.ts: its WASM-player test
// already asserts on a filtered CSP/WASM pattern of its own, so the finding
// stays scoped to that one assertion instead of failing every check in the
// spec.
//
// episode-catalog: its "configured video source" describe block DELIBERATELY
// drives an unadmitted media-src host so the browser's own CSP blocks it —
// the spec's own comment calls this out, and its assertions are that the
// configured-source error copy renders and the mirror host never leaks into
// text (Q145-2). The violation this trips IS the thing under test.
const PAGE_HEALTH_ALLOWLIST = new Set<string>(["episode-catalog"]);

function pageHealthAllowed(testFile: string): boolean {
  return PAGE_HEALTH_ALLOWLIST.has(basename(testFile).replace(/\.spec\.ts$/, ""));
}

// `test` boots the app pre-authenticated so each spec lands directly in the
// console. Auth-flow specs that exercise sign-in/sign-out should import the raw
// `test` from "@playwright/test" instead and manage storage themselves.
export const test = base.extend({
  page: async ({ page }, use, testInfo) => {
    if (!ADMIN_TOKEN) throw new Error("WARDYN_E2E_TOKEN is missing; use scripts/run-ui-e2e.sh or read e2e-backend.sh token for this backend");
    await page.addInitScript(
      ([key, tok]) => {
        try {
          // Keep a real actor selected before navigation; one initializer owns auth.
          if (!sessionStorage.getItem(key) && !localStorage.getItem(key)) {
            localStorage.setItem(key, tok);
          }
        } catch {
          /* private mode — ignore */
        }
      },
      [TOKEN_KEY, ADMIN_TOKEN],
    );

    // pageerror: a real, Playwright-native page event — no init script needed.
    const pageErrors: string[] = [];
    page.on("pageerror", (err) => pageErrors.push(err.stack || err.message));

    // securitypolicyviolation is a DOM event, not a Playwright page event, so
    // the only channel back to this Node-side collector is a page-JS listener
    // reporting through an exposed binding. addInitScript re-installs it on
    // every document the page navigates to (a fresh document has no listeners
    // of its own), and exposeBinding must be wired before that script can call
    // it — order below matters.
    const cspViolations: string[] = [];
    await page.exposeBinding("__wardynReportCSPViolation", (_source, detail: string) => {
      cspViolations.push(detail);
    });
    await page.addInitScript(() => {
      document.addEventListener("securitypolicyviolation", (e) => {
        (window as unknown as { __wardynReportCSPViolation: (d: string) => void }).__wardynReportCSPViolation(
          `${e.violatedDirective} blocked ${e.blockedURI} (${e.sourceFile}:${e.lineNumber})`,
        );
      });
    });

    await use(page);

    if (pageHealthAllowed(testInfo.file)) return;
    if (pageErrors.length > 0) {
      throw new Error(`uncaught page error(s) during "${testInfo.title}":\n${pageErrors.join("\n---\n")}`);
    }
    if (cspViolations.length > 0) {
      throw new Error(`CSP violation(s) during "${testInfo.title}":\n${cspViolations.join("\n---\n")}`);
    }
  },
});

export { expect };

// Sidebar labels (app-shell.tsx). Navigating by accessible name keeps specs
// resilient to markup churn and needs no shared test-ids. Runs is the first/
// default screen; the Fleet board was merged into Runs and no longer exists, so
// it is intentionally absent from this list.
export type NavLabel =
  | "Runs"
  | "Approvals"
  // 0.8.6 (ppl-p2) — right after Approvals, both admin tiers.
  | "People"
  | "Workspaces"
  | "Policies"
  // 0.7 — sits between Policies and Permissions (app-shell.tsx's NAV_ITEMS), so
  // the three read as one narrowing sequence. Never in MEMBER_NAV_PATHS.
  | "Governance"
  | "Permissions"
  // 0.8 (UT-7a) — both admin tiers, beside Permissions.
  | "User types"
  // CS-8 (design F-1) — right after Permissions, both admin tiers too.
  | "Credentials"
  | "Secrets"
  | "Audit"
  | "Recordings"
  // A security admin's Admin view only (packet M-A).
  | "Drives"
  // #217's slot, under a divider (app-shell.tsx#SidebarNav): Setup and
  // Settings in the Admin view, Getting started and Your account in the User
  // view.
  | "Setup"
  | "Settings"
  | "Getting started"
  | "Your account";

// Sidebar entries are react-router <NavLink>s (role="link"), not <button>s.
// Their accessible name can carry trailing content beyond the label — Runs/
// Approvals a numeric badge ("Runs 2"), Getting started a StatusChip word
// ("Getting started Ready" / "…Checking…") — so match by prefix rather than
// an exact/suffix pattern.
export function sidebarLink(page: Page, label: NavLabel): Locator {
  return page.getByRole("link", { name: new RegExp(`^${label}`) });
}

// gotoConsole loads the app shell (pre-authed) and waits for the sidebar.
// The harness is a single-operator install (a bare admin bearer, no SSO), so
// "/" lands in the User view once onboarded (D1); pass "admin" to land in the
// Admin view, whose sidebar carries Policies, Permissions, Audit and the rest.
export async function gotoConsole(page: Page, view: "user" | "admin" = "user"): Promise<void> {
  await page.goto(view === "admin" ? "/admin" : "/");
  // "/" never stays "/": FirstRunLanding redirects to /runs or /setup once
  // status and role resolve. The sidebar mounts BEFORE that redirect fires, so
  // waiting on the sidebar alone returns with a Navigate still pending — and a
  // test that immediately pushes its own route can then have it clobbered by
  // the stale redirect (a race that widens under suite load; it cost a
  // member-console run at /runs/new). Console-ready means the landing settled.
  await page.waitForURL((u) => u.pathname !== "/" && u.pathname !== "/admin");
  await expect(sidebarLink(page, "Runs")).toBeVisible();
}

// navTo clicks a sidebar entry and returns once the click is registered.
export async function navTo(page: Page, label: NavLabel): Promise<void> {
  await sidebarLink(page, label).click();
}

// navToRoute reaches a screen that is NOT in the sidebar (a run detail page, a
// workspace, /runs/new). Use navTo for anything the sidebar actually lists;
// reaching a sidebar entry this way would stop proving the link works.
//
// This is a CLIENT-SIDE navigation, not page.goto(), and that distinction is
// load-bearing: a full document load re-runs the app's auth probe, so any test
// that has already installed a failing route intercept would never mount the
// shell at all. React Router listens to popstate, so pushState + popstate is
// exactly what a <NavLink> click does.
export async function navToRoute(page: Page, path: string): Promise<void> {
  await page.evaluate((p) => {
    window.history.pushState({}, "", p);
    window.dispatchEvent(new PopStateEvent("popstate"));
  }, path);
}

// ---------------------------------------------------------------------------
// New Run, the guided flow: Start, then Shape, Run, Harnesses, Files, Tools
// and access, Review, the Name dialog and the Launched screen. Every string is
// the copy module's, so a wording change breaks nothing here. A spec that is
// about something other than New Run reaches Review through walkGuidedToReview
// and launches through launchRun; a spec about one step drives that step itself.
// ---------------------------------------------------------------------------

const escapeRe = (s: string) => s.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
const startsWith = (s: string) => new RegExp(`^${escapeRe(s)}`);

export type NewRunShape = "agent_task" | "environment" | "application";
export type NewRunMode = "interactive" | "autonomous";

const SHAPE_TITLE: Record<NewRunShape, string> = {
  agent_task: NR_SHAPE.AGENT_TITLE,
  environment: NR_SHAPE.ENV_TITLE,
  application: NR_SHAPE.APP_TITLE,
};
const RUN_TITLE: Record<NewRunShape, string> = {
  agent_task: NR_RUN.TITLE_AGENT,
  environment: NR_RUN.TITLE_ENV,
  application: NR_RUN.TITLE_APP,
};
// The footer's one primary action: "Continue to <step>", and "Review run" from the last step.
const CONTINUE = new RegExp(`^(${escapeRe(NR_SHARED.CONTINUE_TO(""))}.+|${escapeRe(NR_SHARED.REVIEW_RUN)})$`);

/** A screen's h1: focus moves to it on every screen change, so it is the "arrived" signal. */
export const newRunHeading = (page: Page, title: string): Locator => page.getByRole("heading", { name: title, level: 1 });

/** Opens New Run from the shell's button, as a person does, and waits for the Start screen. */
export async function openNewRun(page: Page): Promise<void> {
  await page.getByRole("button", { name: "New run" }).click();
  await expect(newRunHeading(page, NR_START.TITLE)).toBeVisible();
}

/** Start screen, "Start guided setup": lands on Shape. */
export async function startGuided(page: Page): Promise<void> {
  await page.getByRole("button", { name: NR_START.GUIDED_FOOT }).click();
  await expect(newRunHeading(page, NR_SHAPE.TITLE)).toBeVisible();
}

export async function chooseShape(page: Page, shape: NewRunShape): Promise<void> {
  await page.getByRole("radio", { name: startsWith(SHAPE_TITLE[shape]) }).click();
}

/** The footer's primary action, then the next screen's heading as the arrived signal. */
export async function continueNewRun(page: Page, nextHeading: string): Promise<void> {
  await page.getByRole("button", { name: CONTINUE }).click();
  await expect(newRunHeading(page, nextHeading)).toBeVisible();
}

export async function chooseRunMode(page: Page, mode: NewRunMode): Promise<void> {
  const label = mode === "autonomous" ? NR_RUN.MODE_AUTONOMOUS : NR_RUN.MODE_INTERACTIVE;
  await page.getByRole("radio", { name: startsWith(label) }).click();
}

/** Harnesses step: one harness (the first the roster offers), and an Autonomous run's task. */
export async function addHarness(page: Page, task?: string): Promise<void> {
  await page.getByRole("button", { name: NR_HARNESSES.ADD_HARNESS, exact: true }).click();
  if (task !== undefined) await page.locator("#nr-task").fill(task);
}

/**
 * Files step: "No extra files" when `repos` is empty, otherwise each repository ticked and the first one
 * marked "Starts here". An untouched main drive size is filled, since a run with no size is not complete.
 */
export async function chooseFiles(page: Page, repos: readonly string[] = []): Promise<void> {
  if (repos.length === 0) {
    await page.getByRole("radio", { name: startsWith(NR_FILES.NO_EXTRA) }).check();
  } else {
    for (const repo of repos) await page.getByRole("checkbox", { name: NR_FILES.INCLUDE_ARIA(repo) }).check();
    await page.getByRole("radio", { name: new RegExp(`${escapeRe(repos[0])}.*${escapeRe(NR_FILES.STARTS_HERE)}`) }).check();
  }
  const size = page.locator("#nr-main-drive-size");
  if ((await size.count()) > 0 && (await size.inputValue()) === "") await size.fill("10");
}

export interface GuidedPlan {
  /** Default "environment": it needs no harness, task or command. */
  shape?: NewRunShape;
  /** An agent task's mode; default "interactive". */
  mode?: NewRunMode;
  /** An application's command. */
  command?: string;
  /** An Autonomous run's task. */
  task?: string;
  /** Repository names to include on Files; none means "No extra files". */
  repos?: readonly string[];
}

/**
 * From the Start screen to the Files step with the fewest choices a complete run needs, nothing chosen on
 * Files yet. A spec about the Files step drives it from here.
 */
export async function walkGuidedToFiles(page: Page, plan: GuidedPlan = {}): Promise<void> {
  const shape = plan.shape ?? "environment";
  const mode = plan.mode ?? "interactive";
  await startGuided(page);
  await chooseShape(page, shape);
  await continueNewRun(page, RUN_TITLE[shape]);
  if (shape === "agent_task") await chooseRunMode(page, mode);
  if (shape === "application") await page.getByLabel(NR_RUN.COMMAND_LABEL).fill(plan.command ?? "echo e2e");
  await continueNewRun(page, NR_HARNESSES.TITLE);
  if (shape === "agent_task") await addHarness(page, mode === "autonomous" ? (plan.task ?? "e2e task") : undefined);
  await continueNewRun(page, NR_FILES.TITLE);
}

/** From the Start screen to Review. Stops there, so a spec can read the page, change a setting or launch. */
export async function walkGuidedToReview(page: Page, plan: GuidedPlan = {}): Promise<void> {
  await walkGuidedToFiles(page, plan);
  await chooseFiles(page, plan.repos);
  await continueNewRun(page, NR_TOOLS.TITLE);
  await continueNewRun(page, NR_REVIEW.TITLE);
}

/** Start's JSON door: paste a run document (the intent only), validate it and continue to Review. */
export async function reviewRunDocument(page: Page, intent: Record<string, unknown>): Promise<void> {
  await page.getByRole("button", { name: NR_START.DOC_JSON }).click();
  const doc = { api_version: TEMPLATE_API_VERSION, kind: RUN_DOCUMENT_KIND, intent };
  await page.getByLabel(NR_DOCUMENT.EDITOR_ARIA(NR_DOCUMENT.FORMAT_JSON)).fill(JSON.stringify(doc));
  await page.getByRole("button", { name: NR_DOCUMENT.VALIDATE }).click();
  await page.getByRole("button", { name: NR_DOCUMENT.CONTINUE_REVIEW }).click();
  await expect(newRunHeading(page, NR_REVIEW.TITLE)).toBeVisible();
}

/**
 * Answers New Run's own preflight clear. Review keeps Launch shut until the server's check passes, and
 * the hermetic backend's real answer (no model providers, no barrier) can hold it, so a case about what a
 * LAUNCH does answers the check itself.
 */
export async function mockPreflightClear(page: Page): Promise<void> {
  await page.route("**/api/v1/runs/preflight", (route) =>
    route.fulfill({ json: { enforced_confinement_class: "CC1", setup_items: [] } }),
  );
}

/** Review's Launch run, which opens the Name dialog. */
export async function openLaunchDialog(page: Page): Promise<Locator> {
  await page.getByRole("button", { name: NR_REVIEW.LAUNCH_RUN }).click();
  const dialog = page.getByRole("dialog", { name: NR_LAUNCH.TITLE });
  await expect(dialog).toBeVisible();
  return dialog;
}

/** Names the run in the dialog and presses Launch. The dialog stays open on a refusal. */
export async function submitLaunch(page: Page, title = "e2e run"): Promise<Locator> {
  const dialog = await openLaunchDialog(page);
  await dialog.getByLabel(NR_LAUNCH.TITLE_LABEL).fill(title);
  await dialog.getByRole("button", { name: NR_LAUNCH.LAUNCH, exact: true }).click();
  return dialog;
}

/** Launches from Review and follows "View run" to the run's detail page. */
export async function launchRun(page: Page, title = "e2e run"): Promise<void> {
  await submitLaunch(page, title);
  await page.getByRole("button", { name: NR_LAUNCH.VIEW_RUN }).or(page.getByRole("link", { name: NR_LAUNCH.VIEW_RUN })).click();
  await expect(page).toHaveURL(/\/runs\/[0-9a-f-]{8,}/, { timeout: 15_000 });
}

// LEGACY: the old four-panel wizard's helpers. new-run.spec.ts (E10b) is their last caller; delete
// this block with that spec at the cutover.
/** New Run's four panels (#1922). */
export type NewRunPanel = "run" | "workspace" | "access" | "policy";

const NEW_RUN_PANEL: Record<NewRunPanel, string> = { run: "Run", workspace: "Workspace", access: "Access", policy: "Policy" };

/**
 * Shows one of New Run's four panels through its panel nav, as a person does.
 * A control on a panel that is not on screen is hidden, so every spec that
 * drives New Run reaches its controls through here. Calling it for the panel
 * already on screen changes nothing.
 */
export async function goToNewRunPanel(page: Page, panel: NewRunPanel): Promise<void> {
  // The button's name may carry an issue count ("Run 1 issue").
  const step = page
    .getByRole("navigation", { name: "New run" })
    .getByRole("button", { name: new RegExp(`^${NEW_RUN_PANEL[panel]}\\b`) });
  if ((await step.getAttribute("aria-current")) !== "step") await step.click();
  await expect(step).toHaveAttribute("aria-current", "step");
}

/**
 * Opens New Run's custom policy source for editing, as a person does: the
 * Policy panel, then "Edit policy". The panel opens on its read view, so the
 * source field is not on screen until then. Returns the field.
 */
export async function editNewRunPolicy(page: Page): Promise<Locator> {
  await goToNewRunPanel(page, "policy");
  const source = page.getByLabel(/^Spec \((YAML|JSON)\)/);
  if ((await source.count()) === 0) await page.getByRole("button", { name: "Edit policy" }).click();
  await expect(source).toBeVisible();
  return source;
}


// #1200 compact cards: every CollapsibleCard on Admin Settings/Your account
// (collapsible-card.tsx) starts collapsed, so a spec that reads a card's
// content directly must open it first — the e2e counterpart of the unit
// suites' own `expandCard` (lib/test-dom.ts's `startsWith`). A prefix regex,
// not a plain substring: a bare "Model provider" also matches "Model
// providers"' own heading/button, and vice versa. Idempotent (checks
// aria-expanded first) so a spec can call it even on a card a deep link
// already force-opened (ado-connection.tsx's `#azure-devops` effect) without
// accidentally re-collapsing it.
export async function expandCard(page: Page, title: string): Promise<void> {
  const toggle = page.getByRole("button", { name: new RegExp(`^${title}( |$)`) });
  if ((await toggle.getAttribute("aria-expanded")) !== "true") {
    await toggle.click();
  }
}

// Uses the browser's actual stored credential, including token-field sign-ins.
// Returning only the response keeps bearer values out of assertion diagnostics.
export async function consoleAPI(page: Page, method: string, path: string, body?: unknown): Promise<{ status: number; text: string }> {
  return page.evaluate(async ({ key, method, path, body }) => {
    const token = sessionStorage.getItem(key) ?? localStorage.getItem(key);
    const response = await fetch(path, {
      method,
      headers: { Authorization: `Bearer ${token}`, "Content-Type": "application/json" },
      body: body === undefined ? undefined : JSON.stringify(body),
    });
    return { status: response.status, text: await response.text() };
  }, { key: TOKEN_KEY, method, path, body });
}

async function asRealPerson(page: Page, token: string, principal: string, role: string): Promise<void> {
  // A same-origin document lets us select storage before any console request.
  await page.goto("/healthz");
  await page.evaluate(([key, token]) => {
    sessionStorage.removeItem(key);
    localStorage.setItem(key, token);
  }, [TOKEN_KEY, token]);
  const response = await consoleAPI(page, "GET", "/api/v1/me");
  expect(response.status, response.text).toBe(200);
  expect(JSON.parse(response.text)).toMatchObject({
    principal, method: "token", role, operator: false,
    security_operator: role === "security_admin",
    user_type: { id: "standard" },
  });
}

export async function asRealMember(page: Page): Promise<void> {
  await asRealPerson(page, MEMBER_TOKEN, MEMBER_PRINCIPAL, "user");
}

export async function asRealSecurityAdmin(page: Page): Promise<void> {
  await asRealPerson(page, SECURITY_ADMIN_TOKEN, "e2e-security-admin", "security_admin");
}

// The second security admin, a different human from asRealSecurityAdmin's: its own principal and mailbox.
// The governance four-eyes spec needs a proposer and an approver who are two people.
export async function asRealSecurityAdmin2(page: Page): Promise<void> {
  await asRealPerson(page, SECURITY_ADMIN_2_TOKEN, "e2e-security-admin-2", "security_admin");
}

// Render-only splices for specs that supply deliberately hypothetical states.
// Requests still carry the operator token; use asRealMember for authorization.
export async function mockMemberRole(page: Page): Promise<void> {
  // CACHE-AND-SERVE, not route.fetch()+refulfill per match — the same reason
  // mockMemberSetupStatus below does it: a real round trip PER match races
  // Playwright disposing an in-flight route's response ("apiResponse.json:
  // Response has been disposed"). One real fetch, then every match is
  // fulfilled from the cached body.
  let cached: Record<string, unknown> | null = null;
  await page.route("**/api/v1/me", async (route) => {
    if (!cached) {
      const body = (await (await route.fetch()).json()) as Record<string, unknown>;
      body.role = "user";
      body.operator = false;
      body.security_operator = false;
      cached = body;
    }
    // TS cannot narrow a `let` captured across the await above; the `if` does.
    await route.fulfill({ json: cached! });
  });
  await mockMemberSetupStatus(page);
}

// The OTHER half of the member splice (W6 drift note). Splicing only /me left
// every member-lensed spec reading an OPERATOR's GET /setup/status: the
// redaction is server-side on !isOperator (internal/api/setup.go), and the
// harness's bearer token IS an operator there, so `checks_redacted` was absent,
// `checks` and `secrets.present` were populated and `runner.driver` was the real
// driver. Four of this release's member fixes — the `checks_redacted` gate on
// the Image-builder row, environment-step's narrowed `noDriver`, RunsMemberEmpty
// and the demo grid's `secretNames` gate — are all keyed on fields that only
// ever arrived UNREDACTED, so the member specs could not exercise any of them,
// which is how W6-3's security-admin twin survived a green suite.
//
// A MIRROR of redactSetupStatusForUser's structural drops, not a re-derivation
// of its value projections: `integrations` and `provider_access` are reduced
// server-side by rules whose inputs (the caller's own stored credentials) this
// side cannot see, and inventing them here would prove a render against a body
// no server produces. The drops below are the ones the
// console branches on, and each is exactly what that function writes.
export async function mockMemberSetupStatus(page: Page): Promise<void> {
  // CACHE-AND-SERVE, not route.fetch()+refulfill per match — the same reason
  // agents.spec.ts's own /setup/status splice does it: the landing redirect,
  // the shell's poll and a screen's own mount all hit this endpoint, and a real
  // round trip PER match races Playwright disposing an in-flight route's
  // response ("apiResponse.json: Response has been disposed"). One real fetch,
  // then every match is fulfilled from the cached body. The glob keeps the
  // trailing `*`: the console re-reads with `?recheck=1`.
  let cached: Record<string, unknown> | null = null;
  await page.route("**/api/v1/setup/status*", async (route) => {
    if (!cached) {
      const body = (await (await route.fetch()).json()) as Record<string, unknown>;
      body.checks = [];
      body.checks_redacted = true;
      body.providers = [];
      body.secrets = { present: [] };
      // Rebuilt from confinement_classes and the kubernetes bit ALONE, exactly
      // as the server rebuilds SetupRunner — so driver, confinement_substrates
      // and ephemeral_disk_enforcement are dropped by construction rather than
      // by a line somebody remembered to write. The classes survive redaction:
      // they are the barrier signal deriveReadiness reads for every role; the
      // bit is the one substrate fact a member's Vault remedy keys off.
      const runner = (body.runner ?? {}) as { confinement_classes?: string[]; kubernetes?: boolean };
      body.runner = {
        driver: "",
        confinement_classes: runner.confinement_classes ?? [],
        ...(runner.kubernetes ? { kubernetes: true } : {}),
      };
      body.bedrock = { ready: !!(body.bedrock as { ready?: boolean } | undefined)?.ready };
      body.scm = {};
      body.host_proxy = {};
      body.deployment = {};
      cached = body;
    }
    // TS cannot narrow a `let` captured across the await above; the `if` does.
    await route.fulfill({ json: cached! });
  });
}

// Render-only security tier: server authorization is unchanged by this splice.
// Use asRealSecurityAdmin when a refusal or ownership check is under test.
export async function mockSecurityAdminRole(page: Page): Promise<void> {
  // CACHE-AND-SERVE, not route.fetch()+refulfill per match — the same reason
  // mockMemberSetupStatus above does it: a real round trip PER match races
  // Playwright disposing an in-flight route's response ("apiResponse.json:
  // Response has been disposed"). One real fetch, then every match is
  // fulfilled from the cached body.
  let cached: Record<string, unknown> | null = null;
  await page.route("**/api/v1/me", async (route) => {
    if (!cached) {
      const body = (await (await route.fetch()).json()) as Record<string, unknown>;
      body.role = "security_admin";
      body.operator = false;
      body.security_operator = true;
      cached = body;
    }
    // TS cannot narrow a `let` captured across the await above; the `if` does.
    await route.fulfill({ json: cached! });
  });
}

// Some specs seed state the API can't create (e.g. an approval — `POST
// /internal/approvals` needs a run-scoped token, not the admin one), so they talk
// to the backend's own Postgres via `docker exec`, mirroring the seeding scripts.
// Both drivers always set these env vars (scripts/run-ui-e2e.sh points at the e2e
// DB, scripts/screenshots.sh at its own wardyn_shots so a capture never clobbers
// it), so the defaults only matter when a spec is run by hand.
const PG_CONTAINER = process.env.WARDYN_E2E_PG_CONTAINER || "wardyn-test-pg";
const PG_DBNAME = process.env.WARDYN_E2E_PG_DBNAME || "wardyn_e2e";

// Run one SQL statement against that Postgres. -tA gives tuple-only, unaligned
// output so the caller can parse a single scalar trivially.
export function sql(statement: string): string {
  return execFileSync(
    "docker",
    [
      "exec",
      "-i",
      PG_CONTAINER,
      "psql",
      "-U",
      "wardyn",
      "-d",
      PG_DBNAME,
      "-v",
      "ON_ERROR_STOP=1",
      "-tAc",
      statement,
    ],
    { encoding: "utf8" },
  ).trim();
}
