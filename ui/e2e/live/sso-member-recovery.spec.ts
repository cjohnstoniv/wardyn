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
 *   B → A → A(rail) → C → D → E → G → H → F
 *
 * B runs FIRST because it reads the member's SIGNED-IN card while the member is
 * still `live` from the previous file — case A's console save is what flips the
 * pin and takes that state away. F runs LAST because it signs the ADMIN in to
 * AWS, which breaks sso-member.spec.ts:"the capture belongs to the member
 * alone"'s admin-stays-`not_configured` invariant for anything after it.
 *
 * A leaves the member ACTIONABLE (expired_signin) and C consumes that window;
 * C heals the member back to `live`, and D and E each call
 * makeMemberActionable() for their own.
 *
 * Self-skips without WARDYN_TEST_K8S=1, same as its sibling.
 */

import { expect, test, type Page } from "@playwright/test";
import { execFileSync } from "node:child_process";
import { MEMBER_MODE } from "../../src/app/components/wardyn/member-mode-banner";
import { LOGIN_SANDBOX_NOTE } from "../../src/app/components/screens/run-detail/login-sandbox-note";
import { MEMBER_GETTING_STARTED, YOUR_MODEL_KEY } from "../../src/app/components/wardyn/copy";
import { AGENTS, AGENTS_DRAFT, PROVIDERS } from "../../src/app/lib/workspace-providers-copy";
import {
  ADMIN_EMAIL,
  FAKE_URL,
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
} from "./helpers";

test.skip(process.env.WARDYN_TEST_K8S !== "1", "live cluster walk: set WARDYN_TEST_K8S=1 (scripts/kind-sso-walk.sh)");
test.describe.configure({ mode: "serial" });

// ── CONSTANTS THIS FILE ASSERTS THROUGH THAT ARE NOT MERGED YET ─────────────
//
// Every case that uses one of these is `test.fixme("<lane>")` below. When the
// lane merges, the coordinator REPLACES this block with the real imports and
// deletes the literals — never the other way round, and never a regex loose
// enough to match both spellings.
//
// lane `ui-rail-truth` → ui/src/app/components/wardyn/copy.ts:
//   import { RAIL_CREDENTIAL, RECORDING_DISABLED_TITLE } from "../../src/app/components/wardyn/copy";
//   RAIL_CREDENTIAL.SANDBOX_BEDROCK, RAIL_CREDENTIAL.SANDBOX_BEDROCK_CHIP_PER_USER
// lane `login-pane` → .../settings/harness-login-pane.tsx (exported by that lane)
//   and .../settings/login-start-wait.ts:
//   import { CAPTURE_NOT_CORROBORATED, LOGIN_SANDBOX_UNREADABLE } from "../../src/app/components/screens/settings/harness-login-pane";
//   import { LOGIN_SANDBOX_SLOW_START } from "../../src/app/components/screens/settings/login-start-wait";
const RAIL_CREDENTIAL_SANDBOX_BEDROCK =
  "Model credential — AWS credentials sign inside the sandbox, so this run holds them for its lifetime.";
const RAIL_CREDENTIAL_CHIP_PER_USER = "Your AWS sign-in";
const RECORDING_DISABLED_TITLE_DRAFT = "Session recording is disabled on this deployment";
const CAPTURE_NOT_CORROBORATED = "The sandbox reported a capture the server does not have — sign in again.";
const LOGIN_SANDBOX_UNREADABLE =
  "Wardyn stopped being able to read the sign-in sandbox, so it can't say whether it came up. Try again.";
// NOT DRAFTED AT W2 — lane `login-pane` had not started. Deliberately a
// sentinel rather than "": flipping case E's fixme without re-pointing this at
// the lane's constant must RED immediately, not pass vacuously.
const LOGIN_SANDBOX_SLOW_START = "<LOGIN_SANDBOX_SLOW_START — lane login-pane has not drafted this sentence>";

/** The 409 body of POST /setup/harness-login inside the no-credential preview
 *  (internal/api/membermode_preview.go's memberPreviewSignInRefusal — Go-side
 *  and unexported, so there is no TS constant to import; lane member-preview's
 *  canon hands the spelling over verbatim and pins it from the Go side). */
const MEMBER_PREVIEW_SIGNIN_REFUSAL =
  "Exit member mode to sign in to AWS — the capture would land on your own identity.";

/** The kind context/namespace the walk installed into, so case E can taint the
 *  node and read the run pod's phase. scripts/kind-sso-walk.sh exports both. */
const KUBE_CONTEXT = process.env.WARDYN_LIVE_KUBE_CONTEXT || "kind-wardyn-quickstart";
const KUBE_NAMESPACE = process.env.WARDYN_LIVE_KUBE_NAMESPACE || "wardyn";
const KUBE_NODE = process.env.WARDYN_LIVE_KUBE_NODE || "wardyn-quickstart-control-plane";
const COLDPULL_TAINT = "wardyn-coldpull=1:NoSchedule";

function kubectl(...args: string[]): string {
  return execFileSync("kubectl", ["--context", KUBE_CONTEXT, ...args], { encoding: "utf8" }).trim();
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
  await page.goto("/settings");
  await page.getByTestId("providers-card").getByText(PROVIDERS.CARD_OPEN).click();
  await expect(page).toHaveURL(/\/providers$/);
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
  for (const id of ["codex-cli", "none"]) {
    const other = (await getRoster(request)).find((a) => a.id === id);
    if (other) expect(other.enabled, `${id} must be disabled on this deployment`).toBe(false);
  }
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
  test.fixme(true, "ui-rail-truth");
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

  await expect(page.getByText(RAIL_CREDENTIAL_SANDBOX_BEDROCK)).toBeVisible({ timeout: 60_000 });
  await expect(page.getByText(RAIL_CREDENTIAL_CHIP_PER_USER)).toBeVisible();
  // …and Recording states the truth about a stock Helm install rather than
  // promising a capture that cannot happen: the kind quickstart leaves
  // persistence.enabled=false.
  await expect(page.getByText(RECORDING_DISABLED_TITLE_DRAFT)).toBeVisible();
  await expect(page.getByText("Every keystroke and every outbound connection")).toHaveCount(0);
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
  await openLoginPane(page);

  // Leave the console behind IMMEDIATELY. Nothing server-side stops a login run
  // on capture — the shutdown is this pane's own killRun — so navigating away
  // is what makes the rest of this case the RUNS-LIST path rather than the
  // console one.
  await expect.poll(async () => (await myLoginRuns(page)).length > 0, { timeout: SANDBOX_UP }).toBe(true);
  const loginRun = (await myLoginRuns(page))[0];
  await page.goto("/runs");

  // The run is openable from the list by the task the server stamped on it
  // (harnessLoginTask), which is the whole point of labelling it: opening it
  // from /runs is not a mystery box.
  await page.getByText("harness login").first().click();
  await expect(page).toHaveURL(new RegExp(`/runs/${loginRun.id}$`), { timeout: 60_000 });
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

  // The pane's OWN output, not an echo of an argv nothing prints: the image's
  // banner (SELFRUN_MARKER, imported — TestSelfRunBanner_UIParity pins the
  // shell side to it) and the device-code URL the AWS CLI printed, which is the
  // fake addressed by its in-cluster Service name.
  await awaitSelfRunStarted(screen);
  await expect
    .poll(async () => (await screen.innerText({ timeout: 1_000 }).catch(() => "")).includes(FAKE_URL), {
      timeout: 120_000,
    })
    .toBe(true);

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
  test.fixme(true, "login-pane");
  await dexSignIn(page, MEMBER_EMAIL);
  await makeMemberActionable(request);
  await expect.poll(async () => (await modelAccess(page)).state, { timeout: 120_000 }).toBe("expired_signin");

  // "Reaches live" is VACUOUS here — the member was live when this case
  // started. The assertion is that the BLOB MOVED: source_run_id is the second
  // run's and captured_at advanced.
  const before = await ownAWSRow(page);

  // Cancel is the only control while the run is STARTING, and it kills the run
  // and closes the pane (harness-login-pane.tsx) — so it leaves NO blob and no
  // half-state for the retry to trip over.
  await openLoginPane(page);
  await page.getByRole("button", { name: "Cancel" }).first().click();
  await expect(page.getByTestId("harness-login-pane")).toHaveCount(0, { timeout: 60_000 });

  // The retry, completed.
  await openLoginPane(page);
  const screen = page.locator(".xterm-screen").first();
  await expect(screen).toBeVisible({ timeout: SANDBOX_UP });
  await awaitSelfRunStarted(screen);
  await awaitCapture(page, screen);

  const after = await ownAWSRow(page);
  expect(after.source_run_id, "the stored capture did not move to the retry's run").not.toBe(before.source_run_id);
  expect(Date.parse(after.captured_at ?? "")).toBeGreaterThan(Date.parse(before.captured_at ?? ""));
  // The pane corroborates a marker against the SERVER before it claims a
  // capture; this sentence is what it shows when the server disagrees. On a
  // healthy walk it must never render.
  await expect(page.getByText(CAPTURE_NOT_CORROBORATED)).toHaveCount(0);

  // ── the supersede, live ───────────────────────────────────────────────────
  // Start a sign-in, navigate away WITHOUT Cancel (the orphan), start another.
  await makeMemberActionable(request);
  await openLoginPane(page);
  await expect.poll(async () => (await myLoginRuns(page)).length, { timeout: SANDBOX_UP }).toBeGreaterThan(0);
  const orphan = (await myLoginRuns(page))[0];
  await page.goto("/runs");
  await openLoginPane(page);
  await expect
    .poll(async () => (await myLoginRuns(page)).find((r) => r.id === orphan.id)?.state, { timeout: 120_000 })
    .toBe("KILLED");
  // …and exactly ONE non-terminal login run is left for this member. (The run
  // JSON carries no reason field — the `superseded_by_new_login` reason is
  // asserted on the run.kill AUDIT row, in the Go test.)
  const open = (await myLoginRuns(page)).filter((r) => !["KILLED", "DONE", "FAILED", "TIMED_OUT"].includes(r.state ?? ""));
  expect(open.map((r) => r.id), "more than one live sign-in sandbox for one member").toHaveLength(1);

  await awaitCapture(page, page.locator(".xterm-screen").first());
});

// ── E — a login run that stays in STARTING for 90 s never reads as UNREADABLE ─

test("E (login-pane): a sign-in held in STARTING for 90 s reads as slow, never as unreadable", async ({
  page,
  request,
}) => {
  test.fixme(true, "login-pane");
  // The live twin of the Go characterization test, and the datum that tells an
  // operator whether an UNREADABLE they saw was estate-side. The hold is
  // manufactured by a node TAINT, which never evicts a running pod and which
  // the run pod cannot tolerate — there is not one `Toleration` reference in
  // internal/runner/k8s — so the pod parks in Pending.
  //
  // 90 s, and no longer: it is well inside canaryWaitTimeout's 3 minutes, past
  // which the run itself fails and the case would be measuring the wrong thing.
  kubectl("taint", "nodes", KUBE_NODE, COLDPULL_TAINT);

  await dexSignIn(page, MEMBER_EMAIL);
  await makeMemberActionable(request);
  await openLoginPane(page);

  await expect.poll(async () => (await myLoginRuns(page)).length, { timeout: 120_000 }).toBeGreaterThan(0);
  const loginRun = (await myLoginRuns(page))[0].id;

  // Without this the 90 s assertion is vacuous — a pod that scheduled anyway
  // would pass it by simply working.
  await expect
    .poll(
      () => kubectl("-n", KUBE_NAMESPACE, "get", "pod", `wardyn-agent-${loginRun}`, "-o", "jsonpath={.status.phase}"),
      { timeout: 60_000 },
    )
    .toBe("Pending");

  // The pane's own 2 s GET /runs/{id} poll must stay readable for the whole
  // hold: a single non-200 is what raises LOGIN_SANDBOX_UNREADABLE, and the
  // point of this case is that a SLOW start and an UNREADABLE one are different
  // sentences.
  const codes: number[] = [];
  const stop = Date.now() + 90_000;
  while (Date.now() < stop) {
    codes.push(
      await page.evaluate(async (id: string) => {
        const r = await fetch(`/api/v1/runs/${id}`, { credentials: "include" });
        return r.status;
      }, loginRun),
    );
    await page.waitForTimeout(2_000);
  }
  expect(codes.filter((c) => c !== 200), "the harness could not read the run for the whole hold").toHaveLength(0);

  await expect(page.getByText(LOGIN_SANDBOX_SLOW_START)).toBeVisible();
  await expect(page.getByText(LOGIN_SANDBOX_UNREADABLE)).toHaveCount(0);

  kubectl("taint", "nodes", KUBE_NODE, "wardyn-coldpull-");
  const screen = page.locator(".xterm-screen").first();
  await expect(screen).toBeVisible({ timeout: SANDBOX_UP });
  await awaitSelfRunStarted(screen);
  await awaitCapture(page, screen);
  await expect.poll(async () => (await modelAccess(page)).state, { timeout: 120_000 }).toBe("live");
});

// ── G — a first claude-code run parks on nothing ────────────────────────────

test("G (agent-boot-egress): a member's first claude-code run raises no approvals", async ({ page }) => {
  test.fixme(true, "agent-boot-egress");
  // Red on d8f26511: the CLI auto-installs the official plugin marketplace on
  // its first REPL start, which dials downloads.claude.ai and falls back to a
  // git clone from github.com — two parked approvals for hosts nobody asked
  // for, on a default-deny estate. The fix is that the traffic stops
  // (CLAUDE_CODE_DISABLE_OFFICIAL_MARKETPLACE_AUTOINSTALL=1), not that the
  // hosts are allowed, so the shipped default policy is unchanged and an EMPTY
  // approvals list is the whole assertion.
  await dexSignIn(page, MEMBER_EMAIL);

  // A DELTA, not `> 0`: /_seen counts the whole walk, and by this point it is
  // already non-zero, so "greater than zero" would pass without this run ever
  // reaching Bedrock.
  const before = (await seen()).bedrock_calls;
  const id = await launchAgentRun(page, "a first run that parks on nothing");
  await expect.poll(async () => (await seen()).bedrock_calls, { timeout: 180_000 }).toBeGreaterThan(before);

  expect(await approvalsFor(page, id), "the first run parked an approval nobody asked for").toEqual([]);
});

// ── H — an INTERACTIVE run reaches the model: the owner's literal path ──────

test("H (agent-boot-egress): an interactive run answers ONE trust prompt and reaches Bedrock", async ({ page }) => {
  test.fixme(true, "agent-boot-egress");
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

  // Step 5 — ONE Enter. The default option is already the accepting one.
  await screen.click();
  await page.keyboard.press("Enter");

  // Step 6 — the CLI's own input prompt, on the Bedrock lane.
  await expect
    .poll(async () => (await screen.innerText({ timeout: 1_000 }).catch(() => "")), { timeout: 120_000 })
    .toContain("Amazon Bedrock");

  // Step 7 — one short prompt, and the model call it makes. A DELTA, for the
  // same reason case G uses one.
  const before = (await seen()).bedrock_calls;
  await page.keyboard.type("Reply with the single word: ready.");
  await page.keyboard.press("Enter");
  await expect.poll(async () => (await seen()).bedrock_calls, { timeout: 180_000 }).toBeGreaterThan(before);

  // …and answering the trust prompt raised no approvals: pressing Enter there
  // is not an egress event.
  expect(await approvalsFor(page, runIDFromURL(page))).toEqual([]);
});

// ── F — the no-credential member preview (LAST: it signs the ADMIN in) ──────

test("F (member-preview): an admin previews the state a member is in before they sign in", async ({ page }) => {
  // LAST IN THE FILE, and it must stay last: it signs the ADMIN in to AWS,
  // which breaks sso-member.spec.ts's "the capture belongs to the member alone"
  // invariant (the admin stays `not_configured`) for anything that runs after.
  await dexSignIn(page, ADMIN_EMAIL);
  expect((await me(page)).operator).toBe(true);
  // The menu item is GRANTED by the server, not decided by the console: /me
  // publishes member_preview_available and it is true only under a per_user
  // roster row — which this deployment has.
  expect((await me(page)).member_preview_available, "the walk's roster row is per_user; the preview must be offered").toBe(
    true,
  );

  // The admin's own AWS sign-in first — without it "not signed in" inside the
  // preview would be indistinguishable from the admin simply never having one,
  // and the ceiling this case exists to prove (your session is HIDDEN, not
  // removed, and comes back on exit) would be unprovable. It is NOT on /setup:
  // an operator's /setup is the operator Getting Started, so the admin's own
  // sign-in is the Agents tab's (openAdminLoginPane).
  await openAdminLoginPane(page);
  const screen = page.locator(".xterm-screen").first();
  await expect(screen).toBeVisible({ timeout: SANDBOX_UP });
  await awaitSelfRunStarted(screen);
  await awaitCapture(page, screen);
  await expect.poll(async () => (await modelAccess(page)).state, { timeout: 120_000 }).toBe("live");

  // Into the preview.
  await page.locator("header").getByRole("button").last().click();
  await page.getByRole("menuitem", { name: MEMBER_MODE.MENU_NEW }).click();
  await expect(page.getByText(MEMBER_MODE.BANNER_NEW)).toBeVisible({ timeout: 60_000 });
  await expect.poll(async () => (await me(page)).member_mode_no_credential, { timeout: 30_000 }).toBe(true);

  // The state every new member is in, and the one the plain toggle structurally
  // cannot show: it clamps the role and leaves the subject alone, so every
  // per-principal credential lookup still finds the admin's own session.
  await page.goto("/setup");
  await expect.poll(async () => (await modelAccess(page)).state, { timeout: 60_000 }).toBe("not_configured");
  await expect(page.getByText(YOUR_MODEL_KEY.NOT_SIGNED_IN_CHIP).first()).toBeVisible({ timeout: 60_000 });

  // …and the one door that could WRITE inside the preview is refused, because a
  // capture made here would land on the admin's own identity and overwrite
  // their real session.
  await page.getByRole("button", { name: AGENTS.SIGN_IN_AWS }).first().click();
  await expect(page.getByText(MEMBER_PREVIEW_SIGNIN_REFUSAL)).toBeVisible({ timeout: 60_000 });

  // Nothing was deleted: the admin's session sits untouched in the store and
  // comes back the moment they exit.
  await page.getByRole("button", { name: MEMBER_MODE.EXIT }).click();
  await expect(page.getByText(MEMBER_MODE.BANNER_NEW)).toBeHidden({ timeout: 60_000 });
  await expect.poll(async () => (await modelAccess(page)).state, { timeout: 60_000 }).toBe("live");
});
