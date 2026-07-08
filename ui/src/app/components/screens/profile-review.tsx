/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Recording-Mode profile review — a drawer that calls POST /runs/{id}/profile and
// renders the synthesized least-privilege proposal. The layout CLONES
// compose-review.tsx (overall-risk header, summary grid, deterministic risk rows,
// warnings, verbatim inline_policy) and ADDS the raw observations block (egress
// domains with methods + decision tallies, exec argv0s, file writes, connects,
// and a highlighted anomalies section). A "Save as policy" action persists the
// proposed inline_policy via POST /policies.
import * as React from "react";
import {
  AlertTriangle,
  ArrowRightLeft,
  FilePen,
  Globe,
  KeyRound,
  Loader2,
  Save,
  Sparkles,
  TerminalSquare,
} from "lucide-react";
import { toast } from "sonner";
import type { ProfileProposal, RiskItem, RunPolicySpec } from "../../lib/types";
import { firstUseLabel } from "../../lib/types";
import { api } from "../../lib/api";
import {
  Sheet,
  SheetContent,
  SheetHeader,
  SheetTitle,
} from "../ui/sheet";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "../ui/dialog";
import { Button } from "../ui/button";
import { Input } from "../ui/input";
import { Label } from "../ui/label";
import { Mono, YamlBlock } from "../wardyn/code-block";
import { Chip, ConfinementChip, RiskBadge } from "../wardyn/primitives";
import { ErrorState, TableSkeleton } from "../wardyn/states";
import { RUN_MODE } from "../wardyn/copy";

export function ProfileReview({
  runId,
  suggestedName,
  onClose,
  onSavedPolicy,
}: {
  runId: string | null;
  // Pre-filled "save as is" policy name (e.g. workspace-recording). When set, a
  // one-click "Save as is" button persists the policy under it without the dialog.
  suggestedName?: string;
  onClose: () => void;
  // Called after a policy is successfully saved from the synthesized profile, so
  // a parent (e.g. a policies list) can refresh. Optional.
  onSavedPolicy?: () => void;
}) {
  const [proposal, setProposal] = React.useState<ProfileProposal | null>(null);
  const [status, setStatus] = React.useState<"loading" | "error" | "ready">("loading");
  const [saveOpen, setSaveOpen] = React.useState(false);
  const [savingAsIs, setSavingAsIs] = React.useState(false);

  // "Save as is" — persist the synthesized policy directly under the suggested name
  // (workspace + recording), skipping the name dialog. Falls back to the dialog if
  // no name was supplied.
  const saveAsIs = React.useCallback(async () => {
    if (!proposal || !suggestedName) return;
    setSavingAsIs(true);
    try {
      await api.createPolicy(suggestedName, proposal.proposed.inline_policy);
      toast.success(`Saved policy “${suggestedName}”`);
      onSavedPolicy?.();
      onClose();
    } catch (e) {
      toast.error("Failed to save policy", { description: (e as Error).message });
    } finally {
      setSavingAsIs(false);
    }
  }, [proposal, suggestedName, onSavedPolicy, onClose]);

  const load = React.useCallback(() => {
    if (!runId) return;
    setStatus("loading");
    setProposal(null);
    api
      .profileRun(runId)
      .then((p) => {
        setProposal(p);
        setStatus("ready");
      })
      .catch(() => setStatus("error"));
  }, [runId]);

  React.useEffect(() => {
    if (runId) load();
  }, [runId, load]);

  return (
    <>
      <Sheet open={!!runId} onOpenChange={(o) => !o && onClose()}>
        <SheetContent
          className="scroll-thin w-full gap-0 overflow-y-auto p-0 sm:max-w-[680px]"
          style={{ maxWidth: "98vw" }}
        >
          <SheetHeader className="border-b border-border p-5">
            <SheetTitle className="flex items-center gap-2">
              <Sparkles className="size-4 text-primary" />
              Synthesized profile
            </SheetTitle>
            <p className="text-sm text-muted-foreground">
              Wardyn replayed this run's observed behaviour into a proposed least-privilege policy.
              Advisory and read-only — nothing is created until you save it.
            </p>
            {runId && <Mono className="text-foreground">{runId}</Mono>}
          </SheetHeader>

          {status === "loading" ? (
            <div className="p-5">
              <TableSkeleton rows={5} cols={3} />
            </div>
          ) : status === "error" ? (
            <div className="p-5">
              <ErrorState
                message="Couldn't synthesize a profile for this run."
                onRetry={load}
              />
            </div>
          ) : proposal ? (
            <ProfileBody
              proposal={proposal}
              suggestedName={suggestedName}
              savingAsIs={savingAsIs}
              onSaveAsIs={saveAsIs}
              onSave={() => setSaveOpen(true)}
            />
          ) : null}
        </SheetContent>
      </Sheet>

      {proposal && (
        <SavePolicyDialog
          open={saveOpen}
          onOpenChange={setSaveOpen}
          spec={proposal.proposed.inline_policy}
          defaultName={suggestedName}
          onSaved={() => {
            setSaveOpen(false);
            onSavedPolicy?.();
          }}
        />
      )}
    </>
  );
}

function ProfileBody({
  proposal,
  suggestedName,
  savingAsIs,
  onSaveAsIs,
  onSave,
}: {
  proposal: ProfileProposal;
  suggestedName?: string;
  savingAsIs?: boolean;
  onSaveAsIs?: () => void;
  onSave: () => void;
}) {
  const { proposed, risk_assessment, overall_risk, observations, warnings } = proposal;
  const { run, inline_policy } = proposed;

  return (
    <div className="space-y-5 p-5">
      <div className="flex items-center gap-2">
        <span className="text-sm font-medium text-foreground">Overall risk</span>
        <RiskBadge level={overall_risk} />
      </div>

      {/* --- proposed run + policy summary (mirrors the compose Review grid) --- */}
      <div className="grid grid-cols-2 gap-x-4 gap-y-3 rounded-lg border border-border p-3 text-sm">
        <Summary label="Agent" value={<Mono className="text-foreground">{run.agent}</Mono>} />
        <Summary
          label="Mode"
          value={run.interactive ? RUN_MODE.interactive.label : RUN_MODE.autonomous.label}
        />
        <Summary label="Repo" value={<Mono className="text-foreground">{run.repo || "—"}</Mono>} />
        <Summary
          label="Confinement"
          value={<ConfinementChip value={inline_policy.min_confinement_class} />}
        />
        <Summary
          label="Allowed domains"
          value={
            inline_policy.allow_all_egress
              ? "Allow all (deny-list only)"
              : `${inline_policy.allowed_domains.length} allowed`
          }
        />
        <Summary
          label="Eligible grants"
          value={
            (inline_policy.eligible_grants?.length ?? 0) === 0 ? (
              "none"
            ) : (
              <div className="flex flex-wrap gap-1">
                {inline_policy.eligible_grants!.map((g, i) => (
                  <Chip key={i} tone="info" mono className="px-1.5 py-0 text-[10px]">
                    {String(g.kind)}
                  </Chip>
                ))}
              </div>
            )
          }
        />
        <Summary
          label="First-use approval"
          value={firstUseLabel(inline_policy.first_use_approval)}
        />
      </div>

      {/* --- allowed_domains, listed --- */}
      {!inline_policy.allow_all_egress && inline_policy.allowed_domains.length > 0 && (
        <div>
          <SectionLabel>Proposed allowed domains</SectionLabel>
          <div className="flex flex-wrap gap-1.5">
            {inline_policy.allowed_domains.map((d) => (
              <span
                key={d}
                className="inline-flex items-center gap-1 rounded-md border border-border bg-surface-2 px-2 py-0.5 font-mono text-[11px] text-foreground"
              >
                {d}
              </span>
            ))}
          </div>
        </div>
      )}

      {/* --- deterministic risk assessment, one badged row per choice --- */}
      <div>
        <SectionLabel>Risk assessment</SectionLabel>
        <ul className="divide-y divide-border rounded-lg border border-border" aria-label="Risk assessment">
          {risk_assessment.map((item, i) => (
            <RiskRow key={`${item.field}-${i}`} item={item} />
          ))}
          {risk_assessment.length === 0 && (
            <li className="px-3 py-2 text-xs text-muted-foreground">No graded items.</li>
          )}
        </ul>
      </div>

      {/* --- observations: the raw, deterministic record the proposal is from --- */}
      <Observations observations={observations} />

      {/* --- analyzer / clamp warnings --- */}
      {warnings && warnings.length > 0 && (
        <div className="rounded-lg border border-warning/30 bg-warning-subtle p-3">
          <div className="mb-1 text-[11px] font-medium uppercase tracking-wide text-warning">
            Warnings
          </div>
          <ul className="list-disc space-y-0.5 pl-4 text-xs text-warning">
            {warnings.map((w, i) => (
              <li key={i}>{w}</li>
            ))}
          </ul>
        </div>
      )}

      {/* --- proposed inline_policy, verbatim --- */}
      <div>
        <Label className="text-[11px] uppercase tracking-wide text-muted-foreground">
          inline_policy (proposed)
        </Label>
        <YamlBlock value={inline_policy} className="mt-1.5" />
      </div>

      {/* --- actions --- */}
      <div className="flex flex-wrap items-center justify-end gap-2 border-t border-border pt-4">
        {suggestedName && onSaveAsIs && (
          <Button onClick={onSaveAsIs} disabled={savingAsIs} data-testid="profile-save-as-is">
            {savingAsIs ? <Loader2 className="size-4 animate-spin" /> : <Save className="size-4" />}
            Save as is (<span className="font-mono">{suggestedName}</span>)
          </Button>
        )}
        <Button variant={suggestedName ? "outline" : "default"} onClick={onSave}>
          <Save className="size-4" /> Save as{suggestedName ? " with a name…" : " policy"}
        </Button>
      </div>
    </div>
  );
}

// Exported so the import Record step's per-task review card renders the SAME
// observations block (egress domains + tallies, minted grants, execs, writes,
// connects, anomalies) profile-review renders — one block, no drift.
export function Observations({ observations }: { observations: ProfileProposal["observations"] }) {
  // The backend drops EMPTY arrays (Go omitempty), so any of these can be absent on a
  // real capture (e.g. a recording that only reached egress → no exec/writes/connects).
  // Default each to [] so the .length/.map render can't crash ("reading 'length' of
  // undefined"). Applies to both the record review card and the compose proposal.
  const {
    domains = [],
    minted_grant_ids = [],
    exec_argv0s = [],
    file_writes = [],
    connects = [],
    anomalies = [],
  } = observations ?? {};
  return (
    <div className="space-y-3" aria-label="Observations">
      <SectionLabel>Observations</SectionLabel>

      {/* egress domains with the HTTP methods seen + decision tallies */}
      <div className="rounded-lg border border-border">
        <div className="flex items-center gap-2 border-b border-border px-3 py-2 text-xs text-muted-foreground">
          <Globe className="size-3.5" /> Egress domains
          <span className="rounded-full bg-muted px-1.5 text-[11px]">{domains.length}</span>
        </div>
        {domains.length === 0 ? (
          <EmptyMini text="No egress was observed." />
        ) : (
          <ul className="divide-y divide-border">
            {domains.map((d) => (
              <li key={d.host} className="px-3 py-2">
                <div className="flex flex-wrap items-center justify-between gap-2">
                  <span className="font-mono text-[12.5px] text-foreground">{d.host}</span>
                  <div className="flex items-center gap-1.5">
                    {d.allow_count > 0 && (
                      <Chip tone="success" className="px-1.5 py-0 text-[10px]">
                        {d.allow_count} allow
                      </Chip>
                    )}
                    {d.deny_count > 0 && (
                      <Chip tone="danger" className="px-1.5 py-0 text-[10px]">
                        {d.deny_count} deny
                      </Chip>
                    )}
                    {d.pending_count > 0 && (
                      <Chip tone="warning" className="px-1.5 py-0 text-[10px]">
                        {d.pending_count} pending
                      </Chip>
                    )}
                  </div>
                </div>
                {d.methods && d.methods.length > 0 && (
                  <div className="mt-1 flex flex-wrap gap-1">
                    {d.methods.map((m) => (
                      <span
                        key={m}
                        className="rounded border border-border bg-surface-2 px-1 font-mono text-[10px] text-muted-foreground"
                      >
                        {m}
                      </span>
                    ))}
                  </div>
                )}
              </li>
            ))}
          </ul>
        )}
      </div>

      <ObsList icon={KeyRound} title="Minted grants" items={minted_grant_ids} />
      <ObsList icon={TerminalSquare} title="Executed (argv0)" items={exec_argv0s} />
      <ObsList icon={FilePen} title="File writes" items={file_writes} />
      <ObsList icon={ArrowRightLeft} title="Connects" items={connects} />

      {/* anomalies — the highlighted "unexpected" channel */}
      <div
        className="rounded-lg border border-danger/40 bg-danger-subtle p-3"
        data-testid="profile-anomalies"
      >
        <div className="flex items-center gap-2 text-danger">
          <AlertTriangle className="size-4" />
          <span className="text-sm font-semibold">Anomalies</span>
          <span className="rounded-full bg-danger/15 px-1.5 text-[11px] text-danger">
            {anomalies.length}
          </span>
        </div>
        {anomalies.length === 0 ? (
          <p className="mt-2 text-xs text-muted-foreground">
            No anomalies detected during the recording.
          </p>
        ) : (
          <ul className="mt-2 list-disc space-y-0.5 pl-4 text-xs text-danger">
            {anomalies.map((a, i) => (
              <li key={i} className="font-mono">
                {a}
              </li>
            ))}
          </ul>
        )}
      </div>
    </div>
  );
}

function ObsList({
  icon: Icon,
  title,
  items,
}: {
  icon: React.ElementType;
  title: string;
  items: string[];
}) {
  return (
    <div className="rounded-lg border border-border">
      <div className="flex items-center gap-2 border-b border-border px-3 py-2 text-xs text-muted-foreground">
        <Icon className="size-3.5" /> {title}
        <span className="rounded-full bg-muted px-1.5 text-[11px]">{items.length}</span>
      </div>
      {items.length === 0 ? (
        <EmptyMini text="None observed." />
      ) : (
        <ul className="space-y-1 px-3 py-2">
          {items.map((it, i) => (
            <li key={`${it}-${i}`} className="break-all font-mono text-[12px] text-foreground">
              {it}
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}

// SavePolicyDialog — names the proposed inline_policy and persists it via
// POST /policies. The server validates the spec; a 400 surfaces inline.
function SavePolicyDialog({
  open,
  onOpenChange,
  spec,
  defaultName,
  onSaved,
}: {
  open: boolean;
  onOpenChange: (o: boolean) => void;
  spec: RunPolicySpec;
  // Pre-fill the name field (workspace + recording); editable before saving.
  defaultName?: string;
  onSaved: () => void;
}) {
  const [name, setName] = React.useState(defaultName || "recorded-profile");
  const [saving, setSaving] = React.useState(false);
  const [error, setError] = React.useState<string | null>(null);

  React.useEffect(() => {
    if (open) {
      setName(defaultName || "recorded-profile");
      setError(null);
      setSaving(false);
    }
  }, [open, defaultName]);

  const save = async () => {
    setError(null);
    if (!name.trim()) {
      setError("Name is required.");
      return;
    }
    setSaving(true);
    try {
      await api.createPolicy(name.trim(), spec);
      toast.success(`Saved policy “${name.trim()}”`);
      onSaved();
    } catch (e) {
      setError((e as Error).message || "Failed to save policy.");
      setSaving(false);
    }
  };

  return (
    <Dialog open={open} onOpenChange={(o) => !o && onOpenChange(false)}>
      <DialogContent className="sm:max-w-md">
        <DialogHeader>
          <DialogTitle>Save as policy</DialogTitle>
          <DialogDescription>
            Persist the synthesized inline_policy as a named, reusable run policy. The spec is
            validated server-side before it is saved.
          </DialogDescription>
        </DialogHeader>

        <div className="space-y-2 py-1">
          <Label htmlFor="profile-policy-name">Policy name</Label>
          <Input
            id="profile-policy-name"
            placeholder="recorded-profile"
            value={name}
            onChange={(e) => setName(e.target.value)}
          />
          {error && (
            <div className="rounded-lg border border-danger/30 bg-danger-subtle px-3 py-2 text-xs text-danger">
              {error}
            </div>
          )}
        </div>

        <DialogFooter>
          <Button variant="ghost" onClick={() => onOpenChange(false)} disabled={saving}>
            Cancel
          </Button>
          <Button onClick={save} disabled={saving || !name.trim()}>
            {saving ? <Loader2 className="size-4 animate-spin" /> : <Save className="size-4" />}
            Save policy
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

function RiskRow({ item }: { item: RiskItem }) {
  return (
    <li className="flex items-start gap-3 px-3 py-2">
      <div className="mt-0.5 shrink-0">
        <RiskBadge level={item.risk_level} />
      </div>
      <div className="min-w-0 flex-1">
        <div className="flex flex-wrap items-baseline gap-x-2">
          <span className="font-mono text-xs text-foreground">{item.field}</span>
          <span className="font-mono text-[11px] text-muted-foreground">= {item.value}</span>
          {item.invariant_ref && (
            <span className="text-[10px] text-muted-foreground">
              (invariant {item.invariant_ref})
            </span>
          )}
        </div>
        <p className="mt-0.5 text-[11px] leading-snug text-muted-foreground">{item.rationale}</p>
      </div>
    </li>
  );
}

function Summary({ label, value }: { label: string; value: React.ReactNode }) {
  return (
    <div>
      <div className="text-[11px] uppercase tracking-wide text-muted-foreground">{label}</div>
      <div className="mt-0.5 text-foreground">{value}</div>
    </div>
  );
}

function SectionLabel({ children }: { children: React.ReactNode }) {
  return (
    <div className="mb-1.5 text-[11px] uppercase tracking-wide text-muted-foreground">{children}</div>
  );
}

function EmptyMini({ text }: { text: string }) {
  return <div className="px-3 py-4 text-center text-xs text-muted-foreground">{text}</div>;
}
