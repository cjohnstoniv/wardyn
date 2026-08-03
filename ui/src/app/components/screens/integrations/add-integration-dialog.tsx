/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Add-integration dialog — mockup's AddCategory / AddTypeAI / AddKey / etc.
// Only the AI-provider category is fully modeled by the approved mock
// (BUILDERS has no Add-flow frame for SCM/mirror/proxy at all beyond the
// category card); per the task brief, SCM hands off to the SAME ladder
// ScmProviderStep already uses (AddProviderPanel1/2, exported from that file
// for exactly this reuse), and Artifact mirror / Host proxy embed the
// existing ArtifactRepoStep / HostProxyStep bodies rather than forking a
// second copy of their real site-config writes.
import * as React from "react";
import { Box, ChevronDown, GitBranch, KeyRound, Network, Sparkles } from "lucide-react";
import {
  AI_TYPES,
  BEDROCK_LANE_META,
  CATEGORY_META,
  RESIDENCY_META,
  SUBSCRIPTION_LANE_META,
  T,
  type AiType,
  type BedrockLane,
  type CapabilityRow,
  type IntegrationCategory,
  type SubscriptionLane,
} from "../../../lib/integrations";
import { aiResidency, aiRowName, defaultHolder, type IntegrationRow } from "../../../lib/api/integrations";
import { health } from "../../../lib/api/health";
import type { SetupStatus, SiteConfig } from "../../../lib/types";
import { Button } from "../../ui/button";
import { Input } from "../../ui/input";
import { Checkbox } from "../../ui/checkbox";
import { Switch } from "../../ui/switch";
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "../../ui/dialog";
import { Field, OptionCard } from "../new-run/step-shell";
import { Mono } from "../../wardyn/code-block";
import { Chip, SectionLabel } from "../../wardyn/primitives";
import { AddSecretDialog } from "../secrets";
import { HarnessLoginPane } from "../setup/harness-login-pane";
import { AddProviderPanel1, AddProviderPanel2, type ProviderOption } from "../setup/scm-provider-step";
import { ArtifactRepoStep, HostProxyStep } from "../setup/step-bodies";
import type { Lane } from "../../../lib/scm-provider";

const CATEGORY_ICON: Record<IntegrationCategory, React.ElementType> = {
  ai_provider: Sparkles,
  scm_host: GitBranch,
  artifact_mirror: Box,
  host_proxy: Network,
};
const CATEGORIES: IntegrationCategory[] = ["ai_provider", "scm_host", "artifact_mirror", "host_proxy"];

type Step =
  | { s: "category" }
  | { s: "scm" }
  | { s: "mirror" }
  | { s: "proxy" }
  | { s: "ai_type" }
  | { s: "ai_connect"; type: AiType; hostCli?: boolean; bedrockLane?: BedrockLane };

export function AddIntegrationDialog({
  open,
  onOpenChange,
  status,
  siteConfig,
  existingAiRows,
  reload,
}: {
  open: boolean;
  onOpenChange: (o: boolean) => void;
  status: SetupStatus;
  siteConfig: SiteConfig;
  existingAiRows: IntegrationRow[];
  reload: () => void;
}) {
  const [step, setStep] = React.useState<Step>({ s: "category" });
  const [category, setCategory] = React.useState<IntegrationCategory>("ai_provider");
  const [localSiteConfig, setLocalSiteConfig] = React.useState<SiteConfig>(siteConfig);
  const [proxySecretName, setProxySecretName] = React.useState<string | null>(null);

  React.useEffect(() => {
    if (open) {
      setStep({ s: "category" });
      setCategory("ai_provider");
      setLocalSiteConfig(siteConfig);
    }
    // Only reset when the dialog transitions open — not on every siteConfig
    // prop tick from the parent's own polling, which would stomp an in-flight
    // edit inside the mirror/proxy hand-off panels.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [open]);

  if (!open) return null;

  const close = () => onOpenChange(false);
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

  const advance = () => {
    setStep(
      category === "ai_provider"
        ? { s: "ai_type" }
        : category === "scm_host"
          ? { s: "scm" }
          : category === "artifact_mirror"
            ? { s: "mirror" }
            : { s: "proxy" },
    );
  };

  if (step.s === "category") {
    return (
      <Dialog open onOpenChange={(o) => !o && close()}>
        <DialogContent className="sm:max-w-xl">
          <DialogHeader>
            <DialogTitle>Add integration</DialogTitle>
            <DialogDescription>
              A named connection to a system outside Wardyn. Nothing here is required — every category
              is skippable.
            </DialogDescription>
          </DialogHeader>
          <div className="grid grid-cols-2 gap-2">
            {CATEGORIES.map((cat) => {
              const Icon = CATEGORY_ICON[cat];
              const meta = CATEGORY_META[cat];
              return (
                <OptionCard
                  key={cat}
                  selected={category === cat}
                  onClick={() => setCategory(cat)}
                  title={
                    <span className="flex items-center gap-2">
                      <Icon className="size-4 text-muted-foreground" /> {meta.title}
                    </span>
                  }
                  hint={meta.skipIfLine}
                />
              );
            })}
          </div>
          <DialogFooter>
            <Button variant="outline" onClick={close}>
              Cancel
            </Button>
            <Button onClick={advance}>Continue</Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    );
  }

  if (step.s === "scm") {
    return (
      <ScmHandoff
        siteConfig={localSiteConfig}
        reloadSiteConfig={reloadSiteConfig}
        saveSiteConfig={saveSiteConfig}
        onBack={() => setStep({ s: "category" })}
        onDone={finish}
      />
    );
  }

  if (step.s === "mirror") {
    return (
      <Dialog open onOpenChange={(o) => !o && close()}>
        <DialogContent className="scroll-thin sm:max-w-xl" style={{ maxHeight: "calc(100vh - 96px)", overflowY: "auto" }}>
          <DialogHeader>
            <DialogTitle>Add integration — Artifact mirror</DialogTitle>
            <DialogDescription>{CATEGORY_META.artifact_mirror.skipIfLine}</DialogDescription>
          </DialogHeader>
          <ArtifactRepoStep
            status={status}
            siteConfig={localSiteConfig}
            reloadSiteConfig={reloadSiteConfig}
            saveSiteConfig={saveSiteConfig}
            onRecheck={() => {}}
            rechecking={false}
          />
          <DialogFooter>
            <Button variant="outline" onClick={() => setStep({ s: "category" })}>
              Back
            </Button>
            <Button onClick={finish}>Done</Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    );
  }

  if (step.s === "proxy") {
    return (
      <Dialog open onOpenChange={(o) => !o && close()}>
        <DialogContent className="sm:max-w-xl">
          <DialogHeader>
            <DialogTitle>Add integration — Host proxy</DialogTitle>
            <DialogDescription>{CATEGORY_META.host_proxy.skipIfLine}</DialogDescription>
          </DialogHeader>
          <HostProxyStep
            status={status}
            siteConfig={localSiteConfig}
            reloadSiteConfig={reloadSiteConfig}
            saveSiteConfig={saveSiteConfig}
            onAddSecret={setProxySecretName}
            onRecheck={() => {}}
            rechecking={false}
          />
          <DialogFooter>
            <Button variant="outline" onClick={() => setStep({ s: "category" })}>
              Back
            </Button>
            <Button onClick={finish}>Done</Button>
          </DialogFooter>
        </DialogContent>
        <AddSecretDialog
          open={!!proxySecretName}
          onOpenChange={(o) => !o && setProxySecretName(null)}
          initialName={proxySecretName ?? ""}
          onSaved={() => setProxySecretName(null)}
        />
      </Dialog>
    );
  }

  if (step.s === "ai_type") {
    return (
      <AiTypePanel
        status={status}
        onBack={() => setStep({ s: "category" })}
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
      onBack={() => setStep({ s: "ai_type" })}
      onDone={finish}
    />
  );
}

// ---- SCM hand-off: the existing App/PAT/SSH ladder, not a second one ----

function ScmHandoff({
  siteConfig,
  reloadSiteConfig,
  saveSiteConfig,
  onBack,
  onDone,
}: {
  siteConfig: SiteConfig;
  reloadSiteConfig: () => Promise<void>;
  saveSiteConfig: (next: SiteConfig) => Promise<void>;
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
      return true;
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
  onBack,
  onContinue,
}: {
  status: SetupStatus;
  onBack: () => void;
  onContinue: (type: AiType, hostCli?: boolean, bedrockLane?: BedrockLane) => void;
}) {
  const [selected, setSelected] = React.useState<AiType>("anthropic_api_key");
  const [subLane, setSubLane] = React.useState<SubscriptionLane>("managed");
  const [advancedOpen, setAdvancedOpen] = React.useState(false);
  const [bedrockLane, setBedrockLane] = React.useState<BedrockLane>("bearer");
  // Sealed control plane (wardynd itself runs in a container): the host-CLI
  // subscription lane can never be satisfied here — same gate llm-access.tsx
  // uses for its own "Set up Claude subscription" framing.
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

// mockup's capRow: a Switch for on/off, NO control at all for an impossible
// row — just the muted fact text (never a disabled switch pretending a choice
// exists). Always disabled: a capability row is a FACT about what the
// credential type does, not a per-instance setting anything here can persist.
function CapabilityTable({ rows, editor }: { rows: CapabilityRow[]; editor?: boolean }) {
  return (
    <div className="divide-y divide-border rounded-lg border border-border">
      {rows.map((r, i) => (
        <div key={i} className="flex items-center justify-between gap-3 px-3 py-2.5">
          <div className="min-w-0 flex-1 space-y-0.5">
            <div className="flex flex-wrap items-center gap-2">
              <span className="text-sm text-foreground">{r.label}</span>
              {r.def && (
                <Chip tone="primary" mono>
                  default
                </Chip>
              )}
              {/* ponytail: no persistence exists for a per-capability default
                  yet — inert, matches the mock's own noop handlers exactly. */}
              {editor && r.makeDefault && (
                <button type="button" className="text-[0.6875rem] text-primary hover:underline">
                  Make default
                </button>
              )}
            </div>
            {!r.fact && r.note && <p className="text-[0.6875rem] leading-snug text-muted-foreground">{r.note}</p>}
          </div>
          {r.fact ? (
            <span className="max-w-[280px] shrink-0 text-right text-[0.6875rem] leading-snug text-muted-foreground" title={r.fact}>
              {r.fact}
            </span>
          ) : (
            <Switch checked={!!r.on} disabled aria-label={r.label} />
          )}
        </div>
      ))}
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
  onBack,
  onDone,
}: {
  type: AiType;
  hostCli?: boolean;
  bedrockLane?: BedrockLane;
  existingAiRows: IntegrationRow[];
  onBack: () => void;
  onDone: () => void;
}) {
  const [name, setName] = React.useState(aiRowName(type, hostCli));
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

  const [secretDialogName, setSecretDialogName] = React.useState<string | null>(null);
  const [loginOpen, setLoginOpen] = React.useState(
    (type === "anthropic_subscription" && !hostCli) || (type === "bedrock" && bedrockLane === "sso"),
  );

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

  return (
    <Dialog open onOpenChange={(o) => !o && onBack()}>
      <DialogContent className="scroll-thin sm:max-w-2xl" style={{ maxHeight: "calc(100vh - 96px)", overflowY: "auto" }}>
        <DialogHeader>
          <DialogTitle>Add integration — {AI_TYPES[type].title}</DialogTitle>
          <DialogDescription>Connect &amp; review — what&apos;s stored, what it powers, where it lives.</DialogDescription>
        </DialogHeader>

        <div className="space-y-4 py-1">
          <Field label="Name" htmlFor="int-name" hint="Yours to change — it's how rows read in lists and pickers.">
            <Input id="int-name" value={name} onChange={(e) => setName(e.target.value)} />
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
              loginOpen ? (
                <HarnessLoginPane provider="anthropic" onDone={() => setLoginOpen(false)} onCancel={onBack} />
              ) : (
                <Button size="sm" variant="outline" onClick={() => setLoginOpen(true)}>
                  <KeyRound className="size-3.5" /> Log in
                </Button>
              )
            ) : type === "bedrock" && bedrockLane === "sso" ? (
              loginOpen ? (
                <HarnessLoginPane provider="aws" onDone={() => setLoginOpen(false)} onCancel={onBack} />
              ) : (
                <Button size="sm" variant="outline" onClick={() => setLoginOpen(true)}>
                  <KeyRound className="size-3.5" /> Log in
                </Button>
              )
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

        <div className="flex flex-col gap-2">
          <p className="text-right text-[0.6875rem] text-muted-foreground">{T.STORE_NOTE}</p>
          <DialogFooter>
            <Button variant="outline" onClick={onBack}>
              Back
            </Button>
            <Button onClick={onDone}>Add integration</Button>
          </DialogFooter>
        </div>
      </DialogContent>

      <AddSecretDialog
        open={!!secretDialogName}
        onOpenChange={(o) => !o && setSecretDialogName(null)}
        lockName
        initialName={secretDialogName ?? ""}
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
