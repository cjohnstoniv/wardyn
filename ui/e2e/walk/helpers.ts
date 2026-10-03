/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

/*
 * SHARED HARNESS FOR THE LIVE AWS SSO WALK.
 *
 * Extracted from ui/e2e/walk/sso-member.spec.ts in 0.7.5 when a SECOND live
 * spec (sso-member-recovery.spec.ts) started against the same cluster, in the
 * same `scripts/run-ui-e2e.sh sso-member sso-member-recovery` invocation. Every
 * comment below moved verbatim with the code it explains — they record failures
 * this walk actually had, and a moved helper that lost them would invite the
 * same ones back.
 *
 * NOTHING HERE IS A TEST. It is the walk's inputs, its two Dex sessions, the
 * four read helpers and the two write helpers. `ui/e2e/walk/**` has ONE owner
 * (lane e2e-sso-path); other lanes hand their constant names over via their
 * own canon docs rather than editing these files.
 */

import { expect, type Page, type APIRequestContext } from "@playwright/test";
// From the CSS-free copy module, NEVER from harness-login-pane: that module
// reaches xterm.css, which Playwright's Node loader cannot load ("No tests found").
import { SELFRUN_MARKER, SIGNIN_PROGRESS } from "../../src/app/components/screens/settings/login-pane-copy";
import { termText } from "../terminal-text";

// ── the walk's inputs (scripts/kind-sso-walk.sh exports every one) ──────────
export const ADMIN_TOKEN = process.env.WARDYN_WALK_ADMIN_TOKEN || "";
export const PIN_ACCOUNT = process.env.WARDYN_WALK_PIN_ACCOUNT || "222222222222";
export const PIN_ROLE = process.env.WARDYN_WALK_PIN_ROLE || "WardynDev";
export const SSO_START_URL = process.env.WARDYN_WALK_SSO_START_URL || "https://wardyn-dev.awsapps.com/start";
export const SSO_REGION = process.env.WARDYN_WALK_SSO_REGION || "us-east-1";
/** The model the walk's provider serves claude-code with: an inference-profile
 *  ARN naming the PINNED account, so the model-account check has both halves. */
export const BEDROCK_MODEL = process.env.WARDYN_WALK_BEDROCK_MODEL || "us.anthropic.claude-sonnet-4-5-20250929-v1:0";
/** The fake's bedrock-runtime stub; unset, the provider addresses real Bedrock. */
export const BEDROCK_BASE_URL = process.env.WARDYN_WALK_BEDROCK_BASE_URL || "";
/** The walk's one model provider — every sign-in and every run goes through it. */
export const WALK_PROVIDER = "bedrock-sso";
export const WALK_PROVIDER_NAME = "Amazon Bedrock (AWS sign-in)";
/** The harness's read-only route to the fake's /_seen — see seen() below. */
export const SEEN_URL = process.env.WARDYN_WALK_SEEN_URL || "https://127.0.0.1:8390/_seen";

export const ADMIN_EMAIL = "admin@wardyn.local";
export const MEMBER_EMAIL = "member@wardyn.local";
/** deploy/kind/sso/README.md's demo literal — a throwaway Dex, no secret. */
const DEX_PASSWORD = "password";

/** harness-login-pane.tsx's aws.doneMarker — the helper's PTY success contract. */
export const SUCCESS_MARKER = "wardyn: aws sso credential captured";
/**
 * cmd/wardyn-aws-sso's failMarker (TestFailMarker_UIParity pins this spelling
 * equal to the pane's). Watched ALONGSIDE the success marker, because a login
 * the control plane has already REFUSED prints this and then exits — and a poll
 * that only ever looks for success spends the whole LOGIN_DONE budget before
 * saying "did not happen", with the reason sitting on the terminal the entire
 * time. Five wasted minutes per red, and a red that names nothing.
 */
export const FAIL_MARKER = "wardyn: aws sso credential rejected:";

/**
 * The fake's entitlement fixture (deploy/kind/sso/awsssofake.yaml) has exactly
 * TWO accounts. Index 1 is the walk's pinned pair; index 0 is the "wrong
 * answer" the account pin exists to refuse. makeMemberActionable() flips
 * between them — see there for why a live spec needs to.
 */
export const OTHER_ACCOUNT = "111111111111";
export const OTHER_ROLE = "DevPower";

// A sandbox on a cluster is a pod: image pull, schedule, proxy sidecar, then a
// device-code flow. Generous, and bounded — an unbounded wait is how a live
// suite turns a failure into a hang.
//
// Both ceilings are ENVIRONMENT-OVERRIDABLE (WARDYN_WALK_SANDBOX_UP_MS /
// WARDYN_WALK_LOGIN_DONE_MS), because the 300s default was tuned on a
// developer box and is too tight for a hosted CI runner: the nightly kind SSO
// walk schedules the CNI, Postgres, the daemon, Dex and every sandbox pod
// concurrently on two vCPUs (#285 — the walk's first nightly dispatch timed
// out here waiting for a freshly created sign-in sandbox). This is a timeout,
// not a correctness bound, so raising it for slow CI hardware proves nothing
// less than the same wait would on a fast box.
function envMs(name: string, fallback: number): number {
  const raw = Number(process.env[name]);
  return Number.isFinite(raw) && raw > 0 ? raw : fallback;
}
export const SANDBOX_UP = envMs("WARDYN_WALK_SANDBOX_UP_MS", 300_000);
export const LOGIN_DONE = envMs("WARDYN_WALK_LOGIN_DONE_MS", 300_000);

/**
 * Sign in through Dex's static-password form.
 *
 * Deliberately a local copy of ui/e2e/demo/sso.ts's dexSignIn rather than an
 * import: that module pulls in the demo narration overlay (captions, beats, a
 * recording), none of which belongs in a test gate. The three locators are the
 * whole of it — Dex's login form is plain HTML with no accessible names.
 */
export async function dexSignIn(page: Page, email: string): Promise<void> {
  // The walk drives a production bundle (no __wardynTerm registry), so terminal text is
  // read from the DOM, which only the Compatible renderer fills. Pin it before the first load.
  await page.addInitScript(() => localStorage.setItem("wardyn.terminal.renderer", "compatible"));
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

export async function dexSignOut(page: Page): Promise<void> {
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

/**
 * The signed-in browser session's own view of who it is.
 *
 * `email` is the field that carries the ADDRESS. `principal` is the OIDC SUBJECT
 * — for this Dex it is the opaque `Cg5nc3YtYWRtaW4tMDAwMRIFbG9jYWw`, not
 * "admin@wardyn.local" — and internal/api/me.go says so in place: the principal
 * is "the key every ownership check compares against", while the console header
 * reads name, then email, then principal. An earlier version of this file
 * asserted the address against `principal` and failed on the walk's very first
 * assertion, with a real, correctly signed-in admin session behind it.
 */
export async function me(page: Page): Promise<{
  principal?: string;
  email?: string;
  operator?: boolean;
  user_view?: boolean;
  user_view_no_credential?: boolean;
  user_preview_available?: boolean;
}> {
  return page.evaluate(async () => {
    const r = await fetch("/api/v1/me", { credentials: "include" });
    return (await r.json()) as Record<string, unknown>;
  });
}

/**
 * The signed-in session's own access to the walk's provider, off its
 * /setup/status `provider_access` — a MEMBER's own answer, not an admin's.
 */
export async function modelAccess(page: Page): Promise<{ state?: string; action?: string }> {
  return (await ownAWSRow(page)) as { state?: string; action?: string };
}

/**
 * The caller's OWN `provider_access` row for the walk's provider.
 *
 * `source_run_id` names the sign-in the caller's stored capture came from. It
 * is the only way a member can corroborate WHICH of their sign-ins the stored
 * blob came from, which is what makes "the capture moved" assertable at all on
 * a member who was already `live`.
 */
export async function ownAWSRow(page: Page): Promise<{
  state?: string;
  action?: string;
  source_run_id?: string;
}> {
  return page.evaluate(async (provider) => {
    const r = await fetch("/api/v1/setup/status", { credentials: "include" });
    const body = (await r.json()) as { provider_access?: Array<Record<string, unknown>> };
    return ((body.provider_access ?? []).find((a) => a.provider === provider) ?? {}) as Record<string, never>;
  }, WALK_PROVIDER);
}

/**
 * The fake's `/_seen`, read through the harness's own port-forward.
 *
 * NOTHING UNDER TEST USES THIS ROUTE. The fourth precondition still holds:
 * every sandbox, the login run and dispatch-time renewal all address the fake by
 * its in-cluster Service name. scripts/kind-sso-walk.sh opens a read-only
 * `kubectl port-forward` purely so the harness can read the counter the fake
 * keeps, and hands the URL over in WARDYN_WALK_SEEN_URL.
 *
 * It used to be `kubectl exec deployment/wardyn -- wget`, which CANNOT work on
 * any deployment: the wardynd image is distroless — no wget, no curl, no shell —
 * so that exec fails with "executable file not found in $PATH". This is the one
 * observation in this file that is not Wardyn asserting about itself, so it
 * failing silently-looking was the worst possible place for it.
 */
export async function seen(): Promise<{
  account_id: string;
  role_name: string;
  bedrock_calls: number;
  bedrock_model: string;
  bedrock_models: string[];
  /** One per sign-in: which peers (a run's proxy pod) called GetRoleCredentials
   *  with it, and which made bedrock calls with its key or its bearer. */
  sessions: Array<{ session: number; role_cred_callers: Record<string, number>; bedrock_callers: Record<string, number> }>;
}> {
  const res = await fetch(SEEN_URL);
  if (!res.ok) throw new Error(`GET ${SEEN_URL}: ${res.status}`);
  return (await res.json()) as Awaited<ReturnType<typeof seen>>;
}

/**
 * Launch an autonomous claude-code run from the signed-in person's own seat and
 * wait for it to have left its pre-run states. RETURNS THE RUN ID — /runs/:id is addressable, so
 * the id is simply the last path segment once the launch has navigated, and a
 * caller that needs to scope an API read (approvals, the run row) to THIS run
 * has no other honest source for it.
 *
 * AUTONOMOUS, NOT INTERACTIVE — and the reason is the vendor CLI, not Wardyn.
 * An interactive run starts the Claude Code TUI, and on a FRESH sandbox that TUI
 * opens on its own first-run onboarding ("Welcome to Claude Code v2.1.231 …
 * Choose the text style that looks best with your terminal"). It sits on that
 * theme picker indefinitely: the seed prompt is never reached, no model call is
 * ever made, GetRoleCredentials is never called, and any /_seen assertion behind
 * it can only time out. Autonomous execs claude non-interactively — no TUI, no
 * onboarding — so the task below actually reaches Bedrock. Reaching a TERMINAL
 * is case 2's subject (P1) and is proven there.
 *
 * (0.7.5: lane `agent-boot-egress` seeds `hasCompletedOnboarding` into the
 * sandbox, which is what makes sso-member-recovery.spec.ts's case H — an
 * INTERACTIVE run that reaches the model — writable at all. This helper stays
 * autonomous; H drives the wizard itself, because the shape it must exercise is
 * the default no-seed one.)
 *
 * LAUNCH NAVIGATES straight to the run page (#125), carrying the 201's
 * advisory `warnings[]` with it (use-launch.ts). The warnings this run legitimately carries are
 * governance working, not failure — `api.anthropic.com` is dropped from egress
 * because this deployment is Bedrock, and the member's resources are capped to
 * the operator maximum.
 *
 * NOT "the Running chip is visible": the fake answers a streaming model call
 * with a non-streaming body, so claude makes three calls and exits 1 within
 * seconds, and the run page only refreshes its state every 4 s
 * (DETAIL_POLL_MS). A chip wait is therefore a coin flip on where a poll tick
 * lands inside a window shorter than the tick — when it loses, the page goes
 * STARTING to FAILED, "Running" never renders, and the wait burns its whole
 * SANDBOX_UP. The run row's own state is read instead, and any state past
 * PENDING/STARTING counts as up: every caller's next assertion (the /_seen
 * counters, the run's approvals) is what proves the agent did its work.
 */
export async function launchAgentRun(page: Page, title: string): Promise<string> {
  await page.goto("/runs/new");
  await page.getByRole("combobox", { name: "Title" }).fill(title);
  await page.getByRole("radio", { name: /^Autonomous/ }).click();
  await page.locator("#nr-task").fill("Reply with the single word: ready.");
  await page.getByRole("button", { name: /^Launch/ }).click();
  await expect(page).toHaveURL(/\/runs\/[0-9a-f-]{36}$/, { timeout: SANDBOX_UP });
  const id = runIDFromURL(page);
  await expect
    .poll(
      async () =>
        page.evaluate(async (runID: string) => {
          const r = await fetch(`/api/v1/runs/${runID}`, { credentials: "include" });
          return ((await r.json().catch(() => ({}))) as { state?: string }).state ?? "";
        }, id),
      { timeout: SANDBOX_UP, message: "the run never left PENDING/STARTING" },
    )
    .not.toMatch(/^(|PENDING|STARTING)$/);
  return id;
}

/** /runs/:id is addressable; the id is the last path segment. */
export function runIDFromURL(page: Page): string {
  const id = new URL(page.url()).pathname.split("/").filter(Boolean).pop() ?? "";
  expect(id, `not on a /runs/:id page: ${page.url()}`).not.toBe("");
  return id;
}

/**
 * The model providers write: the walk's one bedrock_sso provider and its pin,
 * in one PUT.
 *
 * The pin is a PARAMETER because the P4 case re-pins the same provider to a
 * DIFFERENT pair and must send a byte-identical body otherwise — a second
 * hand-written literal is how the two drift and the refusal stops being about
 * the pin. Defaults are the walk's own pinned pair. The same id on every PUT
 * keeps the provider's server-owned uid, so a re-pin is not a new provider and
 * the member's stored capture stays theirs to be graded against the new pin.
 */
export async function putProvider(
  request: APIRequestContext,
  account: string = PIN_ACCOUNT,
  role: string = PIN_ROLE,
): Promise<void> {
  const res = await request.put("/api/v1/model-providers", {
    headers: { Authorization: `Bearer ${ADMIN_TOKEN}`, "Content-Type": "application/json" },
    // handlePutModelProviders decodes STRICTLY into types.ModelProviders.
    // internal/api/model_providers_walk_shape_test.go pins this body's shape
    // from the Go side so the next change reds there, not on a cluster.
    data: {
      providers: [
        {
          id: WALK_PROVIDER,
          name: WALK_PROVIDER_NAME,
          kind: "bedrock_sso",
          bedrock: {
            region: SSO_REGION,
            ...(BEDROCK_BASE_URL ? { base_url: BEDROCK_BASE_URL } : {}),
            sso_start_url: SSO_START_URL,
            sso_account_id: account,
            sso_role_name: role,
          },
          harnesses: [{ harness: "claude-code", model: BEDROCK_MODEL }],
        },
      ],
    },
  });
  expect(res.status(), `PUT /model-providers: ${await res.text()}`).toBe(200);
  // …and claude-code's default: the strip and Getting Started speak for a
  // provider only where it is some agent's default (providerAttention).
  const roster = await request.put("/api/v1/agent-providers", {
    headers: { Authorization: `Bearer ${ADMIN_TOKEN}`, "Content-Type": "application/json" },
    data: { agents: [{ id: "claude-code", default_provider: WALK_PROVIDER }] },
  });
  expect(roster.status(), `PUT /agent-providers: ${await roster.text()}`).toBe(200);
}

/** The walk's provider as the server holds it, read with the walk's admin token. */
export async function getProvider(request: APIRequestContext): Promise<Record<string, unknown>> {
  const res = await request.get("/api/v1/model-providers", {
    headers: { Authorization: `Bearer ${ADMIN_TOKEN}` },
  });
  expect(res.status(), `GET /model-providers: ${await res.text()}`).toBe(200);
  const body = (await res.json()) as { providers?: Array<Record<string, unknown>> };
  return (body.providers ?? []).find((p) => p.id === WALK_PROVIDER) ?? {};
}

/** The walk provider's currently pinned pair, or the fixture default. */
export async function currentPin(request: APIRequestContext): Promise<{ account: string; role: string }> {
  const bedrock = ((await getProvider(request)).bedrock ?? {}) as Record<string, string>;
  return {
    account: bedrock.sso_account_id || PIN_ACCOUNT,
    role: bedrock.sso_role_name || PIN_ROLE,
  };
}

/** The OTHER of the fixture's two valid pairs — see makeMemberActionable(). */
export async function otherPin(request: APIRequestContext): Promise<{ account: string; role: string }> {
  const { account } = await currentPin(request);
  return account === PIN_ACCOUNT
    ? { account: OTHER_ACCOUNT, role: OTHER_ROLE }
    : { account: PIN_ACCOUNT, role: PIN_ROLE };
}

/**
 * Put the member back into a state where the "Sign in to AWS" CTA EXISTS.
 *
 * A `live` member has NO such button, and that is not a bug to work around:
 * the connection row renders it only for an actionable state
 * (not_configured, expired_signin, expiring). sso-member.spec.ts leaves the member `live`,
 * so every case in the recovery file that needs to DRIVE a sign-in has to make
 * one legitimately available first.
 *
 * The recipe is the one sso-member.spec.ts's P4 case already proves end to end:
 * flip the provider's pin to the fixture's OTHER valid account/role pair, and
 * the stored capture now contradicts the pin, so the member's access grades
 * `expired_signin` and the CTA comes back. Signing in again under the new pin heals it.
 *
 * WHY THE OTHER *VALID* PAIR and not an invented account: cmd/wardyn-aws-sso can
 * only ever capture a pair the portal actually mints, so an account the fake does
 * not serve would make the heal unprovable for a reason that has nothing to do
 * with the case under test.
 *
 * It FLIPS rather than sets: the two specs and the cases inside this one hand
 * the pin back and forth, so a helper that always wrote the same pair would be
 * a no-op exactly when the previous case had already moved it there.
 */
export async function makeMemberActionable(request: APIRequestContext): Promise<{ account: string; role: string }> {
  const next = await otherPin(request);
  await putProvider(request, next.account, next.role);
  return next;
}

/** A person's model connection rows live on Your account (packet MP-D, #548):
 *  Getting Started keeps only the summary chip. The card there is collapsed by
 *  default, so it is opened before a row is read or its button pressed. */
export async function openModelConnections(page: Page): Promise<void> {
  await page.goto("/account");
  const toggle = page.getByTestId("model-connections-card").locator("button[aria-expanded]").first();
  await expect(toggle).toBeVisible({ timeout: 60_000 });
  if ((await toggle.getAttribute("aria-expanded")) !== "true") await toggle.click();
  await expect(toggle).toHaveAttribute("aria-expanded", "true");
}

/**
 * Open the member's sign-in pane from Your account and wait for its terminal.
 *
 * The walk's provider is the member's one model connection, so Getting
 * Started renders one "Sign in to AWS" button for it; `.first()` keeps the
 * locator strict-mode safe should the shell strip offer the same door.
 *
 * The pane launches the login sandbox the moment it opens — the provider
 * carries the start URL, so there is nothing to ask first.
 *
 * #628: Start opens NO tab any more — the door narrates the start in its own
 * steps, and the provider tab opens only from its Open button, which this walk
 * never needs (the on-cluster fake pre-approves every device code). So every
 * sign-in the walk drives asserts both halves: the steps are on screen, and
 * no page — least of all an about:blank placeholder — opened on the click.
 */
export async function openLoginPane(page: Page): Promise<void> {
  await openModelConnections(page);
  const cta = page.getByRole("button", { name: "Sign in to AWS" }).first();
  await expect(cta).toBeVisible({ timeout: 60_000 });
  await cta.click();
  const opened: Page[] = [];
  const onPage = (p: Page) => opened.push(p);
  page.context().on("page", onPage);
  try {
    const progress = page.getByTestId("signin-progress").first();
    await expect(progress).toBeVisible({ timeout: 60_000 });
    await expect(progress).toContainText(SIGNIN_PROGRESS.STEP_START);
    expect(opened.map((p) => p.url()), "the sign-in opened a tab").toEqual([]);
  } finally {
    page.context().off("page", onPage);
  }
}

/**
 * #891: the same door as openLoginPane, but for the ONE sign-in in the walk
 * that scripts/kind-sso-walk.sh's registry step guarantees is a genuine cold
 * pull — the aws-sso login image is served from a local registry under a
 * fresh tag every walk (never `kind load`ed any more), so the kubelet has
 * never seen this manifest and must actually pull it. On a warm node
 * (`kind load`, or a tag it already holds) the step goes start -> wait with no
 * "download" li ever rendered, so this assertion is expected to be
 * non-vacuous rather than asserted from a guess: dropping the runner Role's
 * `events: list` (the fail-closed path the k8s runner reads pod Events
 * through, #881) should reproduce that same skip. Not run by hand — this
 * walk has not executed against a real kind cluster in this lane — so that
 * stays unproven until the next `kind-sso-walk` nightly run.
 *
 * Reuses openLoginPane for the click through STEP_START, which keeps its
 * "no tab opened" assertion (#628) on this sign-in too. Then keeps polling
 * past that point for the download li: it can be gone within seconds on a
 * small image, so the poll starts right after STEP_START rather than
 * waiting on anything else (signInThroughPane's own race to capture).
 */
export async function openLoginPaneAssertingColdPull(page: Page): Promise<void> {
  await openLoginPane(page);
  const progress = page.getByTestId("signin-progress").first();
  // The download row by POSITION (marksFor's rows are always [start, download,
  // wait?], signin-progress.tsx), not by filtering on STEP_DOWNLOAD_ACTIVE's
  // label text: that text only matches while the row is ALSO in the active
  // state, so a text filter plus an attribute check must both land on the same
  // poll — the rate-limited cold pull (scripts/lib/kind-sso-walk-cold-pull.sh)
  // keeps the row active for seconds, not minutes, so the two could straddle
  // it and time out on a real cold pull. Position is stable regardless of state.
  const downloadStep = progress.getByRole("listitem").nth(1);
  await expect(
    downloadStep,
    "the download step never lit — either the aws-sso image was not a cold pull (see scripts/kind-sso-walk.sh's registry step), or the runner Role lost events:list (#881's fail-closed path)",
  ).toHaveAttribute("data-state", "active", { timeout: 90_000 });
}

/**
 * Wait for the SANDBOX to announce that it is running the sign-in itself.
 *
 * 0.7.5 (lane login-sandbox-selfrun): the aws-sso image creates the `wardyn`
 * tmux session on signin-pane.sh and runs the chained command ONCE, so the
 * console types nothing. This replaces a 60 s poll for the literal text
 * "aws sso login" followed by a TYPING fallback — after the self-run that text
 * never appears (the pane body RUNS the pair; nothing echoes its argv), so every
 * walk burned the full 60 s and then typed a SECOND login into a pane already
 * signing in: the double-run the lane warns about, whose second half is an
 * `already_captured` 409 and the pane's fail marker.
 *
 * SELFRUN_MARKER is imported, not quoted — TestSelfRunBanner_UIParity pins the
 * shell banner's prefix equal to this constant, and a literal here would assert
 * a third spelling neither side is bound to.
 *
 * THE SERIES LAW, kept with the code it governs: assertions on an xterm buffer
 * are `expect.poll(innerText)`, never `toContainText`. `toContainText` starves
 * on a terminal that repaints under load — it re-queries the same node and can
 * miss every frame the text was in. (It also matters that this reads
 * `innerText`: the walk pins the Compatible renderer in dexSignIn, so xterm's DOM
 * renderer is mounted and the buffer really is in the DOM to be read.)
 *
 * STRICT, and only for a caller with NO console pane attached — case C, which
 * has abandoned the pane and is watching from the Runs list. A pane-attached
 * sign-in tears the terminal down on capture, so it uses signInThroughPane()
 * below instead, which never depends on the terminal existing.
 */
export async function awaitSelfRunStarted(screen: ReturnType<Page["locator"]>): Promise<void> {
  await expect
    .poll(async () => (await termText(screen).catch(() => "")).includes(SELFRUN_MARKER), {
      timeout: 120_000,
    })
    .toBe(true);
}

/**
 * Wait for the capture to land — the marker if we catch it in flight, or the
 * SERVER's own answer if the pane beat us to the teardown.
 *
 * THE PANE TEARS THE TERMINAL DOWN THE MOMENT IT SEES THE MARKER, so polling
 * for the marker alone is a race the test loses on a slow box.
 * harness-login-pane.tsx's handleOutput → confirmCapture fires `killRun` and
 * then `onDone`, and the parent closes the pane — all within milliseconds of
 * the marker being printed. `termText(screen)` then reads a detached node
 * (or throws), so the poll can watch for its full five minutes while the
 * capture has ALREADY succeeded server-side. That is exactly what happened:
 * `harness.credential.capture` in the audit, `session.detach reason="client
 * closed"` right after it, and a spec still waiting.
 *
 * So accept either witness, and keep failing fast on the helper's refusal. The
 * server fact is the stronger of the two: it is what every assertion after this
 * one rests on.
 */
export async function awaitCapture(page: Page, screen: ReturnType<Page["locator"]>): Promise<void> {
  await expect
    .poll(
      async () => {
        // BOUNDED, and that bound is the whole fix. `locator.innerText()` takes
        // no default timeout, so once the pane has unmounted the terminal this
        // call does not throw — it WAITS, swallowing the entire LOGIN_DONE
        // budget inside a single poll iteration, and the server is never asked.
        // That is what made this case fail at exactly 300s with the provider access
        // sitting at "live" the whole time.
        const text = await termText(screen).catch(() => "");
        if (text.includes(FAIL_MARKER)) {
          const line = text.split("\n").find((l) => l.includes(FAIL_MARKER)) ?? FAIL_MARKER;
          throw new Error(`the login helper refused this capture: ${line.trim()}`);
        }
        if (text.includes(SUCCESS_MARKER)) return true;
        return (await modelAccess(page)).state === "live";
      },
      { timeout: LOGIN_DONE },
    )
    .toBe(true);
}

/**
 * Drive a sign-in THROUGH THE CONSOLE PANE, from opening it to the capture
 * landing, without ever depending on the terminal being on screen.
 *
 * WHY THE TERMINAL CANNOT BE THE WITNESS ANY MORE. Since 0.7.5 the sandbox
 * starts the sign-in at BOOT (signin-pane.sh) and the on-cluster fake
 * pre-approves every device code permanently, so for a no-repo run the whole
 * pair can finish before — or within a fraction of a second of — the pane
 * attaching. The pane sees the done marker in its first tmux redraw, corroborates
 * it with the server, kills the run and unmounts the terminal in ~0.2-0.5 s.
 * `expect(screen).toBeVisible()` polls at second granularity once it has waited
 * a little, so it can miss that window entirely and then burn its whole 300 s
 * budget on a sign-in that SUCCEEDED. In 0.7.4 this could not happen: the
 * console typed the command AFTER attaching, so the terminal was necessarily
 * still there.
 *
 * SO: two witnesses, neither of them the DOM node's presence.
 *
 *   1. the sandbox ANNOUNCED itself (SELFRUN_MARKER in the pane), or
 *   2. the SERVER says the stored capture MOVED — the caller's own aws row
 *      carries a `source_run_id` different from the one it carried before the
 *      pane was opened.
 *
 * (2) is the one that is never vacuous. "reaches live" is not usable here: a
 * member who was ALREADY live reads live throughout, which is exactly the trap
 * case D exists to avoid. The refusal marker still fails fast, because a login
 * the control plane refused prints it and exits, and waiting the full budget for
 * a reason sitting on the terminal is five wasted minutes per red.
 */
export async function signInThroughPane(page: Page, openPane: (p: Page) => Promise<void>): Promise<void> {
  const before = (await ownAWSRow(page)).source_run_id ?? "";
  await openPane(page);
  const screen = page.locator(".xterm-screen").first();
  const pane = page.getByTestId("harness-login-pane");

  // Bounded read; see awaitCapture() for why the bound is the whole fix.
  const paneText = async (): Promise<string> => {
    const text = await termText(screen).catch(() => "");
    if (text.includes(FAIL_MARKER)) {
      const line = text.split("\n").find((l) => l.includes(FAIL_MARKER)) ?? FAIL_MARKER;
      throw new Error(`the login helper refused this capture: ${line.trim()}`);
    }
    // The pane's error arm before any terminal attached ("the sandbox could not
    // be created: …", a stuck or refused start): nothing will ever capture, so
    // fail now with the pane's own sentence instead of running out both budgets.
    const alert = pane.getByRole("alert");
    if ((await screen.count()) === 0 && (await alert.count()) > 0) {
      const said = (await alert.first().innerText({ timeout: 1_000 }).catch(() => "")).trim();
      if (said) throw new Error(`the sign-in pane showed its error before a terminal attached: ${said}`);
    }
    return text;
  };
  const captureMoved = async (): Promise<boolean> => {
    const row = await ownAWSRow(page);
    return !!row.source_run_id && row.source_run_id !== before;
  };

  // The sandbox is up and doing something — or it has already finished.
  await expect
    .poll(async () => (await paneText()).includes(SELFRUN_MARKER) || (await captureMoved()), { timeout: SANDBOX_UP })
    .toBe(true);
  // …and the capture itself landed, on a NEW login run.
  await expect
    .poll(async () => {
      await paneText();
      return captureMoved();
    }, { timeout: LOGIN_DONE })
    .toBe(true);
}
