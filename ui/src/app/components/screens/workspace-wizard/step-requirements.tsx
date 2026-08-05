/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Step ④ Requirements — four tabs (Reach · Secrets · Files & services ·
// services), matching mockup2/wardyn-workspaces.js's AddWorkspaceWizardV2
// bodyReqs2/RD2 (V2S3Reqs / V2S3ReqsEgress / V2S3ReqsNone fixtures). The tab
// design existed on paper before any mock drew it, so an earlier pass shipped
// an ungrouped stack instead (mockup/wardyn-workspaces.js's AddWorkspaceWizard
// bodyReqs) — this reorganizes that SAME row-level behavior (lane toggles,
// stored/not-stored pills, the holding block, the leak banner, the blind-spot
// + raw-profile disclosure) into the now-drawn tabs. Nothing the ungrouped
// stack rendered was dropped, only regrouped. bodyReqs2 itself is a simplified
// static illustration (fixed example rows, no lane controls) — the real
// per-row interactivity below carries over from the shipped implementation,
// not from the mock's illustration.
import * as React from "react";
import { AlertTriangle } from "lucide-react";
import { Button } from "../../ui/button";
import { RadioGroup, RadioGroupItem } from "../../ui/radio-group";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "../../ui/tabs";
import { Chip } from "../../wardyn/primitives";
import { JsonBlock, Mono } from "../../wardyn/code-block";
import { ConfirmEgressDialog } from "../../wardyn/confirm-egress-dialog";
import { AddSecretDialog } from "../secrets";
import { C, RD2 } from "../../../lib/workspace-copy";
import type { SetupStatus, WorkspaceProfile } from "../../../lib/types";
import { IntegrationRequirements } from "./integration-requirements";
import {
  isFixtureLeak,
  requirementKey,
  setRequirementLane,
  splitRequirementKey,
  unmetRequiredSecrets,
  type PowerSource,
  type RequirementLevel,
  type SourceRow,
  type WorkspaceRequirementsMap,
} from "./wizard-types";

// The Required | Optional segmented control every contract row gets — a
// RadioGroup pair, never a Switch, so "neither" is never a representable state.
export function RequiredOptionalToggle({
  idPrefix,
  label,
  value,
  onChange,
}: {
  idPrefix: string;
  label: string;
  value: RequirementLevel;
  onChange: (level: RequirementLevel) => void;
}) {
  return (
    <RadioGroup
      value={value}
      onValueChange={(v) => onChange(v as RequirementLevel)}
      aria-label={label}
      className="flex flex-none flex-row items-center gap-3"
    >
      <label className="flex items-center gap-1.5 text-xs text-foreground" htmlFor={`${idPrefix}-required`}>
        <RadioGroupItem value="required" id={`${idPrefix}-required`} /> Required
      </label>
      <label className="flex items-center gap-1.5 text-xs text-foreground" htmlFor={`${idPrefix}-optional`}>
        <RadioGroupItem value="optional" id={`${idPrefix}-optional`} /> Optional
      </label>
    </RadioGroup>
  );
}

function GroupHead({ title, chips }: { title: string; chips?: React.ReactNode }) {
  return (
    <div className="flex flex-wrap items-center gap-2">
      <h4 className="text-[0.8125rem] font-semibold text-foreground">{title}</h4>
      {chips}
    </div>
  );
}

function LeakBanner({ leaks }: { leaks: { path: string; kind: string; line?: number }[] }) {
  // Two tiers, path-classified (isFixtureLeak): outside test-conventional
  // paths the red headline is unchanged; under them, one muted collapsed line —
  // "rotate" is meaningless advice for a fixture, "confirm they're fake" is the
  // honest ask. Never suppression: still shown, still counted, still expandable.
  const hot = leaks.filter((l) => !isFixtureLeak(l));
  const fixtures = leaks.filter(isFixtureLeak);
  return (
    <div className="space-y-2" data-testid="leak-banner">
      {hot.length > 0 && (
        <div className="space-y-2 rounded-lg border border-danger/40 bg-danger-subtle p-3" data-testid="leak-hot">
          <div className="flex items-center gap-2 text-danger">
            <AlertTriangle className="size-4 shrink-0" />
            <span className="text-[0.8125rem] font-semibold">Suspected committed secrets — rotate or remove before mounting</span>
          </div>
          <div className="space-y-1">
            {hot.map((lk, i) => (
              <Mono key={i} className="block text-xs text-foreground">
                {lk.path}
                {lk.line != null ? `:${lk.line}` : ""} — {lk.kind}
              </Mono>
            ))}
          </div>
          <p className="text-[0.6875rem] leading-snug text-danger/90">{C.LOCATION_ONLY}</p>
        </div>
      )}
      {fixtures.length > 0 && (
        <div className="space-y-2 rounded-lg border border-border bg-surface-2/50 p-3" data-testid="leak-fixtures">
          <details>
            <summary className="cursor-pointer select-none text-xs leading-snug text-muted-foreground">
              {fixtures.length} key-shaped string{fixtures.length > 1 ? "s" : ""} in test files — usually fixtures; confirm
              they&apos;re fake. They mount like everything else.
            </summary>
            <div className="space-y-1 pt-2">
              {fixtures.map((lk, i) => (
                <Mono key={i} className="block text-xs text-foreground">
                  {lk.path}
                  {lk.line != null ? `:${lk.line}` : ""} — {lk.kind}
                </Mono>
              ))}
            </div>
          </details>
          <p className="text-[0.6875rem] leading-snug text-muted-foreground">{C.LOCATION_ONLY}</p>
        </div>
      )}
    </div>
  );
}

// One labeled row of the Record tab's carry card.
function CarryRow({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <div className="grid grid-cols-[150px_1fr] items-baseline gap-2.5">
      <span className="text-[0.6875rem] uppercase tracking-wide text-muted-foreground">{label}</span>
      <span className="text-xs leading-snug text-foreground">{children}</span>
    </div>
  );
}

// A chip naming whatever resolved model access seeded a row — never a
// fabricated host or secret name. PowerSource carries an integration NAME for
// "pinned" (no aiType/host/secret), so only the well-established "default"
// convention (server default == Anthropic, POWER_LINE_DEFAULT's own claim,
// "anthropic-api-key" the codebase-wide placeholder for it) gets a Mono,
// hostname/secret-shaped token; a pinned integration is named plainly instead.
function ResolvedToken({ powerSource, fallback }: { powerSource: PowerSource; fallback: string }) {
  if (powerSource.kind === "pinned") return <span className="text-xs text-foreground">{powerSource.name}</span>;
  return <Mono className="text-xs text-foreground">{fallback}</Mono>;
}

type ReqTab = "reach" | "secrets" | "files" | "record";

export function StepRequirements({
  profile,
  sources,
  requirements,
  onChange,
  storedSecretNames,
  onSecretStored,
  powerSource,
  status,
  verifyPanel,
}: {
  profile: WorkspaceProfile | null | undefined;
  sources: SourceRow[];
  requirements: WorkspaceRequirementsMap;
  onChange: (next: WorkspaceRequirementsMap) => void;
  storedSecretNames: string[];
  onSecretStored: (name: string) => void;
  // The resolved model/harness access from step ②'s power-source line/peek
  // (wizard.tsx's `s.powerSource`) or, on the workspace detail page, the real
  // llm_cred binding mapped onto the same shape (requirements-card.tsx) — one
  // idiom for "does anything resolve for this image's agent tool" everywhere
  // StepRequirements is reused.
  powerSource: PowerSource;
  // Live setup status, only for the integrations a workspace may name. Optional
  // so every existing caller (and every fixture) keeps working — absent simply
  // means the integrations section doesn't render.
  status?: SetupStatus | null;
  // The LIVE verify-session panel (wizard only: it owns a real workspace to
  // launch against). Absent — the detail page, fixtures — keeps the plain
  // buttons, whose real launcher lives elsewhere on that page.
  verifyPanel?: React.ReactNode;
}) {
  const [addSecretName, setAddSecretName] = React.useState<string | null>(null);
  const [pendingHost, setPendingHost] = React.useState<string | null>(null);
  const [reqTab, setReqTab] = React.useState<ReqTab>("reach");

  const setLane = (key: string, level: RequirementLevel) => onChange(setRequirementLane(requirements, key, level));
  // Removing a row is ABSENCE, not a third lane — the contract has exactly two
  // states plus "not in it at all" (see RequiredOptionalToggle).
  const clearRequirement = (key: string) => {
    const next = { ...requirements };
    delete next[key];
    onChange(next);
  };

  const secrets = profile?.required_secrets ?? [];
  const localDirPaths = sources.filter((s) => s.type === "local_dir").map((s) => s.path).filter(Boolean);
  // Egress rows = the profile's auto-allowed hosts UNION any host already
  // promoted into `requirements` (an operator approval from the holding block
  // below) — a promoted host must show up as a real contract row with a lane
  // control, not vanish back into "detected, not required" on next render.
  const promotedEgressHosts = Object.keys(requirements)
    .map((k) => splitRequirementKey(k))
    .filter((s): s is { type: string; rest: string } => !!s && s.type === "egress")
    .map((s) => s.rest);
  const autoAllowed = Array.from(new Set([...(profile?.egress_domains ?? []), ...promotedEgressHosts]));
  const holding = (profile?.suggested_egress ?? []).filter((h) => !autoAllowed.includes(h));
  const services = profile?.services_needed ?? [];
  const leaks = profile?.leak_findings ?? [];

  const hasProfile = profile != null;
  const contractEmpty =
    hasProfile &&
    secrets.length === 0 &&
    (profile?.egress_domains ?? []).length === 0 &&
    holding.length === 0 &&
    services.length === 0 &&
    leaks.length === 0 &&
    localDirPaths.length === 0;

  const unmet = unmetRequiredSecrets(requirements, storedSecretNames);
  const nothingResolves = powerSource.kind === "none";
  // What the Record tab's carry card states, derived from the contract-so-far —
  // these are computed facts, not canon strings.
  const requiredSecretCount = Object.entries(requirements).filter(
    ([k, v]) => k.startsWith("secret:") && v.level === "required",
  ).length;
  const carrySecrets =
    requiredSecretCount > 0
      ? `${requiredSecretCount} required secret${requiredSecretCount > 1 ? "s" : ""} ride${requiredSecretCount > 1 ? "" : "s"} proxy-side`
      : "none yet";
  const carryEgress =
    autoAllowed.length > 0
      ? `${autoAllowed.length} host${autoAllowed.length > 1 ? "s" : ""} allowed — anything else is held at the door`
      : "only the auto-allowed set — anything else is held at the door";

  return (
    <div className="space-y-5">
      {!hasProfile && (
        <div className="rounded-xl border border-border p-4">
          <p className="text-sm font-medium text-foreground">No contract yet</p>
          <p className="mt-1 text-xs text-muted-foreground">{C.NO_CONTRACT}</p>
        </div>
      )}

      {hasProfile && contractEmpty && (
        <div className="space-y-1.5 rounded-xl border border-border p-4">
          <p className="text-sm font-medium text-foreground">Nothing to require</p>
          <p className="text-xs text-muted-foreground">{C.EMPTY_SCAN}</p>
          <p className="text-xs text-muted-foreground">{C.BLIND_SPOT}</p>
        </div>
      )}

      {hasProfile && !contractEmpty && (
        <div className="space-y-4">
          {/* Not tab-scoped — a suspected committed secret is relevant no
              matter which tab is open. */}
          {leaks.length > 0 && <LeakBanner leaks={leaks} />}

          <Tabs value={reqTab} onValueChange={(v) => setReqTab(v as ReqTab)}>
            <TabsList>
              <TabsTrigger value="reach">Reach</TabsTrigger>
              <TabsTrigger value="secrets">Secrets</TabsTrigger>
              <TabsTrigger value="files">Files & services</TabsTrigger>
              <TabsTrigger value="record">Verify</TabsTrigger>
            </TabsList>

            <TabsContent value="reach" className="space-y-2 pt-3" data-testid="group-reach">
              {/* The power source, first: recording and every agent run read
                  this resolution, and nothing on this step re-asks it. The card
                  is DISPLAY-ONLY here — "its page" is the workspace detail
                  page, where the real pin control (the llm-cred dialog) lives. */}
              <div
                data-testid="power-source-card"
                className={`space-y-1 rounded-lg border p-3 ${
                  nothingResolves ? "border-warning/30 bg-warning-subtle" : "border-border bg-surface-2/40"
                }`}
              >
                <div className="flex flex-wrap items-center gap-2">
                  <span
                    className={`text-[0.6875rem] uppercase tracking-wide ${nothingResolves ? "text-warning/80" : "text-muted-foreground"}`}
                  >
                    Agent runs here use
                  </span>
                  <span className={`text-xs font-medium ${nothingResolves ? "text-warning" : "text-foreground"}`}>
                    {nothingResolves
                      ? "nothing yet — this image's agent won't have model access"
                      : powerSource.kind === "pinned"
                        ? `${powerSource.name} — pinned to this workspace`
                        : "server default"}
                  </span>
                  {!nothingResolves && <Chip tone="success">resolves</Chip>}
                </div>
                <p className={`text-[0.6875rem] leading-snug ${nothingResolves ? "text-warning/90" : "text-muted-foreground"}`}>
                  {nothingResolves ? RD2.POWER_NONE_BODY : RD2.POWER_RESOLVES_BODY}
                </p>
              </div>
              <p className="text-[0.6875rem] leading-snug text-muted-foreground">{RD2.REACH_LEAD}</p>
              <IntegrationRequirements
                status={status ?? null}
                requirements={requirements}
                setLane={setLane}
                clear={clearRequirement}
              />
              {autoAllowed.length > 0 || !nothingResolves ? (
                <div className="divide-y divide-border rounded-lg border border-border">
                  {autoAllowed.map((host) => {
                    const key = requirementKey("egress", host);
                    const level = requirements[key]?.level ?? "required";
                    return (
                      <div key={host} className="flex flex-wrap items-center gap-2 p-2.5">
                        <div className="min-w-0">
                          <Mono className="text-xs text-foreground">{host}</Mono>
                          <p className="text-[0.6875rem] text-muted-foreground">auto-allowed</p>
                        </div>
                        <span className="ml-auto" />
                        <RequiredOptionalToggle idPrefix={`egress-${host}`} label={`${host} lane`} value={level} onChange={(l) => setLane(key, l)} />
                      </div>
                    );
                  })}
                  {!nothingResolves && (
                    <div className="flex flex-wrap items-center gap-2 p-2.5">
                      <div className="min-w-0">
                        <ResolvedToken powerSource={powerSource} fallback="api.anthropic.com" />
                        <p className="text-[0.6875rem] text-muted-foreground">seeded</p>
                      </div>
                      <span className="ml-auto" />
                      <Chip tone="info" title={RD2.EGRESS_TIP}>
                        from model access
                      </Chip>
                    </div>
                  )}
                </div>
              ) : (
                <p className="text-xs text-muted-foreground">
                  No hosts beyond the auto-allowed set — the scan found nothing this workspace has to reach.
                </p>
              )}
              {autoAllowed.length === 0 && holding.length === 0 && (
                <button
                  type="button"
                  className="text-[0.6875rem] text-muted-foreground underline"
                  onClick={() => setReqTab("record")}
                >
                  {RD2.ESCAPE}
                </button>
              )}
              {holding.length > 0 && (
                <div className="space-y-2 rounded-lg border border-border bg-surface-2/50 p-3" data-testid="holding-block">
                  <p className="text-[0.6875rem] font-semibold uppercase tracking-wide text-muted-foreground">
                    Detected — not in the contract
                  </p>
                  <p className="text-[0.6875rem] leading-snug text-muted-foreground">{C.HOLDING}</p>
                  <div className="space-y-1.5">
                    {holding.map((host) => (
                      <div key={host} className="flex items-center gap-2 border-t border-border pt-1.5 first:border-t-0 first:pt-0">
                        <Mono className="text-xs text-foreground">{host}</Mono>
                        <span className="text-[0.6875rem] text-muted-foreground">from file content</span>
                        <Button size="sm" variant="outline" className="ml-auto h-7" onClick={() => setPendingHost(host)}>
                          Approve
                        </Button>
                      </div>
                    ))}
                  </div>
                </div>
              )}
            </TabsContent>

            <TabsContent value="secrets" className="space-y-2 pt-3" data-testid="group-secrets">
              <div className="space-y-1 rounded-lg border border-border bg-surface-2/40 p-3">
                <p className="text-[0.6875rem] leading-snug text-foreground">{C.REQ_DEF}</p>
                <p className="text-[0.6875rem] leading-snug text-foreground">{C.OPT_DEF}</p>
                <p className="text-[0.6875rem] italic leading-snug text-muted-foreground">{C.SEEDED}</p>
              </div>
              <div className="flex items-center gap-2">
                <Chip tone="neutral">names only</Chip>
              </div>
              {secrets.length > 0 || !nothingResolves ? (
                <div className="divide-y divide-border rounded-lg border border-border">
                  {secrets.map((s) => {
                    const key = requirementKey("secret", s.name);
                    const stored = storedSecretNames.includes(s.name);
                    const level = requirements[key]?.level ?? "required";
                    return (
                      <div key={s.name} className="flex flex-wrap items-center gap-2 p-2.5">
                        <Mono className="min-w-36 text-xs text-foreground">{s.name}</Mono>
                        {s.kind && <Chip tone="neutral">{s.kind}</Chip>}
                        {stored ? (
                          <Chip tone="success">stored</Chip>
                        ) : (
                          <div className="flex items-center gap-1.5">
                            <Chip tone="warning">not stored yet</Chip>
                            <Button size="sm" variant="outline" className="h-6 px-2 text-[0.6875rem]" onClick={() => setAddSecretName(s.name)}>
                              Add
                            </Button>
                          </div>
                        )}
                        <span className="ml-auto" />
                        <RequiredOptionalToggle idPrefix={`sec-${s.name}`} label={`${s.name} lane`} value={level} onChange={(l) => setLane(key, l)} />
                      </div>
                    );
                  })}
                  {!nothingResolves && (
                    <div className="flex flex-wrap items-center gap-2 p-2.5">
                      <ResolvedToken powerSource={powerSource} fallback="anthropic-api-key" />
                      <span className="text-[0.6875rem] text-muted-foreground">managed</span>
                      <span className="ml-auto" />
                      <Chip tone="info" title={RD2.SECRET_TIP}>
                        integration
                      </Chip>
                    </div>
                  )}
                </div>
              ) : (
                <p className="text-xs text-muted-foreground">No secrets declared.</p>
              )}
              {unmet.length > 0 && (
                <p className="text-[0.6875rem] leading-snug text-warning">
                  {unmet.length} required secret{unmet.length > 1 ? "s aren't" : " isn't"} stored yet. {C.UNMET_OK}
                </p>
              )}
              {secrets.length > 0 && (
                <>
                  <p className="text-[0.6875rem] leading-snug text-muted-foreground">{C.DECLARED}</p>
                  <p className="text-[0.6875rem] leading-snug text-muted-foreground">{C.SECRET_INJECT}</p>
                </>
              )}
            </TabsContent>

            <TabsContent value="files" className="space-y-5 pt-3">
              {localDirPaths.length > 0 && (
                <section className="space-y-2" data-testid="group-write">
                  <GroupHead title="Host directory" />
                  <div className="divide-y divide-border rounded-lg border border-border">
                    {localDirPaths.map((path) => {
                      const key = requirementKey("write", path);
                      const level = requirements[key]?.level ?? "optional";
                      const required = level === "required";
                      return (
                        <div key={path} className="flex flex-wrap items-center gap-2 p-2.5">
                          <div className="min-w-0">
                            <p className="text-xs text-foreground">
                              Write to <Mono className="text-xs">{path}</Mono>
                            </p>
                            <p className={`text-[0.6875rem] leading-snug ${required ? "text-warning" : "text-muted-foreground"}`}>
                              {required
                                ? "Required — every run can change these files on your machine."
                                : "Optional — runs mount it read-only and can ask for write access."}
                            </p>
                          </div>
                          <span className="ml-auto" />
                          <RequiredOptionalToggle idPrefix={`write-${path}`} label={`write access to ${path}`} value={level} onChange={(l) => setLane(key, l)} />
                        </div>
                      );
                    })}
                  </div>
                </section>
              )}

              {services.length > 0 && (
                <section className="space-y-2" data-testid="group-services">
                  <GroupHead title="Services" chips={<Chip tone="neutral">declared, not provisioned</Chip>} />
                  <div className="divide-y divide-border rounded-lg border border-border">
                    {services.map((name) => (
                      <div key={name} className="p-2.5">
                        <Mono className="text-xs text-foreground">{name}</Mono>
                      </div>
                    ))}
                  </div>
                  <p className="text-[0.6875rem] leading-snug text-muted-foreground">{C.SERVICES}</p>
                </section>
              )}

              {localDirPaths.length === 0 && services.length === 0 && (
                <p className="text-xs text-muted-foreground">No local directories or declared services.</p>
              )}

              <section className="space-y-2">
                <p className="text-[0.6875rem] leading-snug text-muted-foreground">{C.BLIND_SPOT}</p>
                <details className="rounded-lg border border-border">
                  <summary className="cursor-pointer px-3 py-2 text-xs font-medium text-muted-foreground">Raw profile</summary>
                  <div className="px-2 pb-2">
                    <JsonBlock value={profile ?? {}} />
                  </div>
                </details>
              </section>
            </TabsContent>

            <TabsContent value="record" className="space-y-3 pt-3" data-testid="group-record">
              <p className="text-[0.6875rem] leading-snug text-muted-foreground">{RD2.RECORD_LEAD}</p>
              <div className="space-y-2 rounded-lg border border-border bg-surface-2/40 p-3" data-testid="record-carry">
                <p className="text-[0.6875rem] font-semibold uppercase tracking-wide text-muted-foreground">{RD2.CARRY}</p>
                <CarryRow label="Power source">
                  {nothingResolves ? (
                    <span className="text-warning">nothing resolves — the power source is on Reach</span>
                  ) : powerSource.kind === "pinned" ? (
                    `${powerSource.name} — pinned to this workspace`
                  ) : (
                    "server default"
                  )}
                </CarryRow>
                <CarryRow label="Required secrets">{carrySecrets}</CarryRow>
                <CarryRow label="Egress posture">{carryEgress}</CarryRow>
                <p className="text-[0.6875rem] leading-snug text-muted-foreground">{RD2.CARRY_FROM}</p>
              </div>
              {nothingResolves && (
                <div className="space-y-2 rounded-lg border border-warning/30 bg-warning-subtle p-2.5">
                  <p className="text-[0.75rem] leading-snug text-warning">{RD2.RECORD_NEEDS}</p>
                  <Button type="button" size="sm" variant="outline" onClick={() => setReqTab("reach")}>
                    Open the Reach tab →
                  </Button>
                </div>
              )}
              {/* ponytail: inert, like the mock's own onClick: noop — launching
                  a real recording session needs a real sandbox/run, which this
                  wizard step doesn't have yet. Wire onClick once the workspace
                  page's record-launch action grows a prop this step can call. */}
              {verifyPanel ?? (
                <>
                  <div className="flex flex-wrap gap-2">
                    {!nothingResolves && (
                      <Button type="button" size="sm">
                        Verify with a session
                      </Button>
                    )}
                    <Button type="button" size="sm" variant={nothingResolves ? "default" : "outline"}>
                      Verify in a terminal
                    </Button>
                  </div>
                  <p className="text-[0.6875rem] leading-snug text-muted-foreground">
                    {nothingResolves ? RD2.TERMINAL_ONLY : RD2.RECORD_SUB}
                  </p>
                </>
              )}
              <p className="text-[0.6875rem] leading-snug text-muted-foreground">{RD2.RECORD_LOOP}</p>
            </TabsContent>
          </Tabs>
        </div>
      )}

      <AddSecretDialog
        open={!!addSecretName}
        onOpenChange={(o) => {
          if (!o) setAddSecretName(null);
        }}
        lockName
        initialName={addSecretName ?? ""}
        onSaved={(name) => {
          onSecretStored(name);
          setAddSecretName(null);
        }}
      />
      <ConfirmEgressDialog
        hosts={pendingHost ? [pendingHost] : null}
        onOpenChange={(o) => {
          if (!o) setPendingHost(null);
        }}
        onConfirm={() => {
          if (pendingHost) setLane(requirementKey("egress", pendingHost), "required");
          setPendingHost(null);
        }}
      />
    </div>
  );
}
