/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Detected-status sections for the Getting-started funnel: the barrier list
// (Fence/Wall/Vault, guidance-led per B9/D5), the LLM-access list (API keys +
// resident CLI subscriptions, B7), and the composer-backends list. Each reads
// from the SetupStatus the daemon already reports and offers the right action:
//   - API keys  -> one-click AddSecretDialog (setSecret)
//   - CLI logins / runtimes -> a SetupGuide the operator runs on the host, or an
//     inline "Show setup command" (B7/D9)
import * as React from "react";
import { Check, Copy, Cpu, Info, KeyRound, Plus } from "lucide-react";
import type { ConfinementClass, SetupStatus } from "../../../lib/types";
import { useCopyToClipboard } from "../../../lib/use-copy-to-clipboard";
import { Button } from "../../ui/button";
import { Chip, ConfinementChip } from "../../wardyn/primitives";
import { BarrierStrengthStrip } from "../../wardyn/barrier-strength-strip";
import { StatusChip } from "../../wardyn/status-chip";
import type { StatusKind } from "../../wardyn/copy";
import { RESIDUAL_PREFIX, BTN } from "../../wardyn/copy";
import { CC_META, CC_ORDER, CONFINEMENT_CONSTANT_NOTE } from "../../wardyn/cc-meta";
import { TierMatrixDialog } from "../../wardyn/tier-matrix";
import { Mono } from "../../wardyn/code-block";
import { RecheckFeedback } from "../onboarding/intro";
import { PROVIDER_GUIDES, TIER_GUIDES, type SetupGuide } from "./setup-guide";

type RowState = "ready" | "todo" | "incompatible";
type Provider = SetupStatus["providers"][number];

function rowStatus(state: RowState, rechecking: boolean): StatusKind {
  if (rechecking && state !== "ready") return "checking";
  if (state === "ready") return "ready";
  if (state === "incompatible") return "incompatible";
  return "needs-setup";
}

// ---------------------------------------------------------------------------
// Confinement barriers: Fence / Wall / Vault (guidance-led)
// ---------------------------------------------------------------------------

// "Pick this when…" guidance leads each card (B9/D5); the runc/gVisor/KVM
// mechanism is demoted to the ConfinementChip + info-icon tooltips.
const PICK_WHEN: Record<ConfinementClass, string> = {
  CC1: "Trying Wardyn out, or the code is your own — quickest start, works on any host.",
  CC2: "Real work on real repos — closes the Fence's holes so the agent never touches your kernel.",
  CC3: "Untrusted code or secrets nearby — the strongest box Wardyn can build.",
};

// The concrete "still not detected" line shown by RecheckFeedback (B5) once a
// re-check has run and the tier is still missing.
const NOT_DETECTED: Partial<Record<ConfinementClass, string>> = {
  CC2: "Still not detected — gVisor's runsc runtime isn't listed in `docker info` runtimes yet.",
  CC3: "Still not detected — no kata runtime in `docker info` yet.",
};

function InlineCommand({ command, docNote }: { command: string; docNote?: string }) {
  const { copied, copyAsync } = useCopyToClipboard(1400);
  return (
    <div className="mt-3 space-y-2">
      <div className="flex items-center gap-2.5 rounded-lg border border-border bg-background px-3 py-2">
        <span className="font-mono text-[12.5px] text-muted-foreground">$</span>
        <Mono className="min-w-0 flex-1 truncate text-[12.5px] text-foreground">{command}</Mono>
        <Button
          size="sm"
          variant="outline"
          onClick={() => void copyAsync(command)}
          aria-label="Copy command"
        >
          {copied ? <Check className="size-3.5 text-success" /> : <Copy className="size-3.5" />}
          {copied ? "Copied" : "Copy"}
        </Button>
      </div>
      {docNote && <p className="text-xs leading-snug text-muted-foreground">{docNote}</p>}
    </div>
  );
}

function BarrierCard({
  cc,
  state,
  recommended,
  rechecking,
  recheckedOnce,
  selected,
  onSelect,
  incompatibleReason,
  substrate,
}: {
  cc: ConfinementClass;
  state: RowState;
  recommended: boolean;
  rechecking: boolean;
  recheckedOnce: boolean;
  // Selection is the default-barrier picker: exactly one ready card is selected
  // (the resolved default). Todo/incompatible cards are never selectable.
  selected: boolean;
  onSelect: () => void;
  incompatibleReason?: string;
  // The concrete substrate label backing this class on THIS host ("oci/<runtime>"),
  // from runner.confinement_substrates. Present only for live classes, so it
  // naturally shows on ready cards only. Undefined => nothing to show.
  substrate?: string;
}) {
  const [showCmd, setShowCmd] = React.useState(false);
  const m = CC_META[cc];
  const guide = TIER_GUIDES[cc];
  // The concrete "why it can never run here" sentence, supplied by RunnerTiers
  // (which knows whether the /dev/kvm fact was PROBED or merely inferred) —
  // only ever set alongside state === "incompatible".
  const notDetected =
    recheckedOnce && !rechecking && state === "todo" ? NOT_DETECTED[cc] : undefined;

  const body = (
    <>
      <div className="flex flex-wrap items-center gap-2.5">
        <ConfinementChip value={cc} />
        <BarrierStrengthStrip tier={cc} />
        {recommended && (
          <Chip tone="primary" className="uppercase tracking-wide">
            Recommended
          </Chip>
        )}
        <span
          className="inline-flex text-muted-foreground/70"
          title={`${m.label} — ${m.mechanism}`}
        >
          <Info className="size-3.5" />
        </span>
        <span className="ml-auto">
          <StatusChip status={rowStatus(state, rechecking)} reason={incompatibleReason} />
        </span>
        {/* Selection badge — mirrors step-confinement.tsx's picker check. */}
        {selected && (
          <span className="flex size-4 shrink-0 items-center justify-center rounded-full bg-primary text-primary-foreground">
            <Check className="size-2.5" />
          </span>
        )}
        {state === "todo" && guide && (
          <Button size="sm" variant="outline" onClick={() => setShowCmd((v) => !v)}>
            {showCmd ? "Hide setup command" : BTN.showSetupCommand}
          </Button>
        )}
      </div>

      <p className="mt-2 text-[13px] leading-relaxed text-foreground/90">{PICK_WHEN[cc]}</p>
      <p className="mt-1 text-xs leading-relaxed text-muted-foreground">
        <span className="font-semibold text-foreground/80">{RESIDUAL_PREFIX}</span> {m.doesntProtect}
      </p>
      {/* Substrate provenance (E2): the concrete runtime this tier is running as
          here — only set for live classes, so it appears on ready cards only. */}
      {substrate && (
        <p className="mt-1 text-xs leading-relaxed text-muted-foreground">Running here as {substrate}</p>
      )}

      {incompatibleReason && (
        <p className="mt-1.5 flex items-start gap-1.5 text-xs leading-snug text-muted-foreground">
          <Cpu className="mt-0.5 size-3 shrink-0" />
          {incompatibleReason}
        </p>
      )}

      {state === "todo" && guide && showCmd && (
        <InlineCommand command={guide.command} docNote={guide.docNote} />
      )}

      <RecheckFeedback rechecking={rechecking && state !== "ready"} message={notDetected} />
    </>
  );

  // Ready cards carry no inner controls (Show-setup-command is todo-only), so the
  // whole card becomes the selectable <button> for the default-barrier picker. The
  // ring/Check is the SOLE primary treatment — Recommended keeps only its Chip.
  // Todo/incompatible cards stay plain <div>s: they own their Show-setup-command /
  // Copy buttons and must not nest inside a button.
  if (state === "ready") {
    return (
      <button
        type="button"
        aria-pressed={selected}
        onClick={onSelect}
        className={
          "w-full rounded-xl border p-4 text-left transition-colors " +
          (selected
            ? "border-primary bg-primary/5 ring-2 ring-primary/40"
            : "border-border bg-card hover:border-border-strong")
        }
      >
        {body}
      </button>
    );
  }
  return <div className="rounded-xl border border-border bg-card p-4">{body}</div>;
}

export function RunnerTiers({
  runner,
  platform,
  rechecking,
  recheckedOnce,
  selected,
  onSelect,
}: {
  runner: SetupStatus["runner"];
  platform: SetupStatus["platform"];
  rechecking: boolean;
  recheckedOnce: boolean;
  // The operator's current default-barrier pick (owned by SetupScreen) and the
  // click handler that persists it. Always an available class, so exactly one
  // ready card shows aria-pressed.
  selected: ConfinementClass;
  onSelect: (cc: ConfinementClass) => void;
}) {
  // "Compare all three" opens the pricing-table matrix (E1) — detail on demand,
  // never the default view. Declared before the no-runner early return so hook
  // order never varies across renders of the same instance.
  const [showMatrix, setShowMatrix] = React.useState(false);
  const classes = runner?.confinement_classes ?? [];
  if (classes.length === 0) {
    return (
      <div className="rounded-xl border border-danger/25 bg-danger-subtle/40 p-4">
        <p className="text-sm font-medium text-foreground">No sandbox runner — runs can&apos;t launch.</p>
        <p className="mt-1 text-xs leading-snug text-muted-foreground">
          <span className="font-semibold text-foreground">Fix:</span> start wardynd with{" "}
          <Mono>-runner docker</Mono> (built with -tags docker).
        </p>
      </div>
    );
  }
  const available = new Set(classes);
  // Incompatible vs needs-setup is a HARDWARE fact, not a platform guess: the
  // backend probes /dev/kvm (platform.kvm). Only a genuinely KVM-less host marks
  // Vault incompatible; a missing runtime on capable hardware is "Needs setup"
  // with the install command. (Old daemons without the kvm field fall back to
  // the previous WSL/macOS heuristic rather than overclaiming compatibility.)
  const kvmProbed = typeof platform.kvm === "boolean";
  const kvm = platform.kvm ?? !(platform.wsl || /darwin|mac/i.test(platform.os));
  const tierState = (cc: ConfinementClass): RowState => {
    if (available.has(cc)) return "ready";
    if (cc === "CC3" && !kvm) return "incompatible";
    return "todo";
  };
  // Only claim the /dev/kvm hardware fact when the backend actually PROBED it;
  // on an old daemon the WSL/macOS inference gets honestly softer wording.
  const vaultIncompatibleReason = kvmProbed
    ? "Vault needs KVM virtualization and this host doesn't expose /dev/kvm — a hardware/VM limit no install can fix. (On WSL2, enable nested virtualization; on a laptop/desktop, enable virtualization in firmware; a containerized wardynd needs /dev/kvm mapped in.)"
    : "Vault likely can't run here — WSL/macOS hosts usually can't register a Kata runtime, and this daemon predates the /dev/kvm probe that would say for sure. Upgrade wardynd for a definitive answer.";
  // Recommend the STRONGEST tier this host is COMPATIBLE with — never demote the
  // recommendation to a weaker tier just because the stronger one isn't installed
  // yet (its card says "Needs setup" + shows the command), and never recommend a
  // hardware-impossible one. The SELECTION ring (the actual default used for new
  // runs) stays on the strongest READY tier until the recommended one is set up.
  const compatible = CC_ORDER.filter((cc) => tierState(cc) !== "incompatible");
  const recommendedTier = compatible[compatible.length - 1];
  return (
    <div className="space-y-2.5">
      {CC_ORDER.map((cc) => (
        <BarrierCard
          key={cc}
          cc={cc}
          state={tierState(cc)}
          recommended={cc === recommendedTier}
          rechecking={rechecking}
          recheckedOnce={recheckedOnce}
          selected={cc === selected}
          onSelect={() => onSelect(cc)}
          incompatibleReason={tierState(cc) === "incompatible" ? vaultIncompatibleReason : undefined}
          substrate={runner?.confinement_substrates?.[cc]}
        />
      ))}
      <p className="pt-0.5 text-xs leading-relaxed text-muted-foreground">
        Your pick is saved in this browser as the default barrier for new runs — changeable any time, or
        per run.
      </p>
      <p className="flex items-start gap-2 pt-1 text-xs leading-relaxed text-muted-foreground">
        <Info className="mt-0.5 size-3.5 shrink-0 text-primary" />
        {CONFINEMENT_CONSTANT_NOTE}
      </p>
      <Button size="sm" variant="link" className="h-auto p-0 text-xs" onClick={() => setShowMatrix(true)}>
        Compare all three →
      </Button>
      <TierMatrixDialog open={showMatrix} onOpenChange={setShowMatrix} />
    </div>
  );
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
        <div className="text-[13.5px] font-semibold text-foreground">{label}</div>
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
        <h3 className="text-[13.5px] font-semibold text-foreground">{title}</h3>
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

// ---------------------------------------------------------------------------
// Composer backends: the LLM backends the AI Run Composer proposes runs with
// ---------------------------------------------------------------------------
export function ComposerBackends({
  status,
  onAddSecret,
}: {
  status: SetupStatus;
  onAddSecret: (name: string) => void;
}) {
  const backends = status.composer.backends;
  if (backends.length === 0) {
    return (
      <p className="text-xs text-muted-foreground">
        No composer backends configured — set <Mono>-composer-config</Mono> to enable natural-language run
        composition.
      </p>
    );
  }
  return (
    <ul className="space-y-1.5">
      {backends.map((b) => {
        const needsKey = b.needs_key && !b.key_resolved;
        return (
          <li
            key={b.name}
            className="flex items-center justify-between gap-2 rounded-md border border-border px-2.5 py-1.5 text-xs"
          >
            <span className="min-w-0 truncate font-mono">
              {b.name} · {b.provider}/{b.model}
              {/* Transport/auth provenance (E2), muted; absent fields render nothing. */}
              {b.transport && <span className="text-muted-foreground"> · {b.transport}</span>}
              {b.auth && <span className="text-muted-foreground"> · {b.auth}</span>}
            </span>
            <div className="flex shrink-0 items-center gap-1.5">
              <StatusChip status={b.key_resolved ? "ready" : "needs-setup"} />
              {needsKey && b.key_secret && (
                <Button size="sm" variant="ghost" onClick={() => onAddSecret(b.key_secret!)}>
                  <Plus className="size-3" /> key
                </Button>
              )}
            </div>
          </li>
        );
      })}
    </ul>
  );
}
