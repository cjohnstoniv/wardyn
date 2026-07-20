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
import { Info, KeyRound, Loader2, Plus } from "lucide-react";
import type { SetupStatus } from "../../../lib/types";
import { harnessAuth as api } from "../../../lib/api/harness-auth";
import { Button } from "../../ui/button";
import { Chip } from "../../wardyn/primitives";
import { StatusChip } from "../../wardyn/status-chip";
import type { StatusKind } from "../../wardyn/copy";
import { BTN } from "../../wardyn/copy";
import { PROVIDER_GUIDES, type SetupGuide } from "./setup-guide";
import { HarnessLoginPane } from "./harness-login-pane";
import type { Readiness } from "../onboarding/intro";

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
    resident (static/SSO)
  </Chip>
);

// SetupOption is one not-yet-configured way to connect a provider family — shown
// as a compact "add" button rather than a full row, so the family surfaces only
// what's DETECTED prominently while keeping every setup path one click away.
type SetupOption = { key: string; label: string; onClick: () => void; icon?: React.ReactNode };

// ProviderFamily groups one model-provider family (Claude/Anthropic or OpenAI/
// Codex): it renders the DETECTED mechanisms as full rows and collapses the rest
// into a contextual set-up affordance, instead of a flat list of every credential
// type. Detection is unchanged (SetupStatus) — this is purely how the wizard
// organizes the options (owner ask). A family with nothing detected leads with
// "Set up:"; a connected family offers "Add another way:".
function ProviderFamily({
  title,
  connected,
  rows,
  options,
}: {
  title: string;
  connected: boolean;
  rows: React.ReactNode[];
  options: SetupOption[];
}) {
  const shown = rows.filter(Boolean);
  return (
    <section className="rounded-xl border border-border bg-card/40 p-3.5">
      <div className="mb-2.5 flex items-center justify-between gap-2">
        <h3 className="text-sm font-semibold text-foreground">{title}</h3>
        <Chip tone={connected ? "success" : "neutral"} className="uppercase tracking-wide">
          {connected ? "Connected" : "Not configured"}
        </Chip>
      </div>
      {shown.length > 0 && <ul className="space-y-2.5">{shown}</ul>}
      {options.length > 0 && (
        <div className={`flex flex-wrap items-center gap-1.5 ${shown.length > 0 ? "mt-2.5" : ""}`}>
          <span className="text-xs text-muted-foreground">{shown.length > 0 ? "Add another way:" : "Set up:"}</span>
          {options.map((o) => (
            <Button key={o.key} size="sm" variant="outline" onClick={o.onClick}>
              {o.icon ?? <Plus className="size-3.5" />} {o.label}
            </Button>
          ))}
        </div>
      )}
    </section>
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
  // Guidance for the most common first-run snag: a personal machine running the
  // sealed (compose/team) control plane, which can't see the host's Claude login —
  // so this step reads "not connected" even when the operator IS logged in. Only
  // shown when the model is genuinely undetected AND we're blind-in-compose on a
  // local box (host_like === false + local auth); host mode never sees it.
  const suggestHostMode =
    !readiness.llmReady && status.deployment?.host_like === false && status.auth.mode === "local";
  // The same deployment fact as suggestHostMode, but WITHOUT the !llmReady gate: a
  // sealed (compose/team) wardynd can never see a host ~/.claude login, whether or
  // not the model is connected yet. Offering "Install / Log in to Claude CLI" here
  // dangles a path that can NEVER satisfy this step — claudeSubDetected is wired to
  // wardynd's own view, which is blind to the host by construction.
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

  const bedrockRow = (
    <React.Fragment key="bedrock">
      <AccessRow
        status={rowStatus(bedrockReady ? "ready" : "todo", rechecking)}
        label="AWS Bedrock (Claude Code)"
        residency={BEDROCK_RESIDENT_CHIP}
        detail={
          <>
            {bedrockDetail}
            {(bedrock.region || bedrock.model) && (
              <span className="mt-0.5 block font-mono">
                {bedrock.region || "region unset"} · {bedrock.model || "model unset"}
              </span>
            )}
            {bedrockConfigured && !bedrockReady && (!bedrock.region || !bedrock.model) && (
              // Region/model have no write API (boot-time config) — say how to set
              // them on the host instead of leaving a dead "needs-creds" end.
              <span className="mt-0.5 block">
                Region/model are wardynd boot-time config: set WARDYN_BEDROCK_REGION /
                WARDYN_BEDROCK_MODEL (or -bedrock-region / -bedrock-model), restart wardynd, then
                Re-check.
              </span>
            )}
          </>
        }
        action={
          // Buttons ordered by the precedence resolveBedrockAuth actually enforces
          // (internal/api/runs_bedrock.go): bearer API key > ~/.aws mount > static
          // access/secret (+ optional session token). The backend already reads all
          // of these names; the UI just never offered the bearer or session token.
          <div className="flex flex-wrap gap-1.5">
            <Button size="sm" variant="outline" onClick={() => onAddSecret("bedrock-api-key")}>
              <KeyRound className="size-3.5" /> Bedrock API key
            </Button>
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
        }
      />
      <p className="pl-1 text-xs leading-relaxed text-muted-foreground">
        The Bedrock API key (bearer) is preferred — the proxy injects it and it never becomes
        resident. Static access/secret keys DO become resident in sandboxes that use Bedrock; for
        SSO/STS temporary credentials add a session token too (it expires with your SSO session).
        An SSO ~/.aws mount auto-rotates and is safer than pasted keys.
      </p>
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

  return (
    <div className="space-y-5">
      <p className="text-sm leading-relaxed text-muted-foreground">
        {readiness.llmReady
          ? `One connected path is enough — you're already covered by ${readiness.llmLabel || "a connected model"}.`
          : "Optional. A model/harness is only needed to run an agent under Wardyn's own harness, or to enable the AI Run Composer. Skip it if you'll bring your own container/agent, or drive an interactive run yourself. Connect one below with a stored API key (proxy-injected), a Claude subscription, or Bedrock."}
      </p>

      {/* Explicit skip for the optional step — a deliberate "no model" decision,
          not an unfinished gap. A real connected model supersedes it. */}
      {!readiness.llmReady && onSkip && (
        <div className="flex flex-wrap items-center justify-between gap-3 rounded-lg border border-border bg-muted/40 p-3">
          <p className="text-xs leading-relaxed text-muted-foreground">
            {skipped
              ? "Model skipped — runs will bring their own agent/container (or you drive them). Connect a provider below anytime to change that."
              : "Don't need a model here? Skip — you can bring your own container/agent or connect one later."}
          </p>
          {!skipped && (
            <Button size="sm" variant="outline" onClick={onSkip}>
              Skip — run without a model
            </Button>
          )}
        </div>
      )}

      {suggestHostMode && !managedCred && (
        <div className="flex items-start gap-2.5 rounded-lg border border-border bg-muted/40 p-3">
          <Info className="mt-0.5 size-4 shrink-0 text-primary" />
          <div className="min-w-0 flex-1 space-y-1.5 text-xs leading-relaxed">
            <p className="text-foreground">
              You&apos;re on the containerized control plane, so wardynd runs sealed in a container that can&apos;t
              see your host&apos;s{" "}
              <code className="rounded bg-background/70 px-1 py-0.5 text-xs">~/.claude</code> login — which is why it
              reads &quot;not connected&quot; even if you are logged in on the host. The supported fix here is{" "}
              <span className="font-medium">container login</span>: Wardyn opens a sandbox, runs{" "}
              <code className="rounded bg-background/70 px-1 py-0.5 text-xs">claude setup-token</code> for you, and
              captures the token — injected proxy-side into every run.
            </p>
            <div>
              <Button size="sm" onClick={() => setLoginOpen(true)}>
                <KeyRound className="size-3.5" /> Connect via container login
              </Button>
            </div>
            <p className="text-muted-foreground">
              Prefer to use your host&apos;s own Claude login? Re-run setup in host mode:{" "}
              <code className="rounded bg-background/70 px-1.5 py-0.5 font-mono text-xs text-foreground">
                WARDYN_SETUP_MODE=local make setup
              </code>{" "}
              — wardynd then runs on your machine and reads <code className="rounded bg-background/70 px-1 py-0.5 text-xs">~/.claude</code> directly (note: host mode&apos;s workspace Verify/Record don&apos;t complete under WSL2 NAT).
            </p>
          </div>
        </div>
      )}

      <div className="space-y-4">
        <ProviderFamily
          title="Claude / Anthropic"
          connected={anthropicConnected}
          rows={[
            claudeSubDetected && subRow,
            managedCred && managedRow,
            anthropic && anthropicKeyRow,
            bedrockConfigured && bedrockRow,
          ]}
          options={[
            // The resident-CLI route. Hidden in two cases, for two different reasons:
            //   · managedCred — the container login ALREADY connected this same Claude
            //     subscription; a second route to the identical credential reads as a
            //     missing setup step next to a CONNECTED badge.
            //   · sealedControlPlane — wardynd is containerized and cannot see a host
            //     login at all, so this option could never flip the step to connected.
            //     (Host mode is offered instead, in the suggestHostMode banner above.)
            !claudeSubDetected &&
              !managedCred &&
              !sealedControlPlane && {
                key: "sub",
                label: claudeInstalled ? "Log in to Claude CLI" : "Install Claude CLI",
                onClick: () => onSetup(PROVIDER_GUIDES.claude),
              },
            // Container login: works with no local Claude install — the compose-mode
            // path. Shown when there is no resident login and no managed token yet.
            !claudeSubDetected &&
              !managedCred && {
                key: "container-login",
                label: "Connect via container login (no local install)",
                icon: <KeyRound className="size-3.5" />,
                onClick: () => setLoginOpen(true),
              },
            !anthropic && {
              key: "akey",
              label: "Add Anthropic API key",
              icon: <KeyRound className="size-3.5" />,
              onClick: () => onAddSecret("anthropic-api-key"),
            },
            !bedrockConfigured && {
              key: "bedrock",
              // Lead with the preferred, never-resident bearer path (the full row
              // above exposes the static-key and session-token alternatives).
              label: "Set up AWS Bedrock (API key)",
              icon: <KeyRound className="size-3.5" />,
              onClick: () => onAddSecret("bedrock-api-key"),
            },
          ].filter(Boolean) as SetupOption[]}
        />
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
        <ProviderFamily
          title="OpenAI / Codex"
          connected={openaiConnected}
          rows={[openai && openaiKeyRow, codexDetected && codexRow]}
          options={[
            !openai && {
              key: "okey",
              label: "Add OpenAI API key",
              icon: <KeyRound className="size-3.5" />,
              onClick: () => onAddSecret("openai-api-key"),
            },
            // Same sealed-control-plane rule as the Claude CLI option above, and
            // stricter: Codex has no container-login path at all (agentHarnessLogin
            // implements claude-code only), so on a sealed wardynd a resident Codex
            // login can never be detected AND cannot be captured — the API key is the
            // only reachable route. Offering the CLI here is a dead end.
            !codexDetected &&
              !sealedControlPlane && {
                key: "codex",
                label: codexInstalled ? "Log in to Codex CLI" : "Install Codex CLI",
                onClick: () => onSetup(PROVIDER_GUIDES.codex),
              },
          ].filter(Boolean) as SetupOption[]}
        />
        <div className="flex justify-end">
          <Button size="sm" variant="link" onClick={onRecheck} disabled={rechecking}>
            {rechecking ? "Refreshing…" : "Refresh detection"}
          </Button>
        </div>
      </div>
    </div>
  );
}
