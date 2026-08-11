/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Add-integration dialog — mockup's AddCategory / AddTypeAI / AddKey / etc.
// TWO categories: AI provider (fully modeled by the approved mock) and SCM
// host, which hands off to the SAME ladder ScmProviderStep already uses
// (AddProviderPanel1/2, exported from that file for exactly this reuse).
// Egress redirection and Host proxy are NOT addable here — they're network
// topology, owned by the Corporate network step, where a redirect has to prove
// itself before the step hands off. That retired this dialog's hand-off to
// setup/step-bodies.tsx's ArtifactRepoStep / HostProxyStep, and with it those
// two step bodies.
import * as React from "react";
import { ChevronDown, KeyRound, ShieldCheck } from "lucide-react";
import { toast } from "sonner";
import {
  AI_TYPES,
  BEDROCK_LANE_META,
  CATEGORY_META,
  RESIDENCY_META,
  SUBSCRIPTION_LANE_META,
  T,
  type AiType,
  type BedrockLane,
  type SubscriptionLane,
} from "../../../lib/integrations";
import { aiResidency, aiRowName, aiServerId, defaultHolder, type IntegrationRow } from "../../../lib/api/integrations";
import { health } from "../../../lib/api/health";
import { setup as setupApi } from "../../../lib/api/setup";
import type { SetupStatus, SiteConfig, WireIntegration } from "../../../lib/types";
import { Button } from "../../ui/button";
import { Checkbox } from "../../ui/checkbox";
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "../../ui/dialog";
import { Field, OptionCard } from "../new-run/step-shell";
import { Mono } from "../../wardyn/code-block";
import { Chip, SectionLabel } from "../../wardyn/primitives";
import { AddSecretDialog } from "../secrets";
import { HarnessLoginPane } from "../setup/harness-login-pane";
import { AddProviderPanel1, AddProviderPanel2, type ProviderOption } from "../setup/scm-provider-step";
import { CapabilityTable } from "./integration-detail";
import { setDefaultFor, type DefaultForMark } from "./actions";
import type { Lane } from "../../../lib/scm-provider";
import { getErrorMessage, relativeTime } from "../../../lib/format";

// Where the search-first Add flow (add-service-dialog.tsx) hands off TO. That
// flow is the ONLY way in here — the old "AI provider or SCM host?" category
// grid is gone, because by the time this dialog opens that question has always
// been answered by the pick itself. Two landings exist for an AI pick:
//
//   - ai_connect: the pick left NO open question (an OpenAI key is an OpenAI
//     key) — land straight on its connect panel.
//   - ai_type, preselected: the pick left a REAL question — "Anthropic" still
//     splits into API key vs Claude subscription, Bedrock still has its four
//     credential lanes, a subscription still chooses managed vs host login.
//     The type panel is where those sub-choices live, so it opens with the
//     picked row selected and the siblings one click away.
export type AddIntegrationTarget =
  | { s: "ai_connect"; type: AiType }
  | { s: "ai_type"; preselect?: AiType }
  | { s: "scm" };

type Step =
  | { s: "scm" }
  | { s: "ai_type"; preselect?: AiType }
  | { s: "ai_connect"; type: AiType; hostCli?: boolean; bedrockLane?: BedrockLane };

export function AddIntegrationDialog({
  open,
  onOpenChange,
  status,
  siteConfig,
  existingAiRows,
  secretNames,
  reload,
  target,
  onBackToSearch,
}: {
  open: boolean;
  onOpenChange: (o: boolean) => void;
  status: SetupStatus;
  siteConfig: SiteConfig;
  existingAiRows: IntegrationRow[];
  /** Every currently-stored secret name — so the credential dialogs this
   *  hands off to (App PEM / PAT / SSH key / API key) can arm their own
   *  overwrite gate on a name collision (SCM-SEAM-4) instead of silently
   *  clobbering an in-use secret. */
  secretNames: string[];
  reload: () => void;
  /** What the search-first flow picked — this dialog always opens ON it. */
  target: AddIntegrationTarget;
  /** Backing out of the first panel returns to the search dialog, so the walk
   *  reads as one flow, not two dialogs trading places. */
  onBackToSearch: () => void;
}) {
  const [step, setStep] = React.useState<Step>(target);
  const [localSiteConfig, setLocalSiteConfig] = React.useState<SiteConfig>(siteConfig);

  React.useEffect(() => {
    if (open) {
      setStep(target);
      setLocalSiteConfig(siteConfig);
    }
    // Only reset when the dialog transitions open — not on every siteConfig
    // prop tick from the parent's own polling, which would stomp an in-flight
    // edit inside the SCM hand-off panels.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [open]);

  if (!open) return null;

  const finish = () => {
    onOpenChange(false);
    reload();
  };
  // V2 discipline (step-bodies.tsx's useSiteConfigStep): re-GET before a save
  // so a copy that went stale while this dialog was open never clobbers it.
  const reloadSiteConfig = async () => setLocalSiteConfig(await health.getSiteConfig());
  const saveSiteConfig = async (next: SiteConfig) => {
    await health.putSiteConfig(next);
    setLocalSiteConfig(next);
  };

  if (step.s === "scm") {
    return (
      <ScmHandoff
        siteConfig={localSiteConfig}
        reloadSiteConfig={reloadSiteConfig}
        saveSiteConfig={saveSiteConfig}
        secretNames={secretNames}
        onBack={onBackToSearch}
        onDone={finish}
      />
    );
  }

  if (step.s === "ai_type") {
    return (
      <AiTypePanel
        status={status}
        initialType={step.preselect}
        onBack={onBackToSearch}
        onContinue={(type, hostCli, bedrockLane) => setStep({ s: "ai_connect", type, hostCli, bedrockLane })}
      />
    );
  }

  return (
    <ConnectReviewPanel
      type={step.type}
      hostCli={step.hostCli}
      bedrockLane={step.bedrockLane}
      existingAiRows={existingAiRows}
      secretNames={secretNames}
      status={status}
      onBack={() => setStep({ s: "ai_type", preselect: step.type })}
      onDone={finish}
    />
  );
}

// ---- SCM hand-off: the existing App/PAT/SSH ladder, not a second one ----

function ScmHandoff({
  siteConfig,
  reloadSiteConfig,
  saveSiteConfig,
  secretNames,
  onBack,
  onDone,
}: {
  siteConfig: SiteConfig;
  reloadSiteConfig: () => Promise<void>;
  saveSiteConfig: (next: SiteConfig) => Promise<void>;
  secretNames: string[];
  onBack: () => void;
  onDone: () => void;
}) {
  React.useEffect(() => {
    void reloadSiteConfig();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  type Panel =
    | { step: "panel1" }
    | { step: "panel2"; kind: ProviderOption["kind"]; host: string; title: string; showHostedNote: boolean };
  const [panel, setPanel] = React.useState<Panel>({ step: "panel1" });
  const [secretDialog, setSecretDialog] = React.useState<{ name: string; host: string; lane: Lane } | null>(null);
  const [saving, setSaving] = React.useState(false);

  const addHost = async (h: string): Promise<boolean> => {
    setSaving(true);
    try {
      const hosts = Array.from(new Set([...(siteConfig.scm_hosts ?? []), h]));
      await saveSiteConfig({ ...siteConfig, scm_hosts: hosts });
      // SCM-SEAM-2: the ONLY disclosure a hosted (GitHub/ADO/GitLab/Bitbucket)
      // pick gets — Panel2's own inline note renders for the generic kind
      // only. Every pick still widens every future run's egress allowlist, so
      // every pick gets told so, even after the fact.
      toast.info(`${h} added to the egress allowlist`, {
        description: "Every future run can now reach it, credential or not — delete the integration to revoke that.",
      });
      return true;
    } catch (e) {
      // SCM-SEAM-3: this used to have no catch at all — a rejected write left
      // the spinner stopping with no toast, no inline error, and the host
      // never registered. Matches the retired predecessor step's own
      // addHost (scm-provider-step.tsx, pre-7f8d091).
      toast.error("Failed to add the SCM host", { description: getErrorMessage(e) });
      return false;
    } finally {
      setSaving(false);
    }
  };

  if (panel.step === "panel1") {
    return (
      <AddProviderPanel1
        onCancel={onBack}
        onContinue={(opt, host) => setPanel({ step: "panel2", kind: opt.kind, host, title: opt.title, showHostedNote: !opt.host })}
      />
    );
  }

  return (
    <>
      <AddProviderPanel2
        kind={panel.kind}
        host={panel.host}
        title={panel.title}
        showHostedNote={panel.showHostedNote}
        saving={saving}
        siteConfigLoaded
        onBack={() => setPanel({ step: "panel1" })}
        onClose={onBack}
        onDone={async () => {
          if (await addHost(panel.host)) onDone();
        }}
        onOpenSecret={(name, host, lane) => setSecretDialog({ name, host, lane })}
        onRecheck={() => {}}
      />
      <AddSecretDialog
        open={!!secretDialog}
        onOpenChange={(o) => !o && setSecretDialog(null)}
        initialName={secretDialog?.name ?? ""}
        lockName
        existingNames={secretNames}
        host={secretDialog?.host}
        lane={secretDialog?.lane}
        onSaved={() => setSecretDialog(null)}
      />
    </>
  );
}

// ---- AI provider: Panel 2 (type pick) ----

function AiTypePanel({
  status,
  initialType,
  onBack,
  onContinue,
}: {
  status: SetupStatus;
  /** The search pick this panel opens on; its siblings stay one click away. */
  initialType?: AiType;
  onBack: () => void;
  onContinue: (type: AiType, hostCli?: boolean, bedrockLane?: BedrockLane) => void;
}) {
  const [selected, setSelected] = React.useState<AiType>(initialType ?? "anthropic_api_key");
  const [subLane, setSubLane] = React.useState<SubscriptionLane>("managed");
  const [advancedOpen, setAdvancedOpen] = React.useState(false);
  const [bedrockLane, setBedrockLane] = React.useState<BedrockLane>("bearer");
  // Sealed control plane (wardynd itself runs in a container): the host-CLI
  // subscription lane can never be satisfied here — the same rule the funnel
  // applied before it folded into Integrations.
  const sealed = status.deployment?.host_like === false;

  const continueClick = () => {
    if (selected === "anthropic_subscription") onContinue(selected, subLane === "resident_host");
    else if (selected === "bedrock") onContinue(selected, undefined, bedrockLane);
    else onContinue(selected);
  };

  return (
    <Dialog open onOpenChange={(o) => !o && onBack()}>
      <DialogContent className="scroll-thin sm:max-w-xl" style={{ maxHeight: "calc(100vh - 96px)", overflowY: "auto" }}>
        <DialogHeader>
          <DialogTitle>Add integration — {CATEGORY_META.ai_provider.title}</DialogTitle>
          <DialogDescription>
            Multiple integrations of the same type can coexist — a personal login and a team key are two
            rows.
          </DialogDescription>
        </DialogHeader>

        <div className="space-y-2">
          <OptionCard
            selected={selected === "anthropic_api_key"}
            onClick={() => setSelected("anthropic_api_key")}
            title={AI_TYPES.anthropic_api_key.title}
            hint={AI_TYPES.anthropic_api_key.desc}
          />

          <OptionCard
            selected={selected === "anthropic_subscription"}
            onClick={() => setSelected("anthropic_subscription")}
            title={AI_TYPES.anthropic_subscription.title}
            hint={selected === "anthropic_subscription" ? undefined : AI_TYPES.anthropic_subscription.desc}
          />
          {selected === "anthropic_subscription" && (
            <div className="ml-3 space-y-2 border-l border-border pl-3">
              <OptionCard
                selected={subLane === "managed"}
                onClick={() => setSubLane("managed")}
                title={
                  <span className="flex items-center gap-2">
                    {SUBSCRIPTION_LANE_META.managed.title}
                    <Chip tone="primary" className="uppercase tracking-wide">
                      Recommended
                    </Chip>
                  </span>
                }
                hint={
                  <>
                    “{T.MANAGED_LINE}” <span className="block">{T.X_SUB_DIRECT}</span>
                  </>
                }
              />
              {advancedOpen ? (
                <div className="space-y-2">
                  <OptionCard
                    selected={subLane === "resident_host"}
                    onClick={() => setSubLane("resident_host")}
                    title={SUBSCRIPTION_LANE_META.resident_host.title}
                    hint={
                      <>
                        “{T.HOSTCLI_LINE}” <span className="block">{T.X_SUB_DIRECT}</span>
                      </>
                    }
                  />
                  {sealed && (
                    <div className="rounded-lg border border-warning/30 bg-warning-subtle px-3 py-2">
                      <p className="text-[0.6875rem] leading-snug text-warning">{T.SEALED_NOTE}</p>
                    </div>
                  )}
                </div>
              ) : (
                <button
                  type="button"
                  onClick={() => setAdvancedOpen(true)}
                  className="flex items-center gap-1.5 text-xs text-muted-foreground hover:text-foreground"
                >
                  <ChevronDown className="size-3.5" /> Advanced
                </button>
              )}
            </div>
          )}

          <OptionCard
            selected={selected === "bedrock"}
            onClick={() => setSelected("bedrock")}
            title={AI_TYPES.bedrock.title}
            hint={
              selected === "bedrock"
                ? "One integration, four credential lanes — pick how the AWS chain is fed."
                : AI_TYPES.bedrock.desc
            }
          />
          {selected === "bedrock" && (
            <div className="ml-3 space-y-1.5 border-l border-border pl-3">
              {(["bearer", "sso", "aws_dir", "static"] as const).map((lane) => {
                const meta = BEDROCK_LANE_META[lane];
                const res = RESIDENCY_META[meta.residency];
                return (
                  <button
                    key={lane}
                    type="button"
                    onClick={() => setBedrockLane(lane)}
                    className={`flex w-full items-center gap-2.5 rounded-lg border px-3 py-2 text-left transition-colors ${
                      bedrockLane === lane ? "border-primary bg-primary/10" : "border-border hover:border-border-strong"
                    }`}
                  >
                    <span className="text-sm text-foreground">
                      {meta.title}
                      {meta.extra && <span className="text-muted-foreground"> · {meta.extra}</span>}
                    </span>
                    <span className="ml-auto">
                      <Chip tone={res.tone} className="text-[0.6875rem]">
                        {res.label}
                      </Chip>
                    </span>
                  </button>
                );
              })}
              <p className="text-[0.6875rem] text-muted-foreground">
                {T.LANE_SWITCH} Lanes are listed in real precedence order.
              </p>
            </div>
          )}

          <OptionCard
            selected={selected === "openai_api_key"}
            onClick={() => setSelected("openai_api_key")}
            title={AI_TYPES.openai_api_key.title}
            hint={AI_TYPES.openai_api_key.desc}
          />
          <OptionCard
            selected={selected === "azure_openai"}
            onClick={() => setSelected("azure_openai")}
            title={AI_TYPES.azure_openai.title}
            hint={AI_TYPES.azure_openai.desc}
          />
        </div>

        <DialogFooter>
          <Button variant="outline" onClick={onBack}>
            Back
          </Button>
          <Button onClick={continueClick}>Continue</Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

// ---- AI provider: Panel 3 (connect & review) ----

// The credential cell for the two container-login lanes (Claude subscription,
// Bedrock SSO). It owns the whole lifecycle the pane hands back — which the
// panel previously threw away: after a login it showed a bare "Log in" again,
// no sign anything had happened, no way to tell a relogin from a first login.
// Three states: the pane itself (opens on its consent gate), captured-just-now,
// and already-connected-from-an-earlier-capture (SetupStatus.harness — presence
// and age, never a live check).
function LoginCredentialCell({
  provider,
  status,
  onCancelAll,
}: {
  provider: "anthropic" | "aws";
  status: SetupStatus;
  onCancelAll: () => void;
}) {
  const [open, setOpen] = React.useState(true);
  const [freshCapture, setFreshCapture] = React.useState(false);
  const existing = status.harness?.find((h) => h.provider === provider && h.captured);

  if (open) {
    return (
      <HarnessLoginPane
        provider={provider}
        onDone={() => {
          setOpen(false);
          setFreshCapture(true);
        }}
        // Backing out of a RE-login keeps the credential you already have;
        // only a first-ever login has nothing to fall back to, so only that
        // cancel leaves the panel.
        onCancel={freshCapture || existing ? () => setOpen(false) : onCancelAll}
      />
    );
  }

  const line = freshCapture
    ? provider === "anthropic"
      ? "Subscription captured — stored write-only. Runs get it injected proxy-side; a run's sandbox never holds it."
      : "AWS SSO session captured — stored write-only. Bedrock runs exchange it for short-lived role credentials."
    : existing
      ? `Already connected — captured ${existing.captured_at ? relativeTime(existing.captured_at) : "earlier"}${existing.aging ? " · reconnect soon" : ""}.`
      : null;

  return (
    <div className="space-y-2">
      {line ? (
        <p
          className={`flex items-start gap-2 text-xs leading-snug ${freshCapture ? "text-success" : "text-muted-foreground"}`}
          data-testid="login-captured-line"
        >
          <ShieldCheck className="mt-px size-3.5 shrink-0" /> <span className="min-w-0">{line}</span>
        </p>
      ) : (
        <p className="text-xs leading-snug text-muted-foreground">Not connected yet.</p>
      )}
      <Button size="sm" variant="outline" onClick={() => setOpen(true)}>
        <KeyRound className="size-3.5" /> {line ? "Log in again" : "Log in"}
      </Button>
    </div>
  );
}

const AGENT_SLOT = /Claude Code|Codex/;
const FEATURES_SLOT = /^Wardyn features/;

function ConnectReviewPanel({
  type,
  hostCli,
  bedrockLane,
  existingAiRows,
  secretNames,
  status,
  onBack,
  onDone,
}: {
  type: AiType;
  hostCli?: boolean;
  bedrockLane?: BedrockLane;
  existingAiRows: IntegrationRow[];
  secretNames: string[];
  status: SetupStatus;
  onBack: () => void;
  onDone: () => void;
}) {
  // UI-WS-2: the Name field used to be an editable Input that nothing ever
  // read back — deriveAiRows always renders aiRowName() for these four types
  // regardless of what's stored, so a typed rename silently vanished on Add.
  // A fact, not a control, closes the gap honestly instead of wiring a write
  // path deriveAiRows would still ignore.
  const name = aiRowName(type, hostCli);
  const capRows = AI_TYPES[type].capabilityPreview(hostCli);
  const residency = aiResidency(type, hostCli, bedrockLane);
  const resMeta = RESIDENCY_META[residency];

  const canAgent = capRows.some((r) => !r.fact && r.on && AGENT_SLOT.test(r.label));
  const canFeat = capRows.some((r) => !r.fact && r.on && FEATURES_SLOT.test(r.label));
  // Auto-check whenever the type can hold the slot at all (matches AddKey,
  // AddAzure, AddOpenAI's own fixtures) — with ONE deliberate exception: a
  // Claude subscription is a personal credential, so it opts OUT of silently
  // becoming Wardyn's own AI-features backend even though the capability
  // itself is on (AddManaged's fixture leaves this box unchecked).
  const [checkedAgent, setCheckedAgent] = React.useState(canAgent);
  const [checkedFeat, setCheckedFeat] = React.useState(canFeat && type !== "anthropic_subscription");
  const replacesAgent = checkedAgent ? defaultHolder(existingAiRows, AGENT_SLOT) : undefined;
  const replacesFeat = checkedFeat ? defaultHolder(existingAiRows, FEATURES_SLOT) : undefined;
  const [submitting, setSubmitting] = React.useState(false);

  const [secretDialogName, setSecretDialogName] = React.useState<string | null>(null);

  const keySecretName =
    type === "anthropic_api_key"
      ? "anthropic-api-key"
      : type === "openai_api_key"
        ? "openai-api-key"
        : type === "azure_openai"
          ? "azure-openai-key"
          : type === "bedrock" && bedrockLane === "bearer"
            ? "bedrock-api-key"
            : undefined;

  // UI-WS-2: persist the two DefaultFor checkboxes — Add used to patch only
  // local state, so a pre-checked "replaces X" note never actually replaced
  // anything. `status` (this dialog's own prop) is a snapshot from BEFORE the
  // credential above was saved, so it re-GETs rather than trusting it — the
  // same staleness discipline reloadSiteConfig uses for site config.
  const handleAdd = async () => {
    const marks: [DefaultForMark, boolean][] = [];
    if (canAgent) marks.push(["agent_runs", checkedAgent]);
    if (canFeat) marks.push(["wardyn_features", checkedFeat]);
    const serverId = aiServerId(type, hostCli);
    if (serverId && marks.length) {
      setSubmitting(true);
      try {
        const fresh = await setupApi.getSetupStatus();
        let wire: WireIntegration | undefined = fresh.integrations?.find((w) => w.id === serverId);
        // Nothing to adopt/PUT yet (e.g. the operator never entered the
        // credential) — the defaults simply don't persist; Add still succeeds.
        if (wire) {
          for (const [mark, on] of marks) {
            if (on === !!wire.default_for?.includes(mark)) continue;
            await setDefaultFor(wire, mark, on);
            // setDefaultFor's own PUT is a full replace of whatever `wire` it's
            // handed — advance the local copy so a SECOND mark in this same
            // loop doesn't PUT a stale default_for and clobber the first.
            const df: string[] = wire.default_for ?? [];
            wire = { ...wire, source: "stored", default_for: on ? [...df, mark] : df.filter((m: string) => m !== mark) };
          }
        }
      } catch (e) {
        toast.error("Couldn't set the default", { description: getErrorMessage(e) });
      } finally {
        setSubmitting(false);
      }
    }
    onDone();
  };

  return (
    <Dialog open onOpenChange={(o) => !o && onBack()}>
      {/* This is the one dialog that can host a terminal, so its sizing is
          pinned INLINE — beyond any stylesheet cascade — and horizontal
          overflow is structurally impossible: overflowX hidden here, min-w-0
          on every grid child so no leaf's min-content can widen them, and the
          terminal scrolls inside its own container. */}
      <DialogContent
        className="scroll-thin"
        style={{ maxWidth: "min(52rem, calc(100vw - 2rem))", maxHeight: "calc(100vh - 96px)", overflowY: "auto", overflowX: "hidden" }}
      >
        <DialogHeader className="min-w-0">
          <DialogTitle>Add integration — {AI_TYPES[type].title}</DialogTitle>
          <DialogDescription>Connect &amp; review — what&apos;s stored, what it powers, where it lives.</DialogDescription>
        </DialogHeader>

        <div className="min-w-0 space-y-4 py-1">
          <Field label="Name" hint="How this row reads in lists and pickers.">
            <p className="text-sm text-foreground">{name}</p>
          </Field>

          <div className="space-y-2 rounded-xl border border-border p-3.5">
            <SectionLabel>Credential</SectionLabel>
            {keySecretName ? (
              <CredentialKeyRow name={keySecretName} onOpen={() => setSecretDialogName(keySecretName)} />
            ) : type === "anthropic_subscription" && hostCli ? (
              <p className="text-xs leading-snug text-muted-foreground">
                {T.HOSTCLI_LINE} Wardyn detects it automatically once you&apos;ve logged in with the Claude CLI on
                this host — there&apos;s nothing to capture here.
              </p>
            ) : type === "anthropic_subscription" ? (
              <LoginCredentialCell provider="anthropic" status={status} onCancelAll={onBack} />
            ) : type === "bedrock" && bedrockLane === "sso" ? (
              <LoginCredentialCell provider="aws" status={status} onCancelAll={onBack} />
            ) : type === "bedrock" && bedrockLane === "static" ? (
              <div className="space-y-2">
                {["aws-access-key-id", "aws-secret-access-key", "aws-session-token"].map((n) => (
                  <CredentialKeyRow key={n} name={n} onOpen={() => setSecretDialogName(n)} />
                ))}
              </div>
            ) : (
              <p className="text-xs leading-snug text-muted-foreground">
                Boot-time config — set on wardynd (region/model, or the ~/.aws mount), not stored here.
              </p>
            )}
          </div>

          <div className="space-y-2 rounded-xl border border-border p-3.5">
            <SectionLabel>What this powers</SectionLabel>
            <CapabilityTable rows={capRows} />
          </div>

          <div className="space-y-2 rounded-xl border border-border p-3.5">
            <SectionLabel>Residency</SectionLabel>
            <div className="flex items-start gap-2.5">
              <Chip tone={resMeta.tone}>{resMeta.label}</Chip>
              <p className="flex-1 text-xs leading-snug text-muted-foreground">{resMeta.tooltip}</p>
            </div>
          </div>

          {(canAgent || canFeat) && (
            <div className="space-y-3 rounded-xl border border-border p-3.5">
              <SectionLabel>Defaults</SectionLabel>
              {canAgent && (
                <DefaultCheckbox
                  id="def-agent"
                  label="Default for agent runs"
                  checked={checkedAgent}
                  onCheckedChange={setCheckedAgent}
                  replaces={replacesAgent?.name}
                />
              )}
              {canFeat && (
                <DefaultCheckbox
                  id="def-features"
                  label="Default for Wardyn features"
                  checked={checkedFeat}
                  onCheckedChange={setCheckedFeat}
                  replaces={replacesFeat?.name}
                />
              )}
            </div>
          )}
        </div>

        <div className="min-w-0 flex flex-col gap-2">
          <p className="text-right text-[0.6875rem] text-muted-foreground">{T.STORE_NOTE}</p>
          <DialogFooter>
            <Button variant="outline" onClick={onBack} disabled={submitting}>
              Back
            </Button>
            <Button onClick={() => void handleAdd()} disabled={submitting}>
              {submitting ? "Adding…" : "Add integration"}
            </Button>
          </DialogFooter>
        </div>
      </DialogContent>

      <AddSecretDialog
        open={!!secretDialogName}
        onOpenChange={(o) => !o && setSecretDialogName(null)}
        lockName
        initialName={secretDialogName ?? ""}
        existingNames={secretNames}
        onSaved={() => setSecretDialogName(null)}
      />
    </Dialog>
  );
}

function CredentialKeyRow({ name, onOpen }: { name: string; onOpen: () => void }) {
  return (
    <div className="flex flex-wrap items-center gap-2.5">
      <Mono className="text-foreground">{name}</Mono>
      <Button size="sm" variant="outline" onClick={onOpen}>
        <KeyRound className="size-3.5" /> Enter value
      </Button>
    </div>
  );
}

function DefaultCheckbox({
  id,
  label,
  checked,
  onCheckedChange,
  replaces,
}: {
  id: string;
  label: string;
  checked: boolean;
  onCheckedChange: (v: boolean) => void;
  replaces?: string;
}) {
  return (
    <div className="flex items-start gap-2.5">
      <Checkbox id={id} checked={checked} onCheckedChange={(v) => onCheckedChange(!!v)} className="mt-0.5" />
      <label htmlFor={id} className="cursor-pointer text-sm text-foreground">
        {label}
        {replaces ? (
          <span className="block text-[0.6875rem] text-warning">replaces {replaces}</span>
        ) : (
          <span className="block text-[0.6875rem] text-muted-foreground">Auto-checked — nothing holds this mark yet.</span>
        )}
      </label>
    </div>
  );
}
