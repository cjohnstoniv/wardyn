/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// ModelStep — the Getting-started "Connect a model" step body: the API-key +
// resident-CLI subscription list (B7), grouped by provider family (Claude/
// Anthropic, OpenAI/Codex), plus an intro sentence and, when running on the
// containerized control plane (a coming-soon team feature) that can't see the
// host's Claude login, a "use host mode" hint. Reads from the SetupStatus the
// daemon already reports and offers the right action:
//   - API keys  -> one-click AddSecretDialog (setSecret)
//   - CLI logins / runtimes -> a SetupGuide the operator runs on the host
// (The barrier picker moved to ./environment-step and the composer-backends list
// was dropped from setup by owner decision; this module is LLM access only. Has
// NO composer-backends section by design — owner decision: zero composer UI here.)
import * as React from "react";
import { KeyRound, Loader2, Lock, Plus } from "lucide-react";
import type { SetupStatus } from "../../../lib/types";
import { harnessAuth as api } from "../../../lib/api/harness-auth";
import { Button } from "../../ui/button";
import { Chip } from "../../wardyn/primitives";
import { StatusChip } from "../../wardyn/status-chip";
import type { StatusKind } from "../../wardyn/copy";
import { BTN } from "../../wardyn/copy";
import { PROVIDER_GUIDES, type SetupGuide } from "./setup-guide";
import { HarnessLoginPane } from "./harness-login-pane";
import { OptionCard } from "../new-run/step-shell";
import type { Readiness } from "../onboarding/intro";

// The three harnesses a run can use: Claude Code (Anthropic), Codex (OpenAI), or
// none (bring-your-own container / interactive / plain command). Selecting one
// reveals only that harness's real connection methods — instead of a flat list of
// every provider's every credential type. "none" is a first-class choice, not an
// absence: it maps to the explicit skip.
type Harness = "claude" | "codex" | "none";

type RowState = "ready" | "todo";
type Provider = SetupStatus["providers"][number];

function rowStatus(state: RowState, rechecking: boolean): StatusKind {
  if (rechecking && state !== "ready") return "checking";
  return state === "ready" ? "ready" : "needs-setup";
}

// ---------------------------------------------------------------------------
// LLM access: API keys (one-click) + resident CLI subscriptions (guided)
// ---------------------------------------------------------------------------
function AccessRow({
  status,
  reason,
  label,
  detail,
  residency,
  action,
}: {
  status: StatusKind;
  reason?: string;
  label: React.ReactNode;
  detail?: React.ReactNode;
  // Small proxy-injected/resident-at-run-time indicator (see PROXY_INJECTED_CHIP
  // / BEDROCK_RESIDENT_CHIP below) — optional, only set for the Claude/Anthropic
  // auth-mode rows the safest-path guidance covers.
  residency?: React.ReactNode;
  action?: React.ReactNode;
}) {
  return (
    <li className="flex flex-wrap items-center gap-3 rounded-xl border border-border bg-card p-3.5">
      <div className="min-w-[200px] flex-1">
        <div className="text-sm font-semibold text-foreground">{label}</div>
        {detail && <p className="mt-0.5 text-xs leading-snug text-muted-foreground">{detail}</p>}
      </div>
      {residency}
      <StatusChip status={status} reason={reason} />
      {action && <div className="shrink-0">{action}</div>}
    </li>
  );
}

function hasSecret(present: string[], re: RegExp): boolean {
  return present.some((n) => re.test(n));
}

// Auth-mode residency framing (best-practice safest-path guidance): where the
// live secret lives at run time differs materially across these options, so
// each one wears a small indicator instead of leaving it unstated.
// Proxy-injected = the live secret never leaves the proxy — the sandbox holds
// nothing (API key) or an inert sentinel (subscription/managed). Bedrock's
// static-key/SSO SigV4 paths can't be proxy-injected, so those credentials
// land resident in the sandbox at run time — amber, with the static-vs-SSO
// nuance spelled out under that option below. (The Bedrock BEARER transport is
// proxy-injected, but this row's actions only wire the static/SSO paths.)
const PROXY_INJECTED_CHIP = (
  <Chip tone="success" className="uppercase tracking-wide">
    proxy-injected
  </Chip>
);
// The personal-subscription row hedges with "(default)": the
// WARDYN_SUBSCRIPTION_INJECT=off escape hatch stages a resident copy instead.
const PROXY_INJECTED_DEFAULT_CHIP = (
  <Chip tone="success" className="uppercase tracking-wide">
    proxy-injected (default)
  </Chip>
);
const BEDROCK_RESIDENT_CHIP = (
  <Chip tone="warning" className="uppercase tracking-wide">
    resident
  </Chip>
);
// Bedrock's bearer transport IS proxy-injected (unlike its SSO/static-key modes),
// so its sub-row wears the green chip.
const BEDROCK_BEARER_CHIP = (
  <Chip tone="success" className="uppercase tracking-wide">
    proxy-injected
  </Chip>
);

// SetupOption is one not-yet-configured way to connect the chosen harness — a
// compact "Set up:" button under the method list (container login, add key, …).
type SetupOption = { key: string; label: string; onClick: () => void; icon?: React.ReactNode };

// The "Set up:" button row, identical for every harness.
function SetupOptionRow({ options }: { options: SetupOption[] }) {
  if (options.length === 0) return null;
  return (
    <div className="flex flex-wrap items-center gap-1.5">
      {options.map((o) => (
        <Button key={o.key} size="sm" variant="outline" onClick={o.onClick}>
          {o.icon ?? <Plus className="size-3.5" />} {o.label}
        </Button>
      ))}
    </div>
  );
}

export function ModelStep({
  status,
  readiness,
  skipped,
  onAddSecret,
  onSetup,
  onRecheck,
  rechecking,
  onSkip,
}: {
  status: SetupStatus;
  readiness: Readiness;
  // The operator explicitly skipped this optional step (setup-screen state).
  skipped?: boolean;
  onAddSecret: (name: string) => void;
  onSetup: (g: SetupGuide) => void;
  onRecheck: () => void;
  rechecking: boolean;
  // Mark the (optional) model step decided-as-skipped and advance. Absent in
  // contexts that don't offer skipping (e.g. a standalone render).
  onSkip?: () => void;
}) {
  // A sealed (compose/team) wardynd can never see a host ~/.claude login: offering
  // "Install / Log in to Claude CLI" here would dangle a path that can NEVER satisfy
  // this step (claudeSubDetected reads wardynd's own view, blind to the host by
  // construction). Container login is offered instead under the Claude harness.
  const sealedControlPlane = status.deployment?.host_like === false;

  const present = status.secrets.present;
  const anthropic = hasSecret(present, /anthropic/i);
  const openai = hasSecret(present, /openai/i);
  const claude = status.providers.find((p) => p.tool === "claude");
  const codex = status.providers.find((p) => p.tool === "codex");
  // AWS Bedrock: an enterprise Anthropic transport (no direct Anthropic egress,
  // billed via AWS). Region/model are operator boot-time config (read-only
  // here — no live write path, same as the AgentAnthropicModel pin); only the
  // AWS credentials are addable from this screen. bedrock_provider's own
  // detail (ready vs partially-configured, naming exactly what's missing) is
  // reused verbatim so this row can never disagree with the check — the same
  // discipline the Claude subscription row applies to llmProviderDetail.
  const bedrock = status.bedrock ?? { creds_present: false };
  // Prefer the server-computed readiness (accounts for the ~/.aws mount and bearer
  // credential sources, not just resident keys); fall back to the old derivation
  // only for an older daemon that predates `ready`.
  const bedrockReady =
    bedrock.ready ??
    (!!bedrock.region &&
      !!bedrock.model &&
      (bedrock.creds_present || !!bedrock.aws_mount || !!bedrock.bearer_present));
  const bedrockDetail =
    status.checks.find((c) => c.id === "bedrock_provider")?.detail ??
    "An enterprise path via AWS — no direct Anthropic egress, billed via AWS.";
  // The llm_provider check carries the single authoritative subscription sentence
  // (fresh vs EXPIRED, inject on/off). A logged-in resident Claude CLI is the top
  // provenance signal, so when this row is a subscription that check IS this row's
  // story — reuse it verbatim so the check row and this row can never disagree on
  // an expired subscription (binding review note: no ready-vs-EXPIRED contradiction).
  const llmProviderDetail = status.checks.find((c) => c.id === "llm_provider")?.detail;
  // claude_subscription_staging: a resident login that is NOT staged for the
  // per-run subscription mount (the headless-`make setup` skip). Rendered
  // verbatim under the subscription row — same never-disagree discipline as the
  // Bedrock and llm_provider rows above.
  const stagingCheck = status.checks.find((c) => c.id === "claude_subscription_staging");
  // Wardyn-managed subscription (captured via container login) — the compose-mode
  // path with no resident host login. The harness_credential check carries the
  // authoritative aging sentence, reused verbatim.
  const managedCred = status.harness?.find((h) => h.provider === "anthropic" && h.captured);
  const managedCheck = status.checks.find((c) => c.id === "harness_credential");
  // Inline container-login flow toggle (self-contained pane; never routes away).
  const [loginOpen, setLoginOpen] = React.useState(false);
  // Bedrock has THREE credential modes, so "Set up AWS Bedrock" must reveal the
  // chooser — jumping straight into one mode's secret dialog is the same
  // presumes-one-method funnel this redesign removes.
  const [bedrockOpen, setBedrockOpen] = React.useState(false);
  // Containerized AWS SSO login (second container-login provider).
  const [awsLoginOpen, setAwsLoginOpen] = React.useState(false);
  const awsSSOCred = status.harness?.find((h) => h.provider === "aws" && h.captured);
  const [disconnecting, setDisconnecting] = React.useState(false);
  const disconnectManaged = async () => {
    setDisconnecting(true);
    try {
      await api.harnessDisconnect("anthropic");
      onRecheck();
    } finally {
      setDisconnecting(false);
    }
  };

  const keyRow = (
    name: string,
    label: string,
    detail: string,
    configured: boolean,
    residency?: React.ReactNode,
  ) => (
    <AccessRow
      status={rowStatus(configured ? "ready" : "todo", rechecking)}
      label={label}
      detail={detail}
      residency={residency}
      action={
        <Button size="sm" variant="outline" onClick={() => onAddSecret(name)}>
          <KeyRound className="size-3.5" /> {configured ? "Edit" : "Add key"}
        </Button>
      }
    />
  );

  const cliRow = (
    provider: Provider | undefined,
    label: string,
    guide: SetupGuide,
    connectedDetail: string,
    notInstalledDetail: string,
    // When logged in, an authoritative auth-mode/expiry sentence that OVERRIDES
    // connectedDetail. Used only for the Claude subscription row (see below) so it
    // renders verbatim from the llm_provider check — never a contradicting copy.
    authDetail?: string,
    residency?: React.ReactNode,
  ) => {
    const installed = !!provider?.installed;
    const loggedIn = !!provider?.logged_in;
    const state: RowState = loggedIn ? "ready" : "todo";
    return (
      <AccessRow
        status={rowStatus(state, rechecking)}
        label={label}
        detail={
          loggedIn
            ? (authDetail ??
              `${connectedDetail}${provider?.login_detected_via ? ` (via ${provider.login_detected_via})` : ""}`)
            : installed
              ? "Installed but not logged in."
              : notInstalledDetail
        }
        residency={residency}
        action={
          !installed ? (
            // B7: a dead disabled button becomes a real "Install guide →" link.
            <Button size="sm" variant="link" onClick={() => onSetup(guide)}>
              {BTN.installGuide}
            </Button>
          ) : (
            <Button size="sm" variant="outline" onClick={onRecheck} disabled={rechecking}>
              {BTN.recheckLogin}
            </Button>
          )
        }
      />
    );
  };

  // Per-mechanism detection (unchanged signals, just named per family). A CLI is
  // "detected" when logged in; a key when a matching secret is present; Bedrock
  // when any of region/model/creds is configured (its row's chip then shows
  // ready vs needs-creds). "Connected" = the family has at least one usable path.
  const claudeSubDetected = !!claude?.logged_in;
  const claudeInstalled = !!claude?.installed;
  const bedrockConfigured = !!(
    bedrock.region ||
    bedrock.model ||
    bedrock.creds_present ||
    bedrock.aws_mount ||
    bedrock.bearer_present
  );
  const codexDetected = !!codex?.logged_in;
  const codexInstalled = !!codex?.installed;
  const anthropicConnected = anthropic || claudeSubDetected || bedrockReady || !!managedCred;
  const openaiConnected = openai || codexDetected;

  // Harness-first: the operator picks a harness, then sees only its methods. Start
  // on whichever family is already connected (so a return visit lands on what they
  // set up), else default to Claude Code. "none" is chosen explicitly via its card.
  const [harness, setHarness] = React.useState<Harness>(
    anthropicConnected ? "claude" : openaiConnected ? "codex" : "claude",
  );

  // Bedrock exposes its THREE credential modes explicitly, each with its own
  // residency chip, ordered by the precedence resolveBedrockAuth enforces
  // (internal/api/runs_bedrock.go): bearer API key > ~/.aws SSO mount > static
  // access/secret (+ optional session token). Region/model are wardynd boot-time
  // config (no write API), shown as a status line, not an addable field.
  const bedrockRow = (
    <React.Fragment key="bedrock">
      <li className="space-y-2.5 rounded-xl border border-border bg-card p-3.5">
        <div className="flex flex-wrap items-center gap-3">
          <div className="min-w-[200px] flex-1">
            <div className="text-sm font-semibold text-foreground">AWS Bedrock</div>
            <p className="mt-0.5 text-xs leading-snug text-muted-foreground">{bedrockDetail}</p>
          </div>
          <StatusChip status={rowStatus(bedrockReady ? "ready" : "todo", rechecking)} />
        </div>

        {/* Region + model: boot-time config, shown (not editable) with the exact
            env to set when unset — never a dead "needs-creds" end. */}
        <div className="rounded-lg border border-border bg-surface-2/40 px-3 py-2 text-xs">
          <span className="font-mono text-foreground">
            {bedrock.region || "region unset"} · {bedrock.model || "model unset"}
          </span>
          {(!bedrock.region || !bedrock.model) && (
            <p className="mt-1 leading-relaxed text-muted-foreground">
              Region + model are wardynd boot-time config: set WARDYN_BEDROCK_REGION /
              WARDYN_BEDROCK_MODEL (or -bedrock-region / -bedrock-model), restart wardynd, then
              Re-check.
            </p>
          )}
        </div>

        {/* Mode 1 — bearer (proxy-injected, preferred). */}
        <div className="flex flex-wrap items-center gap-2.5 rounded-lg border border-border bg-surface-2/40 p-2.5">
          <div className="min-w-[180px] flex-1">
            <div className="flex items-center gap-1.5 text-xs font-semibold text-foreground">
              Bearer token {BEDROCK_BEARER_CHIP}
              <Chip tone="primary">Recommended</Chip>
            </div>
            <p className="mt-0.5 text-[0.6875rem] leading-snug text-muted-foreground">
              An Amazon Bedrock API key, injected at the proxy — never stored in the sandbox.
            </p>
          </div>
          <Button size="sm" variant="outline" onClick={() => onAddSecret("bedrock-api-key")}>
            <KeyRound className="size-3.5" /> {bedrock.bearer_present ? "Edit" : "Add key"}
          </Button>
        </div>

        {/* Mode 2 — AWS SSO via a CONTAINERIZED login: no host ~/.aws, works on a
            sealed compose control plane. Wardyn captures the SSO session; later
            Bedrock runs exchange it for short-lived role credentials. The session
            itself is resident in the sandbox for now (amber) — narrower than
            mounting the whole ~/.aws, and Phase B proxy-injects it instead. */}
        <div className="flex flex-wrap items-center gap-2.5 rounded-lg border border-border bg-surface-2/40 p-2.5">
          <div className="min-w-[180px] flex-1">
            <div className="flex items-center gap-1.5 text-xs font-semibold text-foreground">
              AWS SSO (containerized login) {BEDROCK_RESIDENT_CHIP}
              {awsSSOCred && !awsSSOCred.expired && <Chip tone="success">Connected</Chip>}
              {awsSSOCred?.expired && <Chip tone="warning">Expired</Chip>}
            </div>
            <p className="mt-0.5 text-[0.6875rem] leading-snug text-muted-foreground">
              {awsSSOCred
                ? awsSSOCred.expired
                  ? `Session expired ${awsSSOCred.expires_at ?? ""}. Re-run the login to refresh it.`
                  : `Session connected, expires ${awsSSOCred.expires_at ?? "—"}. Bedrock runs exchange it for short-lived role credentials.`
                : "Log in once in a sandbox: give Wardyn your AWS access portal URL, then approve the device code in any browser. No host ~/.aws mount and no static keys."}
            </p>
          </div>
          <Button size="sm" variant="outline" onClick={() => setAwsLoginOpen(true)}>
            <KeyRound className="size-3.5" /> {awsSSOCred ? "Re-login" : "Log in"}
          </Button>
        </div>
        {awsLoginOpen && (
          <HarnessLoginPane
            provider="aws"
            onDone={() => {
              setAwsLoginOpen(false);
              onRecheck();
            }}
            onCancel={() => setAwsLoginOpen(false)}
          />
        )}

        {/* Mode 3 — host ~/.aws mount (boot config, not an addable secret). */}
        <div className="flex flex-wrap items-center gap-2.5 rounded-lg border border-border bg-surface-2/40 p-2.5">
          <div className="min-w-[180px] flex-1">
            <div className="flex items-center gap-1.5 text-xs font-semibold text-foreground">
              Host ~/.aws profile {BEDROCK_RESIDENT_CHIP}
            </div>
            <p className="mt-0.5 text-[0.6875rem] leading-snug text-muted-foreground">
              Mount the operator&apos;s existing ~/.aws read-only (exposes every profile in it). Set
              WARDYN_BEDROCK_AWS_DIR (+ WARDYN_BEDROCK_AWS_PROFILE) on wardynd, restart, then Re-check.
            </p>
          </div>
          <Chip tone={bedrock.aws_mount ? "success" : "neutral"} className="uppercase tracking-wide">
            {bedrock.aws_mount ? "mounted" : "not mounted"}
          </Chip>
        </div>

        {/* Mode 3 — static access keys (resident). */}
        <div className="flex flex-wrap items-center gap-2.5 rounded-lg border border-border bg-surface-2/40 p-2.5">
          <div className="min-w-[180px] flex-1">
            <div className="flex items-center gap-1.5 text-xs font-semibold text-foreground">
              Access keys {BEDROCK_RESIDENT_CHIP}
            </div>
            <p className="mt-0.5 text-[0.6875rem] leading-snug text-muted-foreground">
              Static access key + secret (+ optional session token for SSO/STS); signed in-process,
              so they live in the sandbox.
            </p>
          </div>
          <div className="flex flex-wrap gap-1.5">
            <Button size="sm" variant="outline" onClick={() => onAddSecret("aws-access-key-id")}>
              <KeyRound className="size-3.5" /> Access key
            </Button>
            <Button size="sm" variant="outline" onClick={() => onAddSecret("aws-secret-access-key")}>
              <KeyRound className="size-3.5" /> Secret key
            </Button>
            <Button size="sm" variant="outline" onClick={() => onAddSecret("aws-session-token")}>
              <KeyRound className="size-3.5" /> Session token
            </Button>
          </div>
        </div>
      </li>
    </React.Fragment>
  );

  const subRow = (
    <React.Fragment key="claude-sub">
      {cliRow(
        claude,
        "Claude subscription (Claude Code CLI)",
        PROVIDER_GUIDES.claude,
        "Login detected in ~/.claude (not verified live) — agents use your existing subscription",
        "Not installed on this host.",
        // Subscription auth mode: render the llm_provider check's expiry-aware
        // sentence ("w/ Claude subscription", valid vs EXPIRED) so the two rows stay
        // consistent. Non-subscription logins fall back to the generic copy above.
        claude?.auth_mode === "subscription" ? llmProviderDetail : undefined,
        PROXY_INJECTED_DEFAULT_CHIP,
      )}
      {stagingCheck?.status === "warn" && (
        <p className="pl-1 text-xs leading-relaxed text-warning">
          {stagingCheck.detail} {stagingCheck.fix}
        </p>
      )}
    </React.Fragment>
  );
  const managedRow = (
    <React.Fragment key="claude-managed">
      <AccessRow
        status={rowStatus(managedCred?.aging ? "todo" : "ready", rechecking)}
        label="Claude subscription (managed by Wardyn)"
        detail={
          managedCheck?.detail ??
          "A Wardyn-managed Claude subscription token is injected proxy-side into every run — the sandbox holds only an inert sentinel."
        }
        residency={PROXY_INJECTED_CHIP}
        action={
          <div className="flex flex-wrap gap-1.5">
            <Button size="sm" variant="outline" onClick={() => setLoginOpen(true)}>
              Reconnect
            </Button>
            <Button size="sm" variant="ghost" onClick={disconnectManaged} disabled={disconnecting}>
              {disconnecting ? <Loader2 className="size-3.5 animate-spin" /> : null} Disconnect
            </Button>
          </div>
        }
      />
      {managedCred?.aging && managedCheck?.fix && (
        <p className="pl-1 text-xs leading-relaxed text-warning">{managedCheck.fix}</p>
      )}
    </React.Fragment>
  );
  const anthropicKeyRow = (
    <React.Fragment key="anthropic-key">
      {keyRow(
        "anthropic-api-key",
        "Anthropic API key",
        "Enables Claude models over the API — an alternative to the CLI login.",
        anthropic,
        PROXY_INJECTED_CHIP,
      )}
    </React.Fragment>
  );
  const openaiKeyRow = (
    <React.Fragment key="openai-key">
      {keyRow("openai-api-key", "OpenAI API key", "Enables GPT models over the API.", openai)}
    </React.Fragment>
  );
  const codexRow = (
    <React.Fragment key="codex-cli">
      {cliRow(
        codex,
        "Codex CLI",
        PROVIDER_GUIDES.codex,
        "Login detected (not verified live) — agents can use Codex",
        "Not installed on this host — takes about two minutes.",
      )}
    </React.Fragment>
  );

  // Claude-family connect affordances not yet satisfied (container login, add key,
  // etc.), shown as a "Set up:" chip row under the method list. Same gating as
  // before: the resident-CLI route is hidden on a sealed control plane and once a
  // managed token exists; container login only when nothing is connected yet.
  // Options are named for the CREDENTIAL they set up (not the mechanism), so the
  // three real Claude paths read as a parallel choice: subscription / API key /
  // Bedrock. The mechanism (container login vs a host CLI login) is explained
  // inside the flow each one opens, not in the button label.
  const claudeOptions = [
    !claudeSubDetected &&
      !managedCred && {
        key: "sub",
        label: "Set up Claude subscription",
        icon: <KeyRound className="size-3.5" />,
        onClick: () => setLoginOpen(true),
      },
    !anthropic && {
      key: "akey",
      label: "Set up Anthropic API key",
      icon: <KeyRound className="size-3.5" />,
      onClick: () => onAddSecret("anthropic-api-key"),
    },
    // Bedrock is its own connect path — it must stay reachable even when every
    // other Claude option is satisfied (a logged-in CLI + an Anthropic key), and
    // it OPENS THE MODE CHOOSER rather than one mode's secret dialog.
    !bedrockConfigured &&
      !bedrockOpen && {
        key: "bedrock",
        label: "Set up AWS Bedrock",
        icon: <KeyRound className="size-3.5" />,
        onClick: () => setBedrockOpen(true),
      },
  ].filter(Boolean) as SetupOption[];

  const codexOptions = [
    !openai && {
      key: "okey",
      label: "Set up OpenAI API key",
      icon: <KeyRound className="size-3.5" />,
      onClick: () => onAddSecret("openai-api-key"),
    },
    // Codex has no container-login path (agentHarnessLogin implements claude-code
    // only), so on a sealed wardynd a resident Codex login can neither be detected
    // nor captured — the API key is the only reachable route.
    !codexDetected &&
      !sealedControlPlane && {
        key: "codex",
        label: codexInstalled ? "Log in to Codex CLI" : "Install Codex CLI",
        onClick: () => onSetup(PROVIDER_GUIDES.codex),
      },
  ].filter(Boolean) as SetupOption[];

  return (
    <div className="space-y-5">
      {/* ── What this is, and whether you need it ─────────────────────────── */}
      <div className="space-y-3">
        <p className="text-sm leading-relaxed text-muted-foreground">
          A harness is the coding agent Wardyn runs inside the sandbox — Claude Code (Anthropic) or
          Codex (OpenAI) — and the model is what it thinks with.
        </p>
        <div className="grid gap-3 sm:grid-cols-2">
          <div className="rounded-xl border border-border bg-card p-3.5">
            <div className="flex flex-wrap items-center gap-2">
              <Chip tone="success" className="uppercase tracking-wide">
                Required
              </Chip>
              <span className="text-sm font-semibold text-foreground">for AI Composer</span>
              <Chip tone="primary" title="This feature is in beta — expect rough edges.">
                Beta
              </Chip>
            </div>
            <p className="mt-1.5 text-xs leading-relaxed text-muted-foreground">
              Describe a task in plain English and Wardyn&apos;s composer proposes and launches a
              governed sandbox for it. This mode needs a connected model.
            </p>
          </div>
          <div className="rounded-xl border border-border bg-card p-3.5">
            <div className="flex flex-wrap items-center gap-2">
              <Chip tone="neutral" className="uppercase tracking-wide">
                Optional
              </Chip>
              <span className="text-sm font-semibold text-foreground">everywhere else</span>
            </div>
            <p className="mt-1.5 text-xs leading-relaxed text-muted-foreground">
              Connect a model for a ready-to-go base sandbox with an agent already wired up — or skip
              it and bring your own container/agent, drive an interactive run, or run a plain command.
              You can connect anytime later.
            </p>
          </div>
        </div>
        <p className="flex items-start gap-1.5 text-xs leading-relaxed text-muted-foreground">
          <Lock className="mt-0.5 size-3.5 shrink-0" />
          However you connect, your key is injected per run at the proxy and is never stored in the
          sandbox (the few exceptions are labeled &quot;resident&quot;).
        </p>
      </div>

      {/* ── Level 1: choose your harness ──────────────────────────────────── */}
      <div className="space-y-2.5">
        <div className="text-xs font-semibold uppercase tracking-wide text-muted-foreground">
          Choose your agent harness
        </div>
        <div className="grid gap-2.5 sm:grid-cols-3">
          <OptionCard
            selected={harness === "claude"}
            onClick={() => setHarness("claude")}
            title={
              <span className="flex items-center justify-between gap-2">
                Claude Code
                <Chip tone={anthropicConnected ? "success" : "neutral"} className="uppercase tracking-wide">
                  {anthropicConnected ? "Connected" : "Not configured"}
                </Chip>
              </span>
            }
            hint="Anthropic (Claude)"
          />
          <OptionCard
            selected={harness === "codex"}
            onClick={() => setHarness("codex")}
            title={
              <span className="flex items-center justify-between gap-2">
                Codex
                <Chip tone={openaiConnected ? "success" : "neutral"} className="uppercase tracking-wide">
                  {openaiConnected ? "Connected" : "Not configured"}
                </Chip>
              </span>
            }
            hint="OpenAI"
          />
          <OptionCard
            selected={harness === "none"}
            onClick={() => setHarness("none")}
            title="No model / bring your own"
            hint="Run without a model — your own container, an interactive run, or a plain command"
          />
        </div>
      </div>

      {/* ── Level 2: connect the chosen harness ───────────────────────────── */}
      {harness === "claude" && (
        <div className="space-y-3">
          <div className="text-xs font-semibold uppercase tracking-wide text-muted-foreground">
            Connect Claude Code
          </div>
          {sealedControlPlane && !claudeSubDetected && !managedCred && (
            <p className="text-xs leading-relaxed text-muted-foreground">
              wardynd runs sealed in a container and can&apos;t see your host&apos;s{" "}
              <code className="rounded bg-background/70 px-1 py-0.5 text-xs">~/.claude</code> login, so{" "}
              <span className="font-medium">Set up Claude subscription</span> opens a one-time login in
              a sandbox (no local install) and injects the token proxy-side. Prefer your host login?
              Re-run setup in host mode (
              <code className="rounded bg-background/70 px-1 py-0.5 font-mono text-xs text-foreground">
                WARDYN_SETUP_MODE=local make setup
              </code>
              ).
            </p>
          )}
          <ul className="space-y-2.5">
            {[
              claudeSubDetected && subRow,
              managedCred && managedRow,
              anthropic && anthropicKeyRow,
              (bedrockConfigured || bedrockOpen) && bedrockRow,
            ].filter(Boolean)}
          </ul>
          <SetupOptionRow options={claudeOptions} />
          {/* Host mode only: wardynd CAN see a resident ~/.claude, so offer that
              route as a secondary alternative to the sandbox login above. On a
              sealed control plane it could never satisfy this step, so it's hidden. */}
          {!sealedControlPlane && !claudeSubDetected && !managedCred && (
            <p className="text-xs text-muted-foreground">
              Already logged in with the Claude CLI on this host?{" "}
              <Button
                size="sm"
                variant="link"
                className="h-auto p-0 text-xs"
                onClick={() => onSetup(PROVIDER_GUIDES.claude)}
              >
                {claudeInstalled ? "Use that login instead" : "Install the Claude CLI"}
              </Button>
            </p>
          )}
          {loginOpen && (
            <HarnessLoginPane
              provider="anthropic"
              onDone={() => {
                setLoginOpen(false);
                onRecheck();
              }}
              onCancel={() => setLoginOpen(false)}
            />
          )}
        </div>
      )}

      {harness === "codex" && (
        <div className="space-y-3">
          <div className="text-xs font-semibold uppercase tracking-wide text-muted-foreground">
            Connect Codex
          </div>
          <ul className="space-y-2.5">
            {[openai && openaiKeyRow, codexDetected && codexRow].filter(Boolean)}
          </ul>
          <SetupOptionRow options={codexOptions} />
          <p className="text-xs leading-relaxed text-muted-foreground">
            Codex connects with an API key — there&apos;s no container login.
          </p>
        </div>
      )}

      {harness === "none" && (
        <div className="flex items-start gap-2.5 rounded-xl border border-border bg-card p-3.5">
          <Lock className="mt-0.5 size-4 shrink-0 text-muted-foreground" />
          <div className="min-w-0 flex-1 space-y-2 text-xs leading-relaxed text-muted-foreground">
            <p>
              You&apos;ll bring your own container or agent, drive an interactive run, or run a plain
              command — nothing to connect here.
            </p>
            {onSkip && !skipped && !readiness.llmReady && (
              <Button size="sm" variant="outline" onClick={onSkip}>
                Skip — run without a model
              </Button>
            )}
            {skipped && (
              <p className="text-muted-foreground">
                Model skipped — pick Claude Code or Codex above anytime to connect one.
              </p>
            )}
          </div>
        </div>
      )}

      <div className="flex items-center justify-between gap-3">
        {onSkip && !skipped && harness !== "none" && !readiness.llmReady ? (
          <Button size="sm" variant="link" onClick={onSkip} className="px-0 text-muted-foreground">
            Skip — run without a model
          </Button>
        ) : (
          <span />
        )}
        <Button size="sm" variant="link" onClick={onRecheck} disabled={rechecking}>
          {rechecking ? "Refreshing…" : "Refresh detection"}
        </Button>
      </div>
    </div>
  );
}
