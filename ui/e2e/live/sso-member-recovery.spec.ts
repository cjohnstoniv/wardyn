/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

/*
 * THE LIVE AWS SSO WALK, PART TWO — recovery, the console-driven org standard,
 * and the paths sso-member.spec.ts cannot reach from a member who is already
 * signed in.
 *
 * Runs in the SAME invocation and against the SAME cluster as
 * ui/e2e/live/sso-member.spec.ts (scripts/kind-sso-walk.sh runs
 * `run-ui-e2e.sh sso-member sso-member-recovery`), immediately after it. Two
 * facts it inherits and does not re-establish:
 *
 *   - the member is `live`, under the CONTRADICTING pair 111111111111/DevPower
 *     that sso-member.spec.ts's P4 case healed onto. A `live` member has NO
 *     "Sign in to AWS" button — MODEL_ACCESS_ACTIONABLE is
 *     {not_configured, expired_signin, expiring} — so every case here that
 *     needs to DRIVE a sign-in calls makeMemberActionable() first (helpers.ts
 *     explains the flip);
 *   - the Dex sessions are NOT inherited. Playwright gives each spec file its
 *     own browser context, so every case below signs its principal in again.
 *
 * ── EXECUTION ORDER IS LOAD-BEARING ─────────────────────────────────────────
 * The letters below are the REPORT's topics, not the order. The order is:
 *
 *   B → A → A(rail) → C → D → E → G → H → I → E2 → L0 → F → L
 *
 * B runs FIRST because it reads the member's SIGNED-IN card while the member is
 * still `live` from the previous file — case A's console save is what flips the
 * pin and takes that state away. F runs LAST because it signs the ADMIN in to
 * AWS, which breaks sso-member.spec.ts:"the capture belongs to the member
 * alone"'s admin-stays-`not_configured` invariant for anything after it.
 *
 * 0.7.6 adds I and E2, and their slot is the reason they are where they are: I
 * ends by SIGNING THE MEMBER IN (it drives the strip's own door), so it must
 * come after G and H, which need a `live` member to launch an agent run at all;
 * E2 RESTARTS THE DAEMON to re-point its agent-image map, so it goes after
 * everything that would rather not be interrupted and restores the map in a
 * `finally`. Both leave the member where the next case needs them: I heals to
 * `live`, E2's sign-in fails and leaves the member actionable, and F is an
 * ADMIN case that cares about neither.
 *
 * A leaves the member ACTIONABLE (expired_signin) and C consumes that window;
 * C heals the member back to `live`, and D and E each call
 * makeMemberActionable() for their own.
 *
 * 0.7.7 adds L0 and L. L0 CAPTURES the admin's own AWS session and then lapses
 * it (ensureActionable's roster-pin flip — 0.7.8: a never-captured admin never
 * emitted harness_credential_aws at all, so the case proved nothing about the
 * row it was named for), so it sits right before F, which re-signs the admin
 * in from whatever L0 left them at. L makes its own lapse (makeMemberActionable)
 * and ends by HEALING the member through a completed sign-in, so it goes where
 * nothing after it needs the member lapsed.
 *
 * ── NOTHING IS SKIPPED HERE ─────────────────────────────────────────────────
 * Every case runs live (D and E were flipped when lane `login-pane` merged).
 * The spec always executes tests, so it is deliberately NOT on
 * WARDYN_E2E_ALLOW_ALL_SKIPPED.
 *
 * Self-skips without WARDYN_TEST_K8S=1, same as its sibling.
 */

import { expect, test, type APIRequestContext, type Page } from "@playwright/test";
import { execFileSync } from "node:child_process";
import { USER_PREVIEW } from "../../src/app/components/wardyn/copy/console-view";
import { LOGIN_SANDBOX_NOTE } from "../../src/app/components/screens/run-detail/login-sandbox-note";
import { CAPTURE_NOT_CORROBORATED } from "../../src/app/components/screens/settings/capture-confirm";
import { LOGIN_SANDBOX_UNREADABLE, SIGNIN_PROGRESS } from "../../src/app/components/screens/settings/login-pane-copy";
import {
  LOGIN_SANDBOX_SLOW_START,
  LOGIN_SANDBOX_READ_RETRYING,
} from "../../src/app/components/screens/settings/login-start-wait";
// 0.7.6 lane starting-detail (finding 6). run-status-detail.ts is deliberately
// CSS-free and component-free so a Playwright spec can import it — the same
// rule helpers.ts states for SELFRUN_MARKER.
import { STARTING_UNSCHEDULABLE } from "../../src/app/components/screens/run-status-detail";
// 0.7.6 lanes ui-model-access-door (the strip) and ui-new-run-model-access (the
// rail), by constant name from local/v076/canon/*-docs.md.
import { MODEL_ACCESS_BANNER, RAIL_MODEL_ACCESS } from "../../src/app/components/wardyn/model-access-copy";
import {
  MEMBER_GETTING_STARTED,
  RAIL_CREDENTIAL,
  RAIL_RECORDING_ON,
  RECORDING_DISABLED_TITLE,
  YOUR_MODEL_KEY,
} from "../../src/app/components/wardyn/copy";
import { AGENTS, AGENTS_DRAFT, PROVIDERS } from "../../src/app/lib/workspace-providers-copy";
import {
  ADMIN_EMAIL,
  LOGIN_DONE,
  MEMBER_EMAIL,
  SANDBOX_UP,
  SSO_START_URL,
  awaitCapture,
  awaitSelfRunStarted,
  dexSignIn,
  dexSignOut,
  getRoster,
  launchAgentRun,
  makeMemberActionable,
  me,
  modelAccess,
  openLoginPane,
  otherPin,
  ownAWSRow,
  runIDFromURL,
  seen,
  signInThroughPane,
} from "./helpers";

test.skip(process.env.WARDYN_TEST_K8S !== "1", "live cluster walk: set WARDYN_TEST_K8S=1 (scripts/kind-sso-walk.sh)");
test.describe.configure({ mode: "serial" });

// Lane `login-pane` merged (feat/v0.7.5 da115293): cases D and E are LIVE and
// assert THROUGH its exported constants. LOGIN_SANDBOX_READ_RETRYING is case E's
// real negative control: "Wardyn can read the sandbox" and "Wardyn can't read it
// right now" are the two halves the case exists to tell apart, so asserting only
// the absence of the terminal UNREADABLE error would miss a wait that had
// silently flipped to retrying.

/** The 409 body of POST /setup/harness-login inside the no-credential preview
 *  (internal/api/membermode_preview.go's userViewPreviewSignInRefusal — Go-side
 *  and unexported, so there is no TS constant to import; lane member-preview's
 *  canon hands the spelling over verbatim and pins it from the Go side). */
const MEMBER_PREVIEW_SIGNIN_REFUSAL =
  "Exit the user view to sign in to AWS — the capture would land on your own identity.";

/** The kind context/namespace the walk installed into, so case E can taint the
 *  node and read the run pod's phase. scripts/kind-sso-walk.sh exports both. */
const KUBE_CONTEXT = process.env.WARDYN_LIVE_KUBE_CONTEXT || "kind-wardyn-quickstart";
// Where RUN pods land (k8s.runsNamespace) — not the release namespace.
const KUBE_NAMESPACE = process.env.WARDYN_LIVE_KUBE_NAMESPACE || "wardyn-runs";
const KUBE_NODE = process.env.WARDYN_LIVE_KUBE_NODE || "wardyn-quickstart-control-plane";
/** The RELEASE namespace (the daemon Deployment), not the runs one. Case E2
 *  re-points the daemon's agent-image map, which is boot env on that Deployment.
 *  Defaulted rather than exported by the walk, which currently exports only the
 *  three coordinates the taint case needed — overridable for the same reason
 *  those are: a renamed install must red this case, not make it vacuous. */
const KUBE_RELEASE_NAMESPACE = process.env.WARDYN_LIVE_KUBE_RELEASE_NAMESPACE || "wardyn";
const KUBE_RELEASE = process.env.WARDYN_LIVE_KUBE_RELEASE || "wardyn";
const COLDPULL_TAINT = "wardyn-coldpull=1:NoSchedule";

/** `stdio: "pipe"`, deliberately: an untaint of a node that is not tainted
 *  exits 1 and writes to stderr, and inheriting it spams the report after every
 *  single test. Callers that tolerate failure use kubectlOrEmpty(). */
function kubectl(...args: string[]): string {
  return execFileSync("kubectl", ["--context", KUBE_CONTEXT, ...args], { encoding: "utf8", stdio: "pipe" }).trim();
}

/** A kubectl read that may legitimately have nothing to read.
 *
 *  `kubectl get pod <name>` on a pod that does not exist EXITS 1, so
 *  execFileSync throws — and a throw inside `expect.poll` aborts the poll
 *  instead of retrying it. Case E polls for a pod the control plane has not
 *  created yet, so "not there yet" has to read as "" rather than as a failure. */
function kubectlOrEmpty(...args: string[]): string {
  try {
    return kubectl(...args);
  } catch {
    return "";
  }
}

/** The daemon's WARDYN_AGENT_IMAGES map, as the running Deployment holds it. */
function agentImagesEnv(): string {
  return kubectlOrEmpty(
    "-n",
    KUBE_RELEASE_NAMESPACE,
    "get",
    "deployment",
    KUBE_RELEASE,
    "-o",
    `jsonpath={.spec.template.spec.containers[0].env[?(@.name=="WARDYN_AGENT_IMAGES")].value}`,
  );
}

/** Write that map back and WAIT for the new pod to serve.
 *
 *  `kubectl set env` edits the Deployment's own env entry in place (the chart
 *  renders it as a literal value, which is why kind-sso-walk.sh can read it with
 *  the same jsonpath), so this is one rollout and no Helm involvement. Waited
 *  for, always: the rollout is what makes the change real, and a case that
 *  launched against the OLD pod would prove nothing while looking green. */
function setAgentImagesEnv(images: string): void {
  kubectl("-n", KUBE_RELEASE_NAMESPACE, "set", "env", `deployment/${KUBE_RELEASE}`, `WARDYN_AGENT_IMAGES=${images}`);
  kubectl("-n", KUBE_RELEASE_NAMESPACE, "rollout", "status", `deployment/${KUBE_RELEASE}`, "--timeout=300s");
}

/** The member, in a state where a sign-in can be STARTED.
 *
 *  makeMemberActionable() FLIPS the pin, so calling it on a member who is
 *  already contradicted would HEAL them instead — the cases below can arrive
 *  either way (a red earlier in the file leaves its own state), so the flip is
 *  conditional and the postcondition is asserted rather than assumed. */
async function ensureActionable(page: Page, request: APIRequestContext): Promise<void> {
  if ((await modelAccess(page)).state === "live") await makeMemberActionable(request);
  await expect.poll(async () => (await modelAccess(page)).state, { timeout: 120_000 }).not.toBe("live");
}

type RunRow = { id: string; task?: string; state?: string; created_at?: string };

/** The caller's own `harness login` runs, NEWEST FIRST.
 *
 *  GET /runs is already scoped to the caller for a member (fail-closed, not a
 *  narrowed admin list), and `harness login` is the server-side task
 *  discriminator (harnessLoginTask). Sorted here rather than trusted: the
 *  handler's ordering is not part of this file's contract, and "the newest
 *  login run" is the only row every case below means. */
async function myLoginRuns(page: Page): Promise<RunRow[]> {
  const rows = await page.evaluate(async () => {
    const r = await fetch("/api/v1/runs?limit=50", { credentials: "include" });
    return (await r.json()) as RunRow[];
  });
  return rows
    .filter((r) => r.task === "harness login")
    .sort((a, b) => Date.parse(b.created_at ?? "") - Date.parse(a.created_at ?? ""));
}

/** Open the login run the CALLER just started, from the Runs list.
 *
 *  BOTH HALVES OF THIS ARE THE FIX FOR A REAL TRAP. By the time this file runs,
 *  the member already OWNS two KILLED `harness login` runs from
 *  sso-member.spec.ts (the pane kills a login run the moment it corroborates
 *  the capture), so:
 *
 *   - "wait until a login run exists" is ALREADY true and returns instantly,
 *     which is why every caller passes the set of ids it saw BEFORE opening the
 *     pane and waits for one that is not in it. That wait doubles as the
 *     barrier that stops a `page.goto` firing while POST /setup/harness-login
 *     is still in flight — a navigation would abort it;
 *   - picking the FIRST "harness login" card on the board picks the wrong run:
 *     runs.tsx orders `attention, active, done`, and KILLED ranks as attention,
 *     so the top card is a dead run from the previous file. The card is
 *     selected by the run id on its own id chip (`title={run.id}`,
 *     run-card.tsx) instead of by position. */
async function openLoginRunCard(page: Page, runID: string): Promise<void> {
  await page.goto("/runs");
  await page
    .getByTestId("run-card")
    .filter({ has: page.locator(`[title="${runID}"]`) })
    .getByText("harness login")
    .click();
  await expect(page).toHaveURL(new RegExp(`/runs/${runID}$`), { timeout: 60_000 });
}

/** The caller's newest login run that was NOT in `prior` — see openLoginRunCard. */
async function newLoginRun(page: Page, prior: Set<string>): Promise<RunRow> {
  await expect
    .poll(async () => (await myLoginRuns(page)).some((r) => !prior.has(r.id)), { timeout: SANDBOX_UP })
    .toBe(true);
  return (await myLoginRuns(page)).filter((r) => !prior.has(r.id))[0];
}

/** Approvals the member can read for THEIR OWN run (routes.go's classMember +
 *  approvals.go's getRunAuthorized gate). A bare JSON array. */
async function approvalsFor(page: Page, runID: string): Promise<unknown[]> {
  return page.evaluate(async (id: string) => {
    const r = await fetch(`/api/v1/approvals?run_id=${encodeURIComponent(id)}`, { credentials: "include" });
    if (!r.ok) throw new Error(`GET /approvals?run_id=: ${r.status}`);
    return (await r.json()) as unknown[];
  }, runID);
}

/** The Agents tab, from an operator's browser session. Same route the hermetic
 *  ui/e2e/agents.spec.ts drives; spelled out here because that file's fixtures
 *  carry the e2e daemon's bearer token and base URL, neither of which applies
 *  on a cluster driven through Dex. */
async function gotoAgentsTab(page: Page): Promise<void> {
  // NEVER `page.goto("/admin/settings")` as the admin. App.tsx's RequireSetup
  // bounces the FIRST gated-route render of every full document load into
  // /setup while any setup check grades fail or warn — which a fresh kind
  // install always does — and only ONCE per load (setup-gate.ts's
  // gateFiredThisLoad). 0.7.5's first green-looking walk sat 30 minutes on the
  // welcome page for exactly this.
  //
  // AND NEVER TIME THE BOUNCE. The gate decides when /setup/status and /me have
  // BOTH answered, not when the URL changes: the second attempt here loaded
  // /runs, waited for "/runs or /setup" — which the URL satisfies the instant
  // goto returns, before either answer — pushed /admin/settings, and the gate
  // then spent its one bounce ON /admin/settings. So navigate client-side
  // (pushState + popstate, what a <NavLink> click does — ui/e2e/fixtures.ts's
  // navToRoute; a second full load would re-arm the gate) and let the page say
  // when it took: the Settings card only paints once the gate has let the
  // route through, and if the one bounce landed on top of this navigation the
  // retry cannot be bounced again. Correct whichever side of the gate's
  // decision it starts on.
  await page.goto("/runs");
  await expect(async () => {
    if (!/\/admin\/settings$/.test(page.url())) {
      await page.evaluate((path) => {
        window.history.pushState({}, "", path);
        window.dispatchEvent(new PopStateEvent("popstate"));
      }, "/admin/settings");
    }
    await expect(page.getByTestId("providers-card")).toBeVisible({ timeout: 5_000 });
  }).toPass({ timeout: 90_000 });
  await page.getByTestId("providers-card").getByText(PROVIDERS.CARD_OPEN).click();
  await expect(page).toHaveURL(/\/admin\/providers$/);
  await page.getByRole("button", { name: AGENTS.AGENTS_TITLE }).click();
}

/** An ADMIN's own AWS sign-in, which is NOT on /setup.
 *
 *  /setup renders the operator Getting Started for an operator; the member's
 *  CTA lives on member-getting-started.tsx. The admin's own sign-in is the
 *  Agents tab's model-access block (agents-tab.tsx), which mounts the SAME
 *  HarnessLoginPane with the same startURLManaged suppression. */
async function openAdminLoginPane(page: Page): Promise<void> {
  await gotoAgentsTab(page);
  await page.getByRole("button", { name: AGENTS.SIGN_IN_AWS }).first().click();
  const start = page.getByRole("button", { name: "Start login" });
  if (await start.isVisible().catch(() => false)) await start.click();
}

// A red must never leave the cluster tainted — not for the cases after it, and
// not for the next walk. `-` is kubectl's remove suffix and a no-op when the
// taint is not there, so this is safe after every case, not just case E.
test.afterEach(() => {
  if (process.env.WARDYN_TEST_K8S !== "1") return;
  try {
    kubectl("taint", "nodes", KUBE_NODE, "wardyn-coldpull-");
  } catch {
    /* not tainted — the ordinary case */
  }
});

// ── B — the member's own card, while they are still signed in ───────────────

test("B (ui-member-model-key): a per_user member's card names their OWN AWS sign-in", async ({ page }) => {
  // FIRST, and that is not cosmetic: the member is `live` only until case A's
  // console save moves the pin. The before-sign-in half of this card is proven
  // by sso-member.spec.ts's second case plus the lane's own vitest matrix — the
  // ONE thing only a live walk can show is the SIGNED-IN branch under a real
  // per_user roster row, with a real captured session behind it.
  await dexSignIn(page, MEMBER_EMAIL);
  expect((await modelAccess(page)).state, "sso-member.spec.ts must leave the member live").toBe("live");

  await page.goto("/setup");
  await expect(page.getByText(YOUR_MODEL_KEY.SIGNED_IN_CHIP).first()).toBeVisible({ timeout: 60_000 });
  await expect(page.getByText(YOUR_MODEL_KEY.SIGNED_IN_BODY)).toBeVisible();
  // The 0.7.4 sentence this replaced — "Model access is already configured for
  // you", under the chip "Provided by your admin" — is the field report's
  // finding 2 verbatim. Its ABSENCE is the assertion.
  await expect(page.getByText(YOUR_MODEL_KEY.PROVIDED_BODY)).toHaveCount(0);
  await expect(page.getByText(YOUR_MODEL_KEY.PROVIDED_CHIP)).toHaveCount(0);
  // …and the page lede names whose sign-in it is.
  await expect(page.getByText(MEMBER_GETTING_STARTED.SETUP_SUMMARY_HELPER_PER_USER)).toBeVisible();
  await dexSignOut(page);
});

// ── A — the org standard AND the org settings, set in the console ───────────

test("A: an admin sets the org's agent standard in the console and a member is bound by it", async ({
  page,
  request,
}) => {
  // The ONE UI-driven roster save on the walk (every other one is the API PUT,
  // for speed). It is the console half of the owner's E2E goal: the admin never
  // touches an API, and what a member then sees is the consequence.
  //
  // IT IS ALSO THE makeMemberActionable() FLIP. The pair saved here is the
  // fixture's OTHER valid one (helpers.ts explains why the other VALID one),
  // so the member's stored capture stops matching the pin, they grade
  // `expired_signin`, and the "Sign in to AWS" CTA comes back — which is what
  // (iii) below and case C both need.
  const pin = await otherPin(request);

  await dexSignIn(page, ADMIN_EMAIL);
  await gotoAgentsTab(page);

  const row = page.getByTestId("agent-row-claude-code");
  await expect(row).toBeVisible();
  await row.getByRole("radio", { name: AGENTS.MECHANISM_BEDROCK_SSO }).click();
  await row.getByRole("radio", { name: AGENTS.SOURCE_PER_USER }).click();
  // The three ORG SETTINGS the row carries, typed into the console.
  await row.getByLabel(AGENTS.FIELD_SSO_START_URL).fill(SSO_START_URL);
  await row.getByLabel(AGENTS_DRAFT.FIELD_SSO_ACCOUNT_ID).fill(pin.account);
  await row.getByLabel(AGENTS_DRAFT.FIELD_SSO_ROLE_NAME).fill(pin.role);

  // …and the field report's deployment shape: ONE enabled row.
  for (const display of ["Codex CLI", "Your own tools"]) {
    const other = page.getByRole("switch", { name: `${PROVIDERS.FIELD_ENABLED} — ${display}` });
    if ((await other.count()) > 0 && (await other.isChecked())) await other.click();
  }

  // Press Save and WAIT FOR THE WRITE — the success toast is raised only once
  // the PUT has resolved 200 (agents.spec.ts's saveAgents explains why an
  // "error has not rendered" assertion is not a write barrier).
  await page.getByRole("button", { name: PROVIDERS.SAVE_CTA }).click();
  await expect(page.getByText(PROVIDERS.SAVED_TOAST)).toBeVisible({ timeout: 60_000 });
  await expect(page.getByText(PROVIDERS.SAVE_ERROR)).toHaveCount(0);

  // The save ROUND-TRIPS: the server holds what the console showed.
  const saved = (await getRoster(request)).find((a) => a.id === "claude-code");
  expect(saved, "the console save did not reach GET /agent-providers").toMatchObject({
    mechanism: "bedrock_sso",
    credential_source: "per_user",
    sso_account_id: pin.account,
    sso_role_name: pin.role,
  });
  // …and ONE row is enabled, which is this deployment's whole shape.
  //
  // THE WIRE FIELD IS `disabled`, NOT `enabled` (types.AgentProviders). An
  // earlier draft read `.enabled` and compared it to `false`: that is
  // `undefined === false` on every row, so it asserted nothing — and it would
  // not have caught a save that silently re-enabled the other two. Reading the
  // whole enabled SET, rather than each row, is also what makes this fail if a
  // future catalog id appears and defaults on.
  expect(
    (await getRoster(request)).filter((a) => !a.disabled).map((a) => a.id),
    "this deployment must carry exactly one enabled agent row",
  ).toEqual(["claude-code"]);
  await dexSignOut(page);

  // ── and now the MEMBER, bound by all of it ────────────────────────────────
  await dexSignIn(page, MEMBER_EMAIL);

  // (i) the standard reached them: New Run offers the one enabled agent, and
  // names the reason for the other rather than hiding it.
  await page.goto("/runs/new");
  await page.getByRole("radio", { name: /^Autonomous/ }).click();
  await page.getByRole("combobox", { name: "Agent" }).click();
  await expect(page.getByRole("option", { name: "Claude Code" })).toBeEnabled();
  const codex = page.getByRole("option", { name: new RegExp(`Codex CLI.*${AGENTS.UNAVAILABLE}`) });
  await expect(codex).toBeVisible();
  await expect(codex).toHaveAttribute("aria-disabled", "true");
  await page.keyboard.press("Escape");

  // (iii) an ORG SETTING is ENFORCED on the member, not merely saved: the pin
  // flip has made them actionable, and their sign-in pane has NO start-URL
  // field at all — the org's portal wins (harness-login-pane.tsx's
  // startURLManaged, passed only by member-getting-started.tsx).
  await expect.poll(async () => (await modelAccess(page)).state, { timeout: 120_000 }).toBe("expired_signin");
  await page.goto("/setup");
  const cta = page.getByRole("button", { name: AGENTS.SIGN_IN_AWS }).first();
  await expect(cta).toBeVisible({ timeout: 60_000 });
  await cta.click();
  await expect(page.getByTestId("login-intro")).toBeVisible({ timeout: 60_000 });
  await expect(page.getByText(AGENTS.SSO_START_URL_MANAGED)).toBeVisible();
  await expect(page.getByTestId("login-start-url-prompt")).toHaveCount(0);
  await expect(page.getByLabel(AGENTS.FIELD_SSO_START_URL)).toHaveCount(0);
  // Leave the pane WITHOUT launching: case C opens its own sandbox, and a
  // second live device code would be the one thing this walk must not mint.
  await page.getByRole("button", { name: "Cancel" }).first().click();
});

test("A(rail): the New Run rail states THIS run's credential residency, with no click", async ({ page }) => {
  // The field report's finding 1, live: on a per_user/bedrock_sso estate the
  // rail must say the AWS credential is RESIDENT — the opposite of 0.7.4's
  // unconditional "Never written into the sandbox" — and it must say so on the
  // DEFAULT path, before any Preflight click, because that is the state a
  // member is in while deciding whether to sign in at all.
  //
  // Lane ui-rail-truth's handoff: /setup/status's harness row carries
  // credential_residency "sandbox" for this roster shape whether or not the
  // member has signed in, which is the only shape the status surface grades.
  await dexSignIn(page, MEMBER_EMAIL);
  await page.goto("/runs/new");
  await page.getByRole("radio", { name: /^Autonomous/ }).click();

  await expect(page.getByText(RAIL_CREDENTIAL.SANDBOX_BEDROCK)).toBeVisible({ timeout: 60_000 });
  await expect(page.getByText(RAIL_CREDENTIAL.SANDBOX_BEDROCK_CHIP_PER_USER)).toBeVisible();
  // …and Recording states the truth about a stock Helm install rather than
  // promising a capture that cannot happen: the kind quickstart leaves
  // persistence.enabled=false. The Recording row reads /healthz, so it arrives
  // on that answer rather than on mount — poll it rather than racing it.
  await expect(page.getByText(RECORDING_DISABLED_TITLE)).toBeVisible({ timeout: 60_000 });
  // 0.7.4's unconditional Recording sentence — which survives as the ENABLED
  // arm's own copy, so U-15 made it a constant (RAIL_RECORDING_ON) and this
  // asserts through it: on this estate recording is off, and the promise must
  // not be on screen beside the sentence that says so.
  await expect(page.getByText(RAIL_RECORDING_ON)).toHaveCount(0);
  // …and the honest-absence arm is NOT what rendered: this estate's roster row
  // settles residency without a dry run, so RESOLVED_AT_LAUNCH belongs to every
  // OTHER estate and would be the quiet failure here.
  await expect(page.getByText(RAIL_CREDENTIAL.RESOLVED_AT_LAUNCH)).toHaveCount(0);

  // ── A(rail)+ — 0.7.6 finding 1: the rail names the PERSON's state ─────────
  //
  // Case A left this member `expired_signin` (the pin flip), so the state the
  // rail must name here is the LAPSE, not the first run. The `not_configured`
  // sentence has exactly one seat on this walk — before the member's first
  // capture — and is asserted there, in sso-member.spec.ts's case I, together
  // with the `NO_PROVIDER` negative below.
  await expect(page.getByText(RAIL_MODEL_ACCESS.EXPIRED)).toBeVisible({ timeout: 60_000 });
  // THE FINDING-1 NEGATIVE, in the state the field report was written about:
  // this deployment IS connected (the admin's per_user roster row exists), so
  // the deployment-level sentence would be a falsehood on this estate. 0.7.5
  // rendered nothing at all here; the assertion pins that the fix did not
  // instead over-fire the other warning.
  await expect(page.getByText(RAIL_MODEL_ACCESS.NO_PROVIDER)).toHaveCount(0);
  // Two controls, DISTINCT accessible names (U-13's actual rule). The rail's
  // own is an aria-label, so it is the accessible name Playwright matches; the
  // strip's button is gone here because the rail CLAIMED the door, and `exact`
  // is what makes that assertion mean anything — the default name match is a
  // substring, and the rail's visible LABEL is the strip's name.
  await expect(page.getByRole("button", { name: RAIL_MODEL_ACCESS.SIGN_IN_ARIA })).toBeVisible();
  await expect(page.getByRole("button", { name: AGENTS.SIGN_IN_AWS, exact: true })).toHaveCount(0);
});

// ── C — the sandbox signs itself in, and the Runs list joins that session ───

test("C (login-sandbox-selfrun): the sign-in sandbox runs the pair itself and the Runs list joins it", async ({
  page,
}) => {
  // Continues from A(iii): the member is `expired_signin`, so the CTA exists.
  //
  // THE NEGATIVE CONTROL IS THE ABSENCE OF `page.keyboard.type` ANYWHERE IN
  // THIS CASE — and in every other case that drives a SIGN-IN SANDBOX. (Case H
  // types, deliberately: it is a claude-code run's own CLI prompt, not a login
  // pane, and nothing there is signing anything in.)
  // Before 0.7.5 the console typed the chained command into the pane
  // and a Runs-list attach got a bare prompt; now the IMAGE creates the `wardyn`
  // tmux session on signin-pane.sh and runs the pair once, and every attach path
  // — this pane, the Runs list, `wardyn attach`, ssh — joins that one session
  // (attaching is `tmux new-session -A`). If this case ever needs a keystroke to
  // pass, the feature is not there.
  await dexSignIn(page, MEMBER_EMAIL);
  // The member ALREADY owns two KILLED login runs from sso-member.spec.ts, so
  // "a login run exists" says nothing — snapshot the ids first and wait for one
  // that is NOT among them. See openLoginRunCard() for the two bugs that fixes.
  const prior = new Set((await myLoginRuns(page)).map((r) => r.id));
  await openLoginPane(page);

  // Leave the console behind IMMEDIATELY — but only once the new run EXISTS.
  // Nothing server-side stops a login run on capture (the shutdown is this
  // pane's own killRun), so navigating away is what makes the rest of this case
  // the RUNS-LIST path rather than the console one; navigating away too early
  // would abort the POST that creates the run.
  const loginRun = await newLoginRun(page, prior);

  // The run is openable from the list by the task the server stamped on it
  // (harnessLoginTask), which is the whole point of labelling it: opening it
  // from /runs is not a mystery box.
  await openLoginRunCard(page, loginRun.id);
  expect(runIDFromURL(page)).toBe(loginRun.id);
  // …and the run page NAMES the box, including that the sign-in is already
  // running in it and what ends it (LOGIN_SANDBOX_NOTE, imported).
  await expect(page.getByText(LOGIN_SANDBOX_NOTE)).toBeVisible({ timeout: 60_000 });

  // terminal-notice.tsx: attachable = interactive && RUNNING && own, and a
  // member owns their own login run.
  const screen = page.locator(".xterm-screen").first();
  await expect(screen, "the member could not attach to their own sign-in sandbox from /runs").toBeVisible({
    timeout: SANDBOX_UP,
  });

  // The sandbox's OWN output, not an echo of an argv nothing prints: the
  // image's banner (SELFRUN_MARKER, imported — TestSelfRunBanner_UIParity pins
  // the shell side to it). THE STRICT banner assertion belongs here and only
  // here: there is no console pane on this page to tear the terminal down on
  // capture.
  //
  // NOT the device-code URL. AWS CLI 2.31 prints "visit the following URL" only
  // after a FIRST CreateToken answers authorization_pending, and the on-cluster
  // fake pre-approves every device code — so StartDeviceAuthorization is followed
  // by an immediate 200 and the CLI goes straight to "Successfully logged into"
  // (read off the sandbox's own tmux history and an `aws --debug` trace, walk-5).
  // An assertion on that line could only ever time out here; against real AWS
  // the first poll IS pending and the line prints.
  await awaitSelfRunStarted(screen);

  // …and it COMPLETED, with no console around it and nothing typed.
  await awaitCapture(page, screen);
  await expect.poll(async () => (await modelAccess(page)).state, { timeout: 120_000 }).toBe("live");
  // The stored blob is THIS run's — source_run_id survives redaction on the
  // caller's own per-user AWS row alone, which is what lets a member
  // corroborate their own sign-in at all.
  await expect.poll(async () => (await ownAWSRow(page)).source_run_id, { timeout: 120_000 }).toBe(loginRun.id);
});

// ── D — cancel, retry, and the supersede ────────────────────────────────────

test("D (login-pane): a cancelled sign-in retries cleanly, and a new one supersedes the orphan", async ({
  page,
  request,
}) => {
  await dexSignIn(page, MEMBER_EMAIL);
  await makeMemberActionable(request);
  await expect.poll(async () => (await modelAccess(page)).state, { timeout: 120_000 }).toBe("expired_signin");

  // "Reaches live" is VACUOUS here — the member was live when this case
  // started. The assertion is that the BLOB MOVED: source_run_id is the second
  // run's.
  const before = await ownAWSRow(page);

  // Cancel is the only control while the run is STARTING, and it kills the run
  // and closes the pane (harness-login-pane.tsx) — so it leaves NO blob and no
  // half-state for the retry to trip over.
  await openLoginPane(page);
  await page.getByRole("button", { name: "Cancel" }).first().click();
  await expect(page.getByTestId("harness-login-pane")).toHaveCount(0, { timeout: 60_000 });

  // The retry, completed. signInThroughPane() rather than a terminal
  // assertion: a boot-time sign-in against a pre-approving fake can finish
  // before the terminal is ever on screen (see there).
  await signInThroughPane(page, openLoginPane);

  const after = await ownAWSRow(page);
  expect(after.source_run_id, "the stored capture did not move to the retry's run").not.toBe(before.source_run_id);
  // source_run_id is the WHOLE witness, by design: redactSetupStatusForUser
  // keeps it on the caller's own per_user aws row and strips captured_at
  // ("operator credential-lifecycle detail"), so a member's session — the only
  // one this case may use — never sees a capture time to compare.
  // The pane corroborates a marker against the SERVER before it claims a
  // capture; this sentence is what it shows when the server disagrees. On a
  // healthy walk it must never render.
  await expect(page.getByText(CAPTURE_NOT_CORROBORATED)).toHaveCount(0);

  // ── the supersede, live ───────────────────────────────────────────────────
  // Start a sign-in, navigate away WITHOUT Cancel (the orphan), start another.
  // The prior-id snapshot again: by now this member owns several terminal login
  // runs, so "a login run exists" is always true and would hand back a dead one.
  await makeMemberActionable(request);
  const priorOrphan = new Set((await myLoginRuns(page)).map((r) => r.id));
  await openLoginPane(page);
  const orphan = await newLoginRun(page, priorOrphan);
  await page.goto("/runs");
  await signInThroughPane(page, openLoginPane);
  await expect
    .poll(async () => (await myLoginRuns(page)).find((r) => r.id === orphan.id)?.state, { timeout: 120_000 })
    .toBe("KILLED");
  // …and NEVER MORE THAN ONE non-terminal login run is left for this member.
  // At most, not exactly: the second sign-in went through the console PANE,
  // which kills its own run the moment the capture is corroborated
  // (confirmCapture → killRun), so the honest count here is 0, or 1 for the
  // instant that kill is still in flight. "Exactly one" was this file's first
  // live red, against a product doing the right thing. (The run JSON carries no
  // reason field — the `superseded_by_new_login` reason is asserted on the
  // run.kill AUDIT row, in the Go test.)
  const open = (await myLoginRuns(page)).filter((r) => !["KILLED", "DONE", "FAILED", "TIMED_OUT"].includes(r.state ?? ""));
  expect(open.length, `more than one live sign-in sandbox for one member: ${open.map((r) => r.id).join(", ")}`).toBeLessThanOrEqual(1);
});

// ── E — a sign-in held in STARTING reads as slow, never as unreadable ───────

test("E (login-pane): a sign-in held 65 s in STARTING reads as slow, never as unreadable", async ({
  page,
  request,
}) => {
  // The live twin of the Go characterization test, and the datum that tells an
  // operator whether an UNREADABLE they saw was estate-side.
  //
  // THE HOLD IS MANUFACTURED BY A NODE TAINT, and the pod it parks is the
  // PROXY's, not the agent's. CreateSandbox creates `wardyn-proxy-<run id>`
  // FIRST and waits for its pod IP before the agent pod exists at all
  // (internal/runner/k8s's sandbox creation, `podIPWaitTimeout`), so under a
  // taint nothing schedules, the proxy pod sits Pending and `wardyn-agent-<id>`
  // is never created. An earlier draft of this case watched for the AGENT pod
  // and would have polled a name that cannot exist.
  //
  // AND THE BOUND IS THAT 90 s POD-IP WAIT, not the 3-minute canary: at 90 s
  // the RUN FAILS. So the hold is 65 s measured from RUN CREATION — five
  // seconds past the 60 s at which the slow-start sentence appears, and ~25 s
  // of margin before the run dies — and the untaint happens the instant the
  // assertions are made, not at the end of the case.
  kubectl("taint", "nodes", KUBE_NODE, COLDPULL_TAINT);

  await dexSignIn(page, MEMBER_EMAIL);
  await makeMemberActionable(request);
  const prior = new Set((await myLoginRuns(page)).map((r) => r.id));
  await openLoginPane(page);
  const loginRun = await newLoginRun(page, prior);
  const created = Date.parse(loginRun.created_at ?? "") || Date.now();

  // Without this the hold is vacuous — a pod that scheduled anyway would pass
  // it by simply working. kubectlOrEmpty, because `get pod` on a pod the
  // control plane has not created yet EXITS 1, and a throw inside expect.poll
  // aborts the poll instead of retrying it.
  await expect
    .poll(
      () =>
        kubectlOrEmpty(
          "-n",
          KUBE_NAMESPACE,
          "get",
          "pod",
          `wardyn-proxy-${loginRun.id}`,
          "-o",
          "jsonpath={.status.phase}",
        ),
      { timeout: 60_000 },
    )
    .toBe("Pending");

  // The pane's own 2 s GET /runs/{id} poll must stay readable for the whole
  // hold: it is an unreadable RUN, not a slow one, that raises the other
  // sentence, and telling those two apart is this case's entire subject.
  const codes: number[] = [];
  while (Date.now() < created + 65_000) {
    codes.push(
      await page.evaluate(async (id: string) => {
        const r = await fetch(`/api/v1/runs/${id}`, { credentials: "include" });
        return r.status;
      }, loginRun.id),
    );
    await page.waitForTimeout(2_000);
  }
  expect(codes.filter((c) => c !== 200), "the harness could not read the run for the whole hold").toHaveLength(0);

  // 0.7.6 FINDING 6, AND THIS IS A REPLACEMENT, NOT AN ADDITION (lane
  // starting-detail's handoff says so in as many words). The taint leaves the
  // PROXY pod Pending/Unschedulable — precisely waitingReason's
  // PodScheduled-condition fallback — so the pane's "slow" arm now renders the
  // SUBSTRATE's sentence instead of the hedged clock one: it names SCHEDULING
  // for a wait that is not a pull, which is the whole of finding 6 in one
  // assertion. The 65 s hold above is load-bearing for it: below
  // RUN_POLL_SLOW_START_MS (60 s) the pane shows only its first step, because
  // Unschedulable is NOT terminal and therefore grades on the clock.
  await expect(page.getByText(STARTING_UNSCHEDULABLE)).toBeVisible();
  // #628: the whole hold was narrated in the door itself, on its first step —
  // the person was never sent to a blank tab to wait.
  await expect(
    page.getByTestId("signin-progress").getByRole("listitem").filter({ hasText: SIGNIN_PROGRESS.STEP_START }),
  ).toHaveAttribute("data-state", "active");
  // …and the sentence it REPLACED is gone. Asserting only the new one would
  // pass on a pane that showed both, which is the thing finding 6 is against.
  await expect(page.getByText(LOGIN_SANDBOX_SLOW_START)).toHaveCount(0);
  // BOTH of the other two sentences. The pane's terminal error is one of them;
  // the starting-phase "can't read it right now" line is the other, and a wait
  // that had silently flipped to retrying would pass an assertion that only
  // looked for the first.
  await expect(page.getByText(LOGIN_SANDBOX_UNREADABLE)).toHaveCount(0);
  await expect(page.getByText(LOGIN_SANDBOX_READ_RETRYING)).toHaveCount(0);

  // IMMEDIATELY — every second after this is spent against the 90 s bound.
  kubectl("taint", "nodes", KUBE_NODE, "wardyn-coldpull-");
  await expect.poll(async () => (await modelAccess(page)).state, { timeout: LOGIN_DONE }).toBe("live");
});

// ── G — a first claude-code run parks on nothing ────────────────────────────

test("G (agent-boot-egress): a member's first claude-code run raises no approvals", async ({ page }) => {
  // Red on d8f26511: the CLI auto-installs the official plugin MARKETPLACE on
  // its first REPL start — a GCS fetch from downloads.claude.ai with a git
  // clone from github.com as its fallback — so a default-deny estate parked
  // approvals for hosts nobody had asked for. It is NOT the auto-updater: on an
  // npm install that dials registry.npmjs.org, which the shipped default policy
  // already allows, so what DISABLE_AUTOUPDATER removes never parked. The fix
  // is that the traffic stops (CLAUDE_CODE_DISABLE_OFFICIAL_MARKETPLACE_AUTOINSTALL=1),
  // not that the hosts are now allowed — examples/policies/default.json is
  // deliberately unchanged — so an EMPTY approvals list is the whole assertion.
  await dexSignIn(page, MEMBER_EMAIL);

  // A DELTA, not `> 0`: /_seen counts the whole walk, and by this point it is
  // already non-zero, so "greater than zero" would pass without this run ever
  // reaching Bedrock.
  const before = (await seen()).bedrock_calls;
  const id = await launchAgentRun(page, "a first run that parks on nothing");

  // EMPTY FOR THE WHOLE RUN, not merely at the end. A run that parked an
  // approval and had it expire or be swept before the last read would pass a
  // single closing check — and "0 approvals" measured at one instant is exactly
  // the blindness the lane's own review caught in its Go twin.
  await expect
    .poll(async () => (await seen()).bedrock_calls, { timeout: 180_000 })
    .toBeGreaterThan(before);
  for (let i = 0; i < 10; i++) {
    expect(await approvalsFor(page, id), "the first run parked an approval nobody asked for").toEqual([]);
    await page.waitForTimeout(2_000);
  }
});

// ── H — an INTERACTIVE run reaches the model: the owner's literal path ──────

test("H (agent-boot-egress): an interactive run answers ONE trust prompt and reaches Bedrock", async ({ page }) => {
  // THE STEP LIST IS THE W0 SPIKE'S, PRE-DECLARED, NOT DISCOVERED HERE
  // (local/v075/evidence/w0-spike/RESULT.md §2). The spike drove the real image
  // under a real PTY and recorded which screens each config shows:
  //
  //   - with `hasCompletedOnboarding` seeded, the theme picker AND the
  //     "Security notes / Press Enter to continue" page are both gone;
  //   - the workspace-trust screen remains, by design — it is a security
  //     question and Wardyn does not answer it for you. Its default option is
  //     already "Yes, I trust this folder", so ONE bare Enter accepts it;
  //   - there is NO login-method picker at all under CLAUDE_CODE_USE_BEDROCK=1.
  //
  // THE NO-SEED SHAPE, deliberately. Ticking "Let it use tools before I attach"
  // would add the CLI's Bypass Permissions dialog, whose default selection is
  // "No, exit" — a bare Enter there QUITS the CLI, and answering it would be
  // the platform answering a security prompt on the operator's behalf. The
  // default path is also the owner's literal one.
  await dexSignIn(page, MEMBER_EMAIL);

  await page.goto("/runs/new");
  await page.getByRole("combobox", { name: "Title" }).fill("interactive, my own AWS session");
  await page.getByRole("radio", { name: /^Interactive/ }).click();
  // "Start with" stays on its default (`<agent> — launch it in the workspace`)
  // and the Initial prompt stays EMPTY — that is what keeps seed_auto_tools off
  // the wire and the run on the spike's shape (a).
  await page.getByRole("button", { name: /^Launch/ }).click();
  await page.getByRole("button", { name: "Open run" }).click();

  const screen = page.locator(".xterm-screen").first();
  await expect(screen).toBeVisible({ timeout: SANDBOX_UP });

  // Step 4 — the ONLY screen, and the one a human is meant to see.
  await expect
    .poll(async () => (await screen.innerText({ timeout: 1_000 }).catch(() => "")), { timeout: SANDBOX_UP })
    .toContain("Accessing workspace:");
  const trustScreen = await screen.innerText({ timeout: 1_000 });
  expect(trustScreen).toContain("Yes, I trust this folder");
  // …and NOT the product tour the image now pre-answers.
  expect(trustScreen).not.toContain("Choose the text style");
  expect(trustScreen).not.toContain("Security notes");

  // Step 5 — ONE Enter, and one is the measured number on the fixed image (the
  // lane measured 3 on :d8f26511 and 1 on the rebuilt one). The default option
  // is already the accepting one.
  await screen.click();
  await page.keyboard.press("Enter");

  // Step 6 — the CLI's own input prompt, on the Bedrock lane.
  await expect
    .poll(async () => (await screen.innerText({ timeout: 1_000 }).catch(() => "")), { timeout: 120_000 })
    .toContain("Amazon Bedrock");
  // Polled, not read once: the footer paints on its own schedule, and an
  // unpolled read here fails on a frame that simply had not landed yet.
  await expect
    .poll(async () => (await screen.innerText({ timeout: 1_000 }).catch(() => "")), { timeout: 60_000 })
    .toContain("manual mode on");

  // THE APPROVALS CHECK BELONGS AFTER THE ENTER, not before it. Everything the
  // field report saw park happened on the CLI's FIRST REPL start, which is what
  // the Enter above unblocks; a list read while the run was still sitting on
  // the trust dialog is blind to exactly the thing this case measures, and
  // would have read empty on the BROKEN image too.
  const runID = runIDFromURL(page);
  expect(
    await approvalsFor(page, runID),
    "the CLI's first REPL start parked an approval nobody asked for",
  ).toEqual([]);

  // Step 7 — one short prompt, and the model call it makes. A DELTA, for the
  // same reason case G uses one.
  //
  // This is the ONE `page.keyboard.type` in the file, and it is deliberate: it
  // is a claude-code run's own CLI prompt, not a sign-in pane. Case C's
  // negative control is that no sign-in sandbox is ever typed into.
  const before = (await seen()).bedrock_calls;
  await page.keyboard.type("Reply with the single word: ready.");
  await page.keyboard.press("Enter");
  await expect.poll(async () => (await seen()).bedrock_calls, { timeout: 180_000 }).toBeGreaterThan(before);

  // AND AGAIN, at the end. The marketplace fetch is asynchronous AFTER the REPL
  // starts, so the read above can land before a broken image has dialled
  // anything; this one is after a full round trip to Bedrock. Decided approvals
  // are never deleted, so one closing read cannot miss a row that appeared and
  // went — it would still be sitting there.
  expect(await approvalsFor(page, runID), "the run parked an approval after its first model call").toEqual([]);
});

// ── I — the strip carries the sign-in, on whatever screen you were on ───────

test("I (model-access-banner): the strip rides every screen and clears without a reload", async ({
  page,
  request,
}) => {
  // Finding 2's live half that only a real capture can show. The FIRST-RUN
  // sentence (MODEL_ACCESS_BANNER.NOT_SIGNED_IN) is asserted in
  // sso-member.spec.ts's case I, which is the one seat on the walk where a
  // member has never signed in — by the time this file runs the member owns a
  // stored session and nothing in the product deletes one (DELETE
  // /setup/harness-credential is operator-only AND scoped to the caller's own
  // subject). So this case drives the LAPSE arm, which is the one that can be
  // reached honestly here, and it is the arm that carries the door.
  await dexSignIn(page, MEMBER_EMAIL);
  await ensureActionable(page, request);

  // (1) The Runs board — the strip, the sign-in, and no set-aside: a lapse of
  // something you already had is never dismissable (model-access-copy.ts's
  // NOT_NOW is offered for the first-run state and a dead SHARED credential
  // only). The strip is a LAZY chunk behind `Suspense fallback={null}` and says
  // nothing until /me resolves, so it is awaited, never read on the first frame.
  await page.goto("/runs");
  await expect(page.getByText(MODEL_ACCESS_BANNER.EXPIRED)).toBeVisible({ timeout: 60_000 });
  await expect(page.getByRole("button", { name: AGENTS.SIGN_IN_AWS, exact: true })).toBeVisible();
  await expect(page.getByRole("button", { name: MODEL_ACCESS_BANNER.NOT_NOW })).toHaveCount(0);

  // (2) It is the SHELL's band. Client-side navigation (what a <NavLink> click
  // does), not a second full load — a full load would prove only that the strip
  // renders twice.
  await page.evaluate(() => {
    window.history.pushState({}, "", "/workspaces");
    window.dispatchEvent(new PopStateEvent("popstate"));
  });
  await expect(page).toHaveURL(/\/workspaces$/);
  await expect(page.getByText(MODEL_ACCESS_BANNER.EXPIRED)).toBeVisible({ timeout: 60_000 });

  // THE NO-RELOAD WITNESS, planted before the door opens. "The strip is gone
  // without a reload" is the claim; a strip that vanished because the document
  // was re-fetched would satisfy every assertion below without it.
  await page.evaluate(() => {
    (window as unknown as { __wardynDocumentAge?: number }).__wardynDocumentAge = Date.now();
  });
  const urlBefore = page.url();

  // (3) The sign-in ITSELF, in a dialog, on the screen they were on. Driven
  // through signInThroughPane so the witness is the server's moved capture, not
  // the terminal node — the pane unmounts it within half a second of the marker.
  await signInThroughPane(page, async (p: Page) => {
    await p.getByRole("button", { name: AGENTS.SIGN_IN_AWS, exact: true }).click();
    await expect(p.getByRole("heading", { name: MODEL_ACCESS_BANNER.DIALOG_TITLE })).toBeVisible({ timeout: 60_000 });
    await expect(p.getByTestId("harness-login-pane")).toBeVisible({ timeout: 60_000 });
    const start = p.getByRole("button", { name: "Start login" });
    if (await start.isVisible().catch(() => false)) await start.click();
  });

  // (4) The three things the door promises on a completed capture: the toast
  // (CONSOLE-RULES §9 — a surface that disappears is not a confirmation), the
  // strip gone, and the page never left or reloaded.
  await expect(page.getByText(MODEL_ACCESS_BANNER.SIGNED_IN_TOAST)).toBeVisible({ timeout: 60_000 });
  await expect(page.getByText(MODEL_ACCESS_BANNER.EXPIRED)).toHaveCount(0, { timeout: 60_000 });
  await expect(page.getByTestId("harness-login-pane")).toHaveCount(0);
  expect(page.url(), "the door navigated instead of opening in place").toBe(urlBefore);
  expect(
    await page.evaluate(
      () => typeof (window as unknown as { __wardynDocumentAge?: number }).__wardynDocumentAge === "number",
    ),
    "the document was reloaded — the strip cleared for the wrong reason",
  ).toBe(true);

  // …and the server agrees with what the strip stopped saying.
  await expect.poll(async () => (await modelAccess(page)).state, { timeout: 120_000 }).toBe("live");
});

// ── E2 — a terminal reason ends the wait on the REASON, not on the clock ────

test("E2 (starting-detail): a sign-in on an unpullable image fails in seconds with the registry's words", async ({
  page,
  request,
}) => {
  // Finding 6's other half. Case E proves a wait that IS ordinary is narrated;
  // this proves a wait that will never end is not waited out. The 0.7.5 pane
  // graded a start purely on a clock, so this sign-in sat "still starting" for
  // FIVE MINUTES and then said Wardyn could not read the sandbox — which was
  // false: it could read it perfectly, and the answer had been final since
  // second ten.
  //
  // THE UNPULLABLE IMAGE IS THE DAEMON'S agent-image MAP, re-pointed. The login
  // sandbox resolves `aws-sso` through WARDYN_AGENT_IMAGES (kind-sso-walk.sh
  // adds that key for exactly this reason), so one env edit + one rollout puts a
  // tag no registry serves behind the next sign-in. The repository does not
  // exist on docker.io either, so the kubelet's answer is the registry's own
  // refusal rather than a timeout.
  const original = agentImagesEnv();
  expect(original, "the deployment carries no WARDYN_AGENT_IMAGES — run this through scripts/kind-sso-walk.sh").not.toBe(
    "",
  );
  const broken = JSON.stringify({
    ...(JSON.parse(original) as Record<string, string>),
    "aws-sso": "wardyn/agent-aws-sso:no-such-tag-0f0f",
  });

  setAgentImagesEnv(broken);
  try {
    // The rollout replaced the pod; wait for the new one to actually serve
    // before driving a browser at it.
    // The old pod drops its connections mid-rollout (ECONNRESET); a thrown
    // request is "not serving yet", not a failure.
    await expect
      .poll(async () => (await request.get("/healthz").catch(() => null))?.status() ?? 0, { timeout: 120_000 })
      .toBe(200);

    await dexSignIn(page, MEMBER_EMAIL);
    await ensureActionable(page, request);
    const prior = new Set((await myLoginRuns(page)).map((r) => r.id));
    await openLoginPane(page);
    // The budget IS the assertion. openLoginPane returns once "Start login" has
    // been clicked, so the clock below starts no earlier than the POST — which
    // only makes it stricter.
    const started = Date.now();

    // #628 state 7: the door's download step reads failed, and the alert is
    // the server's own status_detail as is — the registry's words are in it,
    // because they are what names the fix.
    await expect(
      page.getByTestId("signin-progress").getByRole("listitem").filter({ hasText: SIGNIN_PROGRESS.STEP_DOWNLOAD_FAILED }),
    ).toBeVisible({ timeout: 20_000 });
    const elapsed = Date.now() - started;
    expect(elapsed, "a terminal reason must end the wait on the reason, not on the 5-minute clock").toBeLessThan(
      20_000,
    );
    await expect(page.getByTestId("harness-login-pane").getByRole("alert")).toContainText("no-such-tag-0f0f");
    // Neither of the two clock-graded sentences: this wait never became "slow",
    // and Wardyn could read the run throughout.
    await expect(page.getByText(LOGIN_SANDBOX_SLOW_START)).toHaveCount(0);
    await expect(page.getByText(LOGIN_SANDBOX_UNREADABLE)).toHaveCount(0);
    // The approved packet offers Retry here (it starts a fresh sandbox), and
    // Cancel beside it.
    await expect(page.getByRole("button", { name: SIGNIN_PROGRESS.RETRY })).toBeVisible();
    await expect(page.getByRole("button", { name: SIGNIN_PROGRESS.CANCEL }).first()).toBeVisible();

    // The same evidence on the wire, which is what makes the sentence above
    // more than a console string: a run that went STARTING → FAILED between two
    // polls still carries its detail, because a TERMINAL reason survives FAILED
    // (internal/api/runs_status_detail.go's projectStatusDetail).
    const loginRun = await newLoginRun(page, prior);
    const row = await page.evaluate(async (id: string) => {
      const r = await fetch(`/api/v1/runs/${id}`, { credentials: "include" });
      return (await r.json()) as { status_detail?: string; status_reason?: string };
    }, loginRun.id);
    expect(
      ["ImagePullBackOff", "ErrImagePull"],
      `the server graded the stuck start as ${row.status_reason}: ${row.status_detail}`,
    ).toContain(row.status_reason);
    expect(row.status_detail, "the registry's own words never reached the wire").toContain("no-such-tag-0f0f");

    // Leave nothing running: Cancel kills the run by the id the POST handed
    // back, which is the whole reason that control is on screen during a wait.
    await page.getByRole("button", { name: "Cancel" }).first().click();
  } finally {
    // ALWAYS, and waited for: every case after this one launches a sandbox.
    setAgentImagesEnv(original);
  }
});

// ── F — the no-credential member preview (LAST: it signs the ADMIN in) ──────

// ── L0 — 0.7.7: the setup gate never confiscates the console over a person ──

test("L0 (setup gate): an admin with a lapsed AWS sign-in of their own opens New Run and stays there", async ({
  page,
  request,
}) => {
  // The 0.7.6 field report's exact shape, on the walk: a per_user roster, an
  // install that never marked onboarding complete, and an admin whose OWN
  // session has lapsed. 0.7.8 rewrote this case: the ORIGINAL 0.7.7 version
  // never captured the admin's own AWS session at all, so harness_credential_aws
  // (h.Captured-gated — internal/api/modelaccess.go's harnessCredentialCheck)
  // never appeared and the case exercised llm_provider/bedrock_provider's
  // per_user arms instead — real rows, but not the one the 0.7.6 field report
  // was actually about. This version captures the admin's own session first,
  // through the SAME pane F uses below, then lapses it exactly the way the
  // owner's did in the field: a roster-pin flip (ensureActionable, the shared
  // helper C/D/E/L all use).
  //
  // ensureActionable's flip is the ROSTER's one shared pin — it lapses EVERY
  // captured session under the old pair, not just this admin's. Nothing
  // downstream in this file depends on a live admin session surviving into F
  // (F re-signs one in itself), so the side effect is harmless here — but it
  // is real, and stated rather than silently relied on.
  await dexSignIn(page, ADMIN_EMAIL);
  await signInThroughPane(page, openAdminLoginPane);
  await expect.poll(async () => (await modelAccess(page)).state, { timeout: 120_000 }).toBe("live");
  await ensureActionable(page, request);
  await expect.poll(async () => (await modelAccess(page)).state, { timeout: 120_000 }).toBe("expired_signin");

  const status = await page.evaluate(async () => {
    const r = await fetch("/api/v1/setup/status", { credentials: "include" });
    return (await r.json()) as {
      onboarding_complete?: boolean;
      checks?: Array<{ id: string; status: string; blocking?: boolean }>;
    };
  });
  expect(status.onboarding_complete, "the walk's fresh install never marks onboarding complete").toBeFalsy();
  expect(
    status.checks?.find((c) => c.id === "harness_credential_aws")?.status,
    "the admin's own captured AWS session was just lapsed by the pin flip — the row the 0.7.6 field report's gate misread",
  ).toBe("warn");
  // The non-vacuity guard, restated against the 0.7.8 contract: every
  // warn/fail row present must be non-blocking, or the assertion below (New
  // Run stays put) would be meaningless — a truly blocking row SHOULD gate.
  const gating = (status.checks ?? []).filter((c) => c.status === "warn" || c.status === "fail");
  expect(
    gating.filter((c) => c.blocking).map((c) => c.id),
    "no warn/fail row on this install may be blocking, or this case proves nothing",
  ).toEqual([]);

  // A full LOAD of a gated route: the once-per-load gate evaluates the landing
  // /setup/status read. The rail's per-person line renders off that same read,
  // so once it is on screen the answer that used to bounce us has landed — and
  // the page is still New Run.
  await page.goto("/runs/new");
  await expect(page.getByRole("heading", { name: "New run" })).toBeVisible({ timeout: 60_000 });
  // EXPIRED, not NOT_SIGNED_IN: new-run-rail.tsx's ModelAccessLine reads
  // `expired_signin` to RAIL_MODEL_ACCESS.EXPIRED — a captured-then-lapsed
  // session, not "never signed in".
  await expect(page.getByText(RAIL_MODEL_ACCESS.EXPIRED)).toBeVisible({ timeout: 60_000 });
  await expect(page).toHaveURL(/\/runs\/new$/);
});

test("F (member-preview): an admin previews the state a member is in before they sign in", async ({ page }) => {
  // LAST IN THE FILE, and it must stay last: it signs the ADMIN in to AWS,
  // which breaks sso-member.spec.ts's "the capture belongs to the member alone"
  // invariant (the admin stays `not_configured`) for anything that runs after.
  await dexSignIn(page, ADMIN_EMAIL);
  expect((await me(page)).operator).toBe(true);
  // The menu item is GRANTED by the server, not decided by the console: /me
  // publishes user_preview_available and it is true only under a per_user
  // roster row — which this deployment has.
  expect((await me(page)).user_preview_available, "the walk's roster row is per_user; the preview must be offered").toBe(
    true,
  );

  // The admin's own AWS sign-in first — without it "not signed in" inside the
  // preview would be indistinguishable from the admin simply never having one,
  // and the ceiling this case exists to prove (your session is HIDDEN, not
  // removed, and comes back on exit) would be unprovable. It is NOT on /setup:
  // an operator's /setup is the operator Getting Started, so the admin's own
  // sign-in is the Agents tab's (openAdminLoginPane).
  await signInThroughPane(page, openAdminLoginPane);
  await expect.poll(async () => (await modelAccess(page)).state, { timeout: 120_000 }).toBe("live");

  // Into the preview, from the Permissions header (M-2).
  await page.goto("/admin/permissions");
  await page.getByRole("button", { name: USER_PREVIEW.MENU_NEW }).click();
  await expect(page.getByText(USER_PREVIEW.BANNER)).toBeVisible({ timeout: 60_000 });
  await expect.poll(async () => (await me(page)).user_view_no_credential, { timeout: 30_000 }).toBe(true);

  // The state every new member is in, and the one the plain toggle structurally
  // cannot show: it clamps the role and leaves the subject alone, so every
  // per-principal credential lookup still finds the admin's own session.
  await page.goto("/setup");
  await expect.poll(async () => (await modelAccess(page)).state, { timeout: 60_000 }).toBe("not_configured");
  await expect(page.getByText(YOUR_MODEL_KEY.NOT_SIGNED_IN_CHIP).first()).toBeVisible({ timeout: 60_000 });

  // …and the one door that could WRITE inside the preview is refused, because a
  // capture made here would land on the admin's own identity and overwrite
  // their real session.
  //
  // THE CTA IS NOT THE REQUEST. Under startURLManaged the pane opens in phase
  // `intro` and POST /setup/harness-login is sent only by "Start login"
  // (harness-login-pane.tsx's launch) — so a case that clicked the CTA and then
  // waited for the 409 sentence would have waited for a request it never made.
  // The sentence is Go-side (userViewPreviewSignInRefusal) and reaches the console
  // as the pane's error, which renders in its role="alert" region.
  await page.getByRole("button", { name: AGENTS.SIGN_IN_AWS }).first().click();
  await page.getByRole("button", { name: "Start login" }).click();
  await expect(page.getByRole("alert").getByText(MEMBER_PREVIEW_SIGNIN_REFUSAL)).toBeVisible({ timeout: 60_000 });

  // Nothing was deleted: the admin's session sits untouched in the store and
  // comes back the moment they exit.
  await page.getByRole("button", { name: USER_PREVIEW.EXIT }).click();
  await expect(page.getByText(USER_PREVIEW.BANNER)).toBeHidden({ timeout: 60_000 });
  await expect.poll(async () => (await modelAccess(page)).state, { timeout: 60_000 }).toBe("live");
});

// ── L — 0.7.7: Launch with a lapsed sign-in is one dialog, then the run ─────

test("L (launch door): Launch with a lapsed AWS sign-in opens the sign-in itself, and the same run launches after it", async ({
  page,
  request,
}) => {
  // The 0.7.6 field report, driven end to end: the member's session is lapsed
  // (a pin flip — helpers.ts's makeMemberActionable, the same lapse every
  // sign-in case here drives), they fill in New Run and click Launch. The
  // server refuses the run before any row exists (422, reason
  // model_credential); the rail opens the AWS sign-in dialog ITSELF; the
  // device flow completes on the fake; the SAME click's run launches. No trip
  // to Getting started, no second click.
  await dexSignIn(page, MEMBER_EMAIL);
  // ensureActionable, not a blind flip: the pin is handed back and forth by
  // every case before this one and E2 leaves the member ALREADY actionable, so
  // an unconditional makeMemberActionable() healed them (walk-2, L red on the
  // opening poll). The lapse here is the pin contradiction — the create-time
  // refusal's stored-identity arm, reason model_credential like the spent one.
  await ensureActionable(page, request);
  await expect.poll(async () => (await modelAccess(page)).state, { timeout: 120_000 }).toBe("expired_signin");

  await page.goto("/runs/new");
  await page.getByRole("combobox", { name: "Title" }).fill("L launch door");
  await page.getByRole("radio", { name: /^Autonomous/ }).click();
  await page.locator("#nr-task").fill("Reply with the single word: ready.");
  const urlBefore = page.url();
  const clickedAt = new Date().toISOString();

  // THE CLICK IS THE CHECK. Nothing on the console pre-grades the cached
  // status; the server's refusal is what opens the door. Driven through
  // signInThroughPane so the witness is the server's MOVED capture, not the
  // terminal node (helpers.ts explains why the DOM cannot be the witness).
  await signInThroughPane(page, async (p: Page) => {
    await p.getByRole("button", { name: /^Launch/ }).click();
    await expect(p.getByRole("heading", { name: MODEL_ACCESS_BANNER.DIALOG_TITLE })).toBeVisible({ timeout: 60_000 });
    await expect(p.getByTestId("harness-login-pane")).toBeVisible({ timeout: 60_000 });
    const start = p.getByRole("button", { name: "Start login" });
    if (await start.isVisible().catch(() => false)) await start.click();
  });

  // The relaunch fired from the door's completion: this deployment's launch
  // carries advisories (egress narrowed, resources capped), so the screen
  // HOLDS with "Open run" exactly as a hand launch does (helpers.ts's
  // launchAgentRun) — a run that launched with no advisories would have
  // navigated already. NOT "Running" (walk-3): the fake answers inference in
  // seconds and the agent exits, so the run can be FINISHED before this spec
  // — which reaches the page only after signInThroughPane's capture poll — ever
  // looks; the witness is the run ROW and its trail below, never a state chip.
  const openRun = page.getByRole("button", { name: "Open run" });
  await expect(openRun.or(page.getByRole("heading", { name: "L launch door" }))).toBeVisible({ timeout: SANDBOX_UP });
  if (await openRun.isVisible().catch(() => false)) await openRun.click();
  await expect(page).toHaveURL(/\/runs\/[0-9a-f-]{36}$/, { timeout: SANDBOX_UP });
  const runID = runIDFromURL(page);
  expect(page.url(), "the run page, not New Run or Getting started").not.toBe(urlBefore);
  expect(new URL(page.url()).pathname, "never the setup funnel").not.toMatch(/^\/setup/);

  // The server's own story of THIS click: created (never refused), credentialed
  // on the fresh capture, and executed. A member reads their own run's trail.
  // POLLED, not read once (#804): run.exec is a follow-on write after the
  // create that already got this spec onto the run page, and under load the
  // row can land after a single read — a live walk saw run.create,
  // credential.mint, secret.read and identity.renew all present with run.exec
  // still missing. Poll the trail (bounded by SANDBOX_UP, the same ceiling
  // dispatch itself races against) until run.exec:success appears, and keep
  // the LAST read for the assertions below rather than fetching again.
  let actions: string[] = [];
  const readAuditTrail = async (): Promise<string[]> => {
    actions = (await page.evaluate(async (id: string) => {
      const r = await fetch(`/api/v1/audit?run_id=${encodeURIComponent(id)}&limit=200`, { credentials: "include" });
      const body = (await r.json()) as { items?: Array<{ action: string; outcome?: string }> } | Array<{ action: string; outcome?: string }>;
      return (Array.isArray(body) ? body : (body.items ?? [])).map((e) => `${e.action}:${e.outcome ?? ""}`);
    }, runID)) as string[];
    return actions;
  };
  await expect
    .poll(readAuditTrail, { timeout: SANDBOX_UP })
    .toContain("run.exec:success");
  expect(actions, "the relaunch created the run").toContain("run.create:success");
  expect(actions.filter((a) => a.startsWith("run.create:failure")), "never refused for its credential").toEqual([]);

  // The server agrees on both halves: the member is live again, and the run
  // that launched is the ONE this click created (a second create would mean the
  // door relaunched twice).
  await expect.poll(async () => (await modelAccess(page)).state, { timeout: 120_000 }).toBe("live");
  const mine = (await page.evaluate(async () => {
    const r = await fetch("/api/v1/runs?limit=1000", { credentials: "include" });
    const body = (await r.json()) as
      | { items?: Array<{ id: string; title?: string; created_at?: string }> }
      | Array<{ id: string; title?: string; created_at?: string }>;
    return Array.isArray(body) ? body : (body.items ?? []);
  })) as Array<{ id: string; title?: string; created_at?: string }>;
  // Scoped to THIS click: an iteration against a cluster an earlier walk left
  // behind sees that walk's run of the same title too.
  const created = mine.filter(
    (r) => r.id === runID || (r.title === "L launch door" && (r.created_at ?? "") >= clickedAt),
  );
  expect(created.map((r) => r.id), "exactly one run for this click").toEqual([runID]);
});
