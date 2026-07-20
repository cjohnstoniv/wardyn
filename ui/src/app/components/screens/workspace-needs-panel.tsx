/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import { AlertTriangle, Check, History, KeyRound, Loader2, X } from "lucide-react";
import { toast } from "sonner";
import { workspaces as api } from "../../lib/api/workspaces";
import { getErrorMessage } from "../../lib/format";
import type { SecretNeed, Workspace, WorkspaceProfile } from "../../lib/types";
import { cn } from "../ui/utils";
import { Button } from "../ui/button";
import { JsonBlock, Mono } from "../wardyn/code-block";
import { ConfirmEgressDialog } from "../wardyn/confirm-egress-dialog";
import { Chip, SectionLabel } from "../wardyn/primitives";

// A single host row in one of WorkspaceNeedsPanel's egress lists — identical
// shape across "Approved by you" (remove), "Suggested" and "Observed but
// denied" (both approve), differing only in the action's icon/label/variant.
function HostRow({
  host,
  busy,
  action,
  onClick,
}: {
  host: string;
  busy: string | null;
  action: "approve" | "remove";
  onClick: () => void;
}) {
  const isRemove = action === "remove";
  return (
    <li className="flex items-center gap-2 rounded-lg border border-border px-2.5 py-1.5">
      <Mono className="flex-1 text-foreground">{host}</Mono>
      <Button
        size="sm"
        variant={isRemove ? "ghost" : "outline"}
        className={cn("h-7", isRemove && "text-danger hover:text-danger")}
        onClick={onClick}
        disabled={busy === host}
      >
        {busy === host ? (
          <Loader2 className="size-3.5 animate-spin" />
        ) : isRemove ? (
          <X className="size-3.5" />
        ) : (
          <Check className="size-3.5" />
        )}
        {isRemove ? "Remove" : "Approve"}
      </Button>
    </li>
  );
}

// ------------------------------------------------------------
// WorkspaceNeedsPanel — the "View profile" dialog body. Turns the raw scan
// profile into an operator-legible "what this workspace needs / touches" view:
// what it declares (secrets by NAME only, services), what it may reach (egress in
// three trust tiers), and what it exposes (present .env-style files). Everything
// here is deterministic, content-derived, and UNTRUSTED — the copy says so, and
// there are no value affordances for any declared secret. The raw JSON stays one
// <details> away for the operator who wants the unfiltered output.
// ------------------------------------------------------------
export function WorkspaceNeedsPanel({
  workspace,
  onWorkspaceUpdated,
  onAddSecret,
  brokeredSecretNames,
}: {
  workspace: Workspace;
  onWorkspaceUpdated: (w: Workspace) => void;
  // Optional (import flow only): when set, each declared secret row that isn't
  // already brokered gets an "Add" action (opens the caller's AddSecretDialog).
  // The /workspaces screen + profile dialog omit it — read-only there, unchanged.
  onAddSecret?: (name: string) => void;
  brokeredSecretNames?: string[];
}) {
  // Typed cast-read of the loosely-typed wire profile (see types.ts). A field the
  // scanner didn't emit is simply undefined => that section doesn't render.
  const profile = (workspace.profile ?? {}) as WorkspaceProfile;
  const approved = workspace.approved_egress ?? [];
  const autoAllowed = profile.egress_domains ?? [];
  // A suggested host that's already auto-allowed or already operator-approved has
  // graduated to a stronger tier — drop it here so a host never appears twice.
  const suggested = (profile.suggested_egress ?? []).filter(
    (h) => !autoAllowed.includes(h) && !approved.includes(h),
  );
  // Split declared config/credentials (from .env keys, ${} placeholders,
  // SealedSecrets, secretKeyRef, compose env) from names that were only READ in
  // source code or a CI workflow — the latter mix config (ports, flags) with
  // secrets, so they're shown as an advisory group, not called "secrets".
  const allNeeds = profile.required_secrets ?? [];
  const secrets = allNeeds.filter((s) => s.kind !== "code" && s.kind !== "ci");
  const codeRefs = allNeeds.filter((s) => s.kind === "code" || s.kind === "ci");
  const services = profile.services_needed ?? [];
  const secretFiles = profile.secret_files_present ?? [];
  const leakFindings = profile.leak_findings ?? [];
  const buildMemMib = profile.build_memory_mib;

  // Host currently being PUT (disables its own button); pendingApprove drives the
  // confirm dialog (a suggested/observed host awaiting the untrusted-content
  // acknowledgement — one confirm shared by both promotion sources).
  const [busy, setBusy] = React.useState<string | null>(null);
  const [pendingApprove, setPendingApprove] = React.useState<string[] | null>(null);
  // Observed-but-denied egress is fetched lazily (the "Check run history" button)
  // so opening the panel never triggers a run-history read the operator didn't ask
  // for. null => not yet fetched; the section shows the button until then.
  const [observed, setObserved] = React.useState<{ denied: string[]; runs_examined: number } | null>(null);
  const [observedLoading, setObservedLoading] = React.useState(false);
  const checkRunHistory = async () => {
    setObservedLoading(true);
    try {
      setObserved(await api.getObservedEgress(workspace.id));
    } catch (e) {
      toast.error("Failed to load observed egress", {
        description: getErrorMessage(e),
      });
    } finally {
      setObservedLoading(false);
    }
  };
  // Denied hosts that haven't already graduated to a stronger tier (same filter as
  // suggested) — so an already-approved/auto-allowed host never shows here too.
  const observedDenied = (observed?.denied ?? []).filter(
    (h) => !autoAllowed.includes(h) && !approved.includes(h),
  );

  // setApprovedEgress is a FULL replacement — send the whole desired list, then
  // adopt the server's returned workspace as the new source of truth.
  const apply = async (host: string, next: string[]) => {
    setBusy(host);
    try {
      onWorkspaceUpdated(await api.setApprovedEgress(workspace.id, next));
    } catch (e) {
      toast.error("Failed to update approved egress", {
        description: getErrorMessage(e),
      });
    } finally {
      setBusy(null);
    }
  };
  const removeHost = (host: string) => apply(host, approved.filter((h) => h !== host));

  return (
    <div className="max-h-[65vh] space-y-4 overflow-y-auto pr-1">
      {profile.needs_review && (
        <div className="flex items-start gap-2 rounded-lg border border-warning/30 bg-warning-subtle px-3 py-2 text-xs text-warning">
          <AlertTriangle className="mt-0.5 size-3.5 shrink-0" />
          <span>Low-confidence scan — review before relying on this profile.</span>
        </div>
      )}

      {leakFindings.length > 0 && (
        <div
          className="flex items-start gap-2 rounded-lg border border-danger/30 bg-danger-subtle px-3 py-2.5"
          data-testid="ws-leaks"
        >
          <AlertTriangle className="mt-0.5 size-4 shrink-0 text-danger" />
          <div className="min-w-0 space-y-1">
            <p className="text-xs font-semibold text-danger">
              Suspected committed secrets — rotate/remove before mounting
            </p>
            <ul className="space-y-0.5">
              {leakFindings.map((f, i) => (
                <li key={`${f.path}:${f.line ?? ""}:${f.kind}:${i}`} className="flex flex-wrap items-baseline gap-x-1.5">
                  <Mono className="text-foreground">
                    {f.path}
                    {f.line != null ? `:${f.line}` : ""}
                  </Mono>
                  <span className="text-[11px] text-muted-foreground">— {f.kind}</span>
                </li>
              ))}
            </ul>
            <p className="text-[11px] leading-snug text-muted-foreground">
              Only the location and detector are flagged — the secret value itself is never shown or stored.
            </p>
          </div>
        </div>
      )}

      <ChipRow label="Languages" items={profile.languages} />
      <ChipRow label="Package managers" items={profile.package_managers} />
      <ChipRow label="Tools" items={profile.tools} />

      {(profile.has_devcontainer || profile.has_dockerfile) && (
        <div className="flex flex-wrap gap-1.5">
          {profile.has_devcontainer && <Chip tone="cyan">devcontainer</Chip>}
          {profile.has_dockerfile && <Chip tone="cyan">Dockerfile</Chip>}
        </div>
      )}

      {typeof buildMemMib === "number" && buildMemMib >= 4096 && (
        <p className="text-xs text-muted-foreground">
          Build wants ~{Math.round(buildMemMib / 1024)} GB memory
        </p>
      )}

      {secrets.length > 0 && (
        <section className="space-y-2" data-testid="ws-secrets">
          <SectionLabel>Secrets this workspace declares ({secrets.length})</SectionLabel>
          <ul className="space-y-1.5">
            {secrets.map((s) => (
              <SecretNeedRow
                key={s.name}
                need={s}
                onAddSecret={onAddSecret}
                brokered={brokeredSecretNames?.includes(s.name)}
              />
            ))}
          </ul>
          <p className="text-[11px] leading-snug text-muted-foreground">
            Declared by workspace files (untrusted) — names only, values are never read.
          </p>
        </section>
      )}

      {codeRefs.length > 0 && (
        <section className="space-y-2" data-testid="ws-code-refs">
          <SectionLabel>Also read in code / CI ({codeRefs.length})</SectionLabel>
          <ul className="space-y-1.5">
            {codeRefs.map((s) => (
              <SecretNeedRow key={s.name} need={s} />
            ))}
          </ul>
          <p className="text-[11px] leading-snug text-muted-foreground">
            Env vars referenced in source or CI — advisory, and may be plain config (ports, flags),
            not credentials.
          </p>
        </section>
      )}

      {services.length > 0 && (
        <section className="space-y-2">
          <SectionLabel>Services</SectionLabel>
          <div className="flex flex-wrap gap-1.5">
            {services.map((s) => (
              <Chip key={s} tone="neutral">
                {s}
              </Chip>
            ))}
          </div>
        </section>
      )}

      {(autoAllowed.length > 0 || approved.length > 0 || suggested.length > 0) && (
        <section className="space-y-3">
          <SectionLabel>Egress</SectionLabel>

          {autoAllowed.length > 0 && (
            <div className="space-y-1.5">
              <p className="text-[11px] font-medium text-muted-foreground">Allowed automatically</p>
              <div className="flex flex-wrap gap-1.5">
                {autoAllowed.map((h) => (
                  <Chip key={h} tone="success" mono>
                    {h}
                  </Chip>
                ))}
              </div>
            </div>
          )}

          {approved.length > 0 && (
            <div className="space-y-1.5">
              <p className="text-[11px] font-medium text-muted-foreground">Approved by you</p>
              <ul className="space-y-1.5">
                {approved.map((h) => (
                  <HostRow key={h} host={h} busy={busy} action="remove" onClick={() => removeHost(h)} />
                ))}
              </ul>
            </div>
          )}

          {suggested.length > 0 && (
            <div className="space-y-1.5">
              <p className="text-[11px] font-medium text-muted-foreground">
                Suggested — needs review, not auto-allowed
              </p>
              <ul className="space-y-1.5">
                {suggested.map((h) => (
                  <HostRow
                    key={h}
                    host={h}
                    busy={busy}
                    action="approve"
                    onClick={() => setPendingApprove([h])}
                  />
                ))}
              </ul>
            </div>
          )}
        </section>
      )}

      <section className="space-y-2" data-testid="ws-observed-egress">
        <SectionLabel>Observed but denied</SectionLabel>
        <p className="text-[11px] leading-snug text-muted-foreground">
          Hosts that runs using this workspace tried to reach but were denied — least-privilege
          promotion candidates. Approve one to allow it for every future run that mounts this workspace.
        </p>
        {observed === null ? (
          <Button size="sm" variant="outline" onClick={checkRunHistory} disabled={observedLoading}>
            {observedLoading ? <Loader2 className="size-3.5 animate-spin" /> : <History className="size-3.5" />}
            Check run history
          </Button>
        ) : observedDenied.length === 0 ? (
          <p className="text-[11px] text-muted-foreground">
            No denied egress observed
            {observed.runs_examined
              ? ` from ${observed.runs_examined} recent run${observed.runs_examined === 1 ? "" : "s"}`
              : ""}
            .
          </p>
        ) : (
          <>
            <ul className="space-y-1.5">
              {observedDenied.map((h) => (
                <HostRow
                  key={h}
                  host={h}
                  busy={busy}
                  action="approve"
                  onClick={() => setPendingApprove([h])}
                />
              ))}
            </ul>
            <p className="text-[11px] text-muted-foreground">
              from {observed.runs_examined} recent run{observed.runs_examined === 1 ? "" : "s"}
            </p>
          </>
        )}
      </section>

      {secretFiles.length > 0 && (
        <div className="flex items-start gap-2 rounded-lg border border-warning/30 bg-warning-subtle px-3 py-2.5">
          <AlertTriangle className="mt-0.5 size-4 shrink-0 text-warning" />
          <div className="min-w-0 space-y-1">
            <p className="text-xs font-semibold text-warning">Secret files present</p>
            <ul className="space-y-0.5">
              {secretFiles.map((f) => (
                <li key={f}>
                  <Mono className="text-foreground">{f}</Mono>
                </li>
              ))}
            </ul>
            <p className="text-[11px] leading-snug text-muted-foreground">
              These files would be readable by the agent if this directory is mounted.
            </p>
          </div>
        </div>
      )}

      <p className="text-[11px] leading-snug text-muted-foreground">
        Detected from committed files only — runtime hosts hidden behind env-var defaults, secrets
        mentioned only in docs, and files deeper than 4 levels are not visible to the scan.
      </p>

      <details className="rounded-lg border border-border">
        <summary className="cursor-pointer px-3 py-2 text-xs font-medium text-muted-foreground">
          Raw profile
        </summary>
        <div className="px-2 pb-2">
          <JsonBlock value={workspace.profile ?? {}} />
        </div>
      </details>

      <ConfirmEgressDialog
        hosts={pendingApprove}
        onOpenChange={(o) => !o && setPendingApprove(null)}
        onConfirm={() => {
          const host = pendingApprove?.[0];
          setPendingApprove(null);
          if (host) apply(host, [...approved, host]);
        }}
      />
    </div>
  );
}

function ChipRow({ label, items }: { label: string; items?: string[] }) {
  if (!items || items.length === 0) return null;
  return (
    <div className="flex flex-wrap items-center gap-1.5">
      <span className="text-[11px] font-medium text-muted-foreground">{label}:</span>
      {items.map((it) => (
        <Chip key={it} tone="neutral">
          {it}
        </Chip>
      ))}
    </div>
  );
}

function SecretNeedRow({
  need,
  onAddSecret,
  brokered,
}: {
  need: SecretNeed;
  onAddSecret?: (name: string) => void;
  brokered?: boolean;
}) {
  // "code" (env var seen only in source — lower confidence) and "ci" (referenced
  // only in a CI workflow) get a plain-language provenance badge instead of the raw
  // kind id; both arrive optional=true, so they suppress the generic "optional" chip.
  // A deploy-time secret gets the clearer "deploy-time" word; any other optional
  // secret is just "optional". None ever exposes a value affordance.
  const advisoryKind = need.kind === "code" ? "from source" : need.kind === "ci" ? "CI-only" : null;
  const optionalLabel =
    !advisoryKind && need.optional ? (need.kind === "deploy" ? "deploy-time" : "optional") : null;
  return (
    <li className="flex flex-wrap items-center gap-2">
      <Mono className="text-foreground">{need.name}</Mono>
      {advisoryKind ? (
        <Chip tone="neutral">{advisoryKind}</Chip>
      ) : (
        need.kind && <Chip tone="info">{need.kind}</Chip>
      )}
      {optionalLabel && <Chip tone="neutral">{optionalLabel}</Chip>}
      {onAddSecret &&
        (brokered ? (
          <Chip tone="success" dot className="ml-auto">
            brokered
          </Chip>
        ) : (
          <Button
            size="sm"
            variant="outline"
            className="ml-auto h-6"
            onClick={() => onAddSecret(need.name)}
          >
            <KeyRound className="size-3" /> Add
          </Button>
        ))}
    </li>
  );
}
