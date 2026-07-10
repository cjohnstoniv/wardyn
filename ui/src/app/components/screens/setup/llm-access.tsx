/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// LLM-access section for the Getting-started funnel: the API-key + resident-CLI
// subscription list (B7), grouped by provider family (Claude/Anthropic, OpenAI/
// Codex). Reads from the SetupStatus the daemon already reports and offers the
// right action:
//   - API keys  -> one-click AddSecretDialog (setSecret)
//   - CLI logins / runtimes -> a SetupGuide the operator runs on the host
// (The barrier picker moved to ./environment-step and the composer-backends list
// was dropped from setup by owner decision; this module is LLM access only.)
import * as React from "react";
import { KeyRound, Plus } from "lucide-react";
import type { SetupStatus } from "../../../lib/types";
import { Button } from "../../ui/button";
import { Chip } from "../../wardyn/primitives";
import { StatusChip } from "../../wardyn/status-chip";
import type { StatusKind } from "../../wardyn/copy";
import { BTN } from "../../wardyn/copy";
import { PROVIDER_GUIDES, type SetupGuide } from "./setup-guide";

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
  action,
}: {
  status: StatusKind;
  reason?: string;
  label: React.ReactNode;
  detail?: React.ReactNode;
  action?: React.ReactNode;
}) {
  return (
    <li className="flex flex-wrap items-center gap-3 rounded-xl border border-border bg-card p-3.5">
      <div className="min-w-[200px] flex-1">
        <div className="text-sm font-semibold text-foreground">{label}</div>
        {detail && <p className="mt-0.5 text-xs leading-snug text-muted-foreground">{detail}</p>}
      </div>
      <StatusChip status={status} reason={reason} />
      {action && <div className="shrink-0">{action}</div>}
    </li>
  );
}

function hasSecret(present: string[], re: RegExp): boolean {
  return present.some((n) => re.test(n));
}

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

export function LlmAccess({
  status,
  onAddSecret,
  onSetup,
  onRecheck,
  rechecking,
}: {
  status: SetupStatus;
  onAddSecret: (name: string) => void;
  onSetup: (g: SetupGuide) => void;
  onRecheck: () => void;
  rechecking: boolean;
}) {
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

  const keyRow = (name: string, label: string, detail: string, configured: boolean) => (
    <AccessRow
      status={rowStatus(configured ? "ready" : "todo", rechecking)}
      label={label}
      detail={detail}
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
  const anthropicConnected = anthropic || claudeSubDetected || bedrockReady;
  const openaiConnected = openai || codexDetected;

  const bedrockRow = (
    <AccessRow
      key="bedrock"
      status={rowStatus(bedrockReady ? "ready" : "todo", rechecking)}
      label="AWS Bedrock (Claude Code)"
      detail={
        <>
          {bedrockDetail}
          {(bedrock.region || bedrock.model) && (
            <span className="mt-0.5 block font-mono">
              {bedrock.region || "region unset"} · {bedrock.model || "model unset"}
            </span>
          )}
        </>
      }
      action={
        <div className="flex flex-wrap gap-1.5">
          <Button size="sm" variant="outline" onClick={() => onAddSecret("aws-access-key-id")}>
            <KeyRound className="size-3.5" /> Access key
          </Button>
          <Button size="sm" variant="outline" onClick={() => onAddSecret("aws-secret-access-key")}>
            <KeyRound className="size-3.5" /> Secret key
          </Button>
        </div>
      }
    />
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
    <div className="space-y-4">
      <ProviderFamily
        title="Claude / Anthropic"
        connected={anthropicConnected}
        rows={[
          claudeSubDetected && subRow,
          anthropic && anthropicKeyRow,
          bedrockConfigured && bedrockRow,
        ]}
        options={[
          !claudeSubDetected && {
            key: "sub",
            label: claudeInstalled ? "Log in to Claude CLI" : "Install Claude CLI",
            onClick: () => onSetup(PROVIDER_GUIDES.claude),
          },
          !anthropic && {
            key: "akey",
            label: "Add Anthropic API key",
            icon: <KeyRound className="size-3.5" />,
            onClick: () => onAddSecret("anthropic-api-key"),
          },
          !bedrockConfigured && {
            key: "bedrock",
            label: "Set up AWS Bedrock",
            icon: <KeyRound className="size-3.5" />,
            onClick: () => onAddSecret("aws-access-key-id"),
          },
        ].filter(Boolean) as SetupOption[]}
      />
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
          !codexDetected && {
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
  );
}
