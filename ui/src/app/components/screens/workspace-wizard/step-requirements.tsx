/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Step ③ Requirements — the UNGROUPED stack mock frames WzS3 / WzS3Empty /
// WzS3NoProfile render via AddWorkspaceWizard's bodyReqs + RequirementsGroups
// (mockup/wardyn-workspaces.js). NO tabs: the sub-tab redesign in
// docs/design/workspace-wizard-s3-requirements-update-prompt.md is a forward
// illustration for a later round, explicitly out of scope here.
import * as React from "react";
import { AlertTriangle } from "lucide-react";
import { Button } from "../../ui/button";
import { RadioGroup, RadioGroupItem } from "../../ui/radio-group";
import { Chip } from "../../wardyn/primitives";
import { JsonBlock, Mono } from "../../wardyn/code-block";
import { ConfirmEgressDialog } from "../../wardyn/confirm-egress-dialog";
import { AddSecretDialog } from "../secrets";
import { C } from "../../../lib/workspace-copy";
import type { WorkspaceProfile } from "../../../lib/types";
import {
  requirementKey,
  setRequirementLane,
  splitRequirementKey,
  unmetRequiredSecrets,
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
  return (
    <div className="space-y-2 rounded-lg border border-danger/40 bg-danger-subtle p-3" data-testid="leak-banner">
      <div className="flex items-center gap-2 text-danger">
        <AlertTriangle className="size-4 shrink-0" />
        <span className="text-[0.8125rem] font-semibold">Suspected committed secrets — rotate or remove before mounting</span>
      </div>
      <div className="space-y-1">
        {leaks.map((lk, i) => (
          <Mono key={i} className="block text-xs text-foreground">
            {lk.path}
            {lk.line != null ? `:${lk.line}` : ""} — {lk.kind}
          </Mono>
        ))}
      </div>
      <p className="text-[0.6875rem] leading-snug text-danger/90">{C.LOCATION_ONLY}</p>
    </div>
  );
}

export function StepRequirements({
  profile,
  sources,
  requirements,
  onChange,
  storedSecretNames,
  onSecretStored,
}: {
  profile: WorkspaceProfile | null | undefined;
  sources: SourceRow[];
  requirements: WorkspaceRequirementsMap;
  onChange: (next: WorkspaceRequirementsMap) => void;
  storedSecretNames: string[];
  onSecretStored: (name: string) => void;
}) {
  const [addSecretName, setAddSecretName] = React.useState<string | null>(null);
  const [pendingHost, setPendingHost] = React.useState<string | null>(null);

  const setLane = (key: string, level: RequirementLevel) => onChange(setRequirementLane(requirements, key, level));

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

  return (
    <div className="space-y-5">
      <div className="space-y-1 rounded-lg border border-border bg-surface-2/40 p-3">
        <p className="text-[0.6875rem] leading-snug text-foreground">{C.REQ_DEF}</p>
        <p className="text-[0.6875rem] leading-snug text-foreground">{C.OPT_DEF}</p>
        <p className="text-[0.6875rem] italic leading-snug text-muted-foreground">{C.SEEDED}</p>
      </div>

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
        <div className="space-y-5">
          {leaks.length > 0 && <LeakBanner leaks={leaks} />}

          {secrets.length > 0 && (
            <section className="space-y-2" data-testid="group-secrets">
              <GroupHead title="Secrets" chips={<Chip tone="neutral">names only</Chip>} />
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
              </div>
              {unmet.length > 0 && (
                <p className="text-[0.6875rem] leading-snug text-warning">
                  {unmet.length} required secret{unmet.length > 1 ? "s aren't" : " isn't"} stored yet. {C.UNMET_OK}
                </p>
              )}
              <p className="text-[0.6875rem] leading-snug text-muted-foreground">{C.DECLARED}</p>
              <p className="text-[0.6875rem] leading-snug text-muted-foreground">{C.SECRET_INJECT}</p>
            </section>
          )}

          <section className="space-y-2" data-testid="group-egress">
            <GroupHead title="Network egress" />
            {autoAllowed.length > 0 ? (
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
              </div>
            ) : (
              <p className="text-xs text-muted-foreground">No hosts beyond the auto-allowed set.</p>
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
          </section>

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

          <section className="space-y-2">
            <p className="text-[0.6875rem] leading-snug text-muted-foreground">{C.BLIND_SPOT}</p>
            <details className="rounded-lg border border-border">
              <summary className="cursor-pointer px-3 py-2 text-xs font-medium text-muted-foreground">Raw profile</summary>
              <div className="px-2 pb-2">
                <JsonBlock value={profile ?? {}} />
              </div>
            </details>
          </section>
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
