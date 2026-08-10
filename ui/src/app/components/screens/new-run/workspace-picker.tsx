/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Onboarded-workspace multi-select for the New Run wizard's Basics step (and
// the AI Run Composer's own Workspaces field — see compose-form.tsx). Composed
// from the SAME primitives the rest of the wizard already uses — a search
// combobox (mirrors StepAccess's SecretCombobox), a removable-chip list, and a
// disclosed per-item config card — rather than inventing new interaction
// patterns.
//
// Each selected card renders straight from the workspace's requirements
// contract (Workspace.requirements, summarized by wizard-types.ts's
// summarizeWorkspaceRequirements): a "Comes with:" line for what rides along
// automatically (Required), and an "Available if you need it" block of
// per-run checkboxes over the Optional set that write into the selection's
// enabledOptional (wire: WorkspaceSelection.enabled_optional).
import * as React from "react";
import { ArrowUpRight, ChevronsUpDown, Plus, TriangleAlert, X } from "lucide-react";
import { Button } from "../../ui/button";
import { Input } from "../../ui/input";
import { Label } from "../../ui/label";
import { Switch } from "../../ui/switch";
import { Checkbox } from "../../ui/checkbox";
import { Popover, PopoverContent, PopoverTrigger } from "../../ui/popover";
import {
  Command,
  CommandEmpty,
  CommandGroup,
  CommandInput,
  CommandItem,
  CommandList,
} from "../../ui/command";
import { Chip } from "../../wardyn/primitives";
import { Mono } from "../../wardyn/code-block";
import type { Workspace } from "../../../lib/types";
import { secrets as secretsApi } from "../../../lib/api/secrets";
import { C } from "../../../lib/workspace-copy";
// Reuse the SAME status vocabulary /workspaces itself renders, so this picker
// can't drift from that screen.
import { KIND_META } from "../workspaces";
import { statusTone, statusWord } from "../../../lib/workspace-status";
import {
  comesWithLine,
  compositionSummary,
  secretAutoGrants,
  summarizeWorkspaceRequirements,
  unstoredRequiredSecrets,
  type RequirementEntry,
  type RunWorkspaceSelection,
} from "./wizard-types";

// "Available if you need it" preview budget, shared across secrets+hosts
// (secrets first, hosts fill the remainder) — keeps a junk-heavy scan from
// walling the card in checkboxes.
const OPTIONAL_PREVIEW_CAP = 6;

export function WorkspacePicker({
  selections,
  onChange,
  workspaces,
  loading,
  onAddWorkspace,
  optionalRequirementsEnabled = true,
}: {
  selections: RunWorkspaceSelection[];
  onChange: (s: RunWorkspaceSelection[]) => void;
  workspaces: Workspace[];
  loading: boolean;
  onAddWorkspace: () => void;
  // The "Available if you need it" optional-requirement checkboxes write into
  // enabledOptional. The manual wizard's buildSpec forwards it onto
  // CreateRunRequest.Workspaces; the AI Run Composer forwards it onto
  // ComposeRequest.WorkspaceSelections (toRunWorkspacesWire, new-run-dialog.tsx)
  // — both wire it now, so this defaults true for every caller.
  optionalRequirementsEnabled?: boolean;
}) {
  const byId = new Map(workspaces.map((w) => [w.id, w]));
  const selectedIds = new Set(selections.map((s) => s.workspaceId));
  const available = workspaces.filter((w) => !selectedIds.has(w.id));
  // Lifted out of the combobox so the empty-state's "Attach a workspace" button
  // (below) can open the SAME popover instead of duplicating its interaction.
  const [comboOpen, setComboOpen] = React.useState(false);

  // Stored secret names, self-fetched: the "N secrets it requires aren't
  // stored" attention line needs them, and fetching here (rather than adding a
  // new required prop) keeps every existing call site — step-basics.tsx,
  // compose-form.tsx — unchanged. Best-effort: a failed fetch just means the
  // attention line stays quiet rather than the picker crashing.
  const [storedSecrets, setStoredSecrets] = React.useState<string[]>([]);
  React.useEffect(() => {
    let alive = true;
    secretsApi
      .listSecrets()
      .then((names) => {
        if (alive) setStoredSecrets(names);
      })
      .catch(() => {});
    return () => {
      alive = false;
    };
  }, []);

  const add = (id: string) => onChange([...selections, { workspaceId: id }]);
  const remove = (id: string) => onChange(selections.filter((s) => s.workspaceId !== id));
  const patch = (id: string, p: Partial<RunWorkspaceSelection>) =>
    onChange(selections.map((s) => (s.workspaceId === id ? { ...s, ...p } : s)));

  return (
    <div className="space-y-3">
      <div className="flex items-center gap-2">
        <WorkspaceCombobox
          workspaces={available}
          loading={loading}
          onSelect={add}
          onAddWorkspace={onAddWorkspace}
          open={comboOpen}
          onOpenChange={setComboOpen}
        />
        <Button type="button" variant="ghost" size="sm" onClick={onAddWorkspace}>
          <Plus className="size-4" /> Add workspace
        </Button>
      </div>

      {selections.length === 0 ? (
        <div className="space-y-2.5 rounded-lg border border-border p-3">
          <p className="text-sm font-medium text-foreground">No workspace attached</p>
          <p className="text-[0.6875rem] leading-snug text-muted-foreground">
            The run gets an empty scratch directory inside the sandbox — nothing on your machine is
            reachable. Attach a workspace to work on real code.
          </p>
          {/* "+ Add workspace" (onboard a brand-new one) is already the toolbar
              button right above this box — this CTA only needs to add the ONE
              thing that box doesn't already offer: attaching one that already
              exists. */}
          <Button type="button" variant="outline" size="sm" onClick={() => setComboOpen(true)}>
            Attach a workspace
          </Button>
        </div>
      ) : (
        <div className="space-y-2">
          {selections.map((sel, i) => (
            <SelectedWorkspaceCard
              key={sel.workspaceId}
              sel={sel}
              index={i}
              ws={byId.get(sel.workspaceId)}
              storedSecrets={storedSecrets}
              optionalRequirementsEnabled={optionalRequirementsEnabled}
              onRemove={() => remove(sel.workspaceId)}
              onPatch={(p) => patch(sel.workspaceId, p)}
            />
          ))}
        </div>
      )}
    </div>
  );
}

function OptionalCheckbox({
  id,
  label,
  sub,
  checked,
  onCheckedChange,
  warnUnmetOk,
}: {
  id: string;
  label: string;
  sub?: string;
  checked: boolean;
  onCheckedChange: (c: boolean) => void;
  // TRUST BOUNDARY (wizard-types.ts's secretAutoGrants): a scan_seeded
  // optional secret NEVER auto-grants, checked or not — the SAME caveat the
  // disclosed Required list already carries (below). Without it this checkbox
  // reads as a real opt-in the server silently skips.
  warnUnmetOk?: boolean;
}) {
  return (
    <label htmlFor={id} className="inline-flex cursor-pointer items-center gap-2">
      <Checkbox id={id} checked={checked} onCheckedChange={(v) => onCheckedChange(v === true)} />
      <span className="text-xs text-foreground">{label}</span>
      {sub && <span className="text-[0.6875rem] text-muted-foreground">{sub}</span>}
      {warnUnmetOk && (
        <span className="text-[0.6875rem] text-warning" title={C.UNMET_OK}>
          won&apos;t auto-grant
        </span>
      )}
    </label>
  );
}

function SelectedWorkspaceCard({
  sel,
  index,
  ws,
  storedSecrets,
  optionalRequirementsEnabled,
  onRemove,
  onPatch,
}: {
  sel: RunWorkspaceSelection;
  index: number;
  ws: Workspace | undefined;
  storedSecrets: string[];
  optionalRequirementsEnabled: boolean;
  onRemove: () => void;
  onPatch: (p: Partial<RunWorkspaceSelection>) => void;
}) {
  const [showRequired, setShowRequired] = React.useState(false);
  const [showAllOptional, setShowAllOptional] = React.useState(false);
  const KindIcon = (ws && KIND_META[ws.kind] ? KIND_META[ws.kind] : KIND_META.local_dir).Icon;
  const summary = ws ? summarizeWorkspaceRequirements(ws) : undefined;
  const unstored = ws ? unstoredRequiredSecrets(ws, storedSecrets) : [];
  const composition = ws ? compositionSummary(ws) : null;
  const enabledOptional = new Set(sel.enabledOptional ?? []);

  const toggleOptional = (key: string, checked: boolean) => {
    const next = new Set(enabledOptional);
    if (checked) next.add(key);
    else next.delete(key);
    onPatch({ enabledOptional: [...next] });
  };
  // The write toggle moves BOTH dials in ONE patch call: enabledOptional is
  // what actually grants write (internal/api/runs_create.go's
  // applyWorkspaceRequirements gates on it), and clearing readOnly alongside it
  // drops any stale force-read-only narrowing from earlier in this same wizard
  // session. Two SEPARATE onPatch calls would each recompute from the same
  // stale `sel` closure (onChange hasn't re-rendered between them) and the
  // second would silently clobber the first — so this must be a single call.
  const toggleOptionalWrite = (key: string, checked: boolean) => {
    const next = new Set(enabledOptional);
    if (checked) next.add(key);
    else next.delete(key);
    onPatch({ enabledOptional: [...next], readOnly: checked ? false : undefined });
  };

  const hasOptional =
    optionalRequirementsEnabled &&
    !!summary &&
    (summary.optionalSecrets.length > 0 ||
      summary.optionalHosts.length > 0 ||
      summary.optionalWriteKeys.length > 0);

  // Secrets first, hosts fill the remainder of the shared budget.
  const optionalSecrets = summary?.optionalSecrets ?? [];
  const optionalHosts = summary?.optionalHosts ?? [];
  const optionalCount = optionalSecrets.length + optionalHosts.length;
  const visibleOptionalSecrets = showAllOptional
    ? optionalSecrets
    : optionalSecrets.slice(0, OPTIONAL_PREVIEW_CAP);
  const visibleOptionalHosts = showAllOptional
    ? optionalHosts
    : optionalHosts.slice(0, Math.max(0, OPTIONAL_PREVIEW_CAP - visibleOptionalSecrets.length));

  return (
    <div className="rounded-lg border border-border p-2.5">
      <div className="flex items-center gap-2.5">
        <KindIcon className="size-4 shrink-0 text-muted-foreground" />
        <div className="min-w-0 flex-1">
          <div className="flex flex-wrap items-center gap-1.5">
            <span className="text-sm font-medium text-foreground">{ws?.name ?? sel.workspaceId}</span>
            {index === 0 && <Chip tone="primary">primary</Chip>}
            {ws && <Chip tone={statusTone(ws.status).tone}>{statusWord(ws.status)}</Chip>}
            {/* A multi-source workspace holds more than one kind — show what it
                holds ("2 dirs · 1 repo") instead of a single kind chip. */}
            {composition && (
              <Chip tone="neutral" mono>
                {composition}
              </Chip>
            )}
          </div>
          {ws && (
            <Mono className="text-[0.6875rem] text-muted-foreground" title={ws.source}>
              {ws.source}
            </Mono>
          )}
        </div>
        <button
          type="button"
          onClick={onRemove}
          className="shrink-0 text-muted-foreground transition-colors hover:text-foreground"
          aria-label={`Remove ${ws?.name ?? sel.workspaceId}`}
        >
          <X className="size-4" />
        </button>
      </div>

      {unstored.length > 0 && (
        <div className="mt-2.5 flex flex-wrap items-center gap-2 rounded-md border border-warning/40 bg-warning-subtle px-2.5 py-1.5">
          <TriangleAlert className="size-3.5 shrink-0 text-warning" aria-hidden="true" />
          <span className="text-[0.6875rem] text-warning">
            {unstored.length} secret{unstored.length > 1 ? "s" : ""} it requires{" "}
            {unstored.length > 1 ? "aren't" : "isn't"} stored
          </span>
          <span className="flex-1" />
          {/* Plain <a>, not react-router's <Link>: this picker is also reachable
              from the manual wizard's Basics step, which does not guarantee a
              Router ancestor in every test/host context. A full navigation to
              another top-level screen is a reasonable price for that safety. */}
          <Button asChild size="sm" variant="outline" className="h-6 px-2 text-[0.6875rem]">
            <a href="/secrets">Add secret</a>
          </Button>
          <Button asChild size="sm" variant="ghost" className="h-6 gap-1 px-2 text-[0.6875rem]">
            <a href="/workspaces">
              Open <ArrowUpRight className="size-3" />
            </a>
          </Button>
        </div>
      )}

      <div className="mt-2.5 space-y-2.5 border-t border-border pl-[26px] pt-2.5">
          {ws && summary && (
            <div className="space-y-1.5">
              <div className="flex items-center gap-2">
                <span className="text-xs text-foreground">
                  <strong className="font-semibold">Comes with:</strong> {comesWithLine(ws, sel)}
                </span>
                <button
                  type="button"
                  onClick={() => setShowRequired((v) => !v)}
                  className="text-[0.6875rem] font-medium text-primary"
                >
                  {showRequired ? "hide" : "show"}
                </button>
              </div>
              {showRequired && (
                <div className="space-y-1 rounded-md border border-border bg-surface-2/40 p-2">
                  {summary.requiredSecrets.map((n) => (
                    <div key={n} className="flex items-center gap-2">
                      <Mono className="text-[0.6875rem] text-foreground">{n}</Mono>
                      <span className="text-[0.6875rem] text-muted-foreground">secret</span>
                      {/* TRUST BOUNDARY (wizard-types.ts's secretAutoGrants): a
                          scan_seeded row never auto-mints a grant, Required or
                          not — never let this list read as "you get this
                          automatically" for one of those. */}
                      {!secretAutoGrants(ws, n) && (
                        <span className="text-[0.6875rem] text-warning" title={C.UNMET_OK}>
                          won&apos;t auto-grant
                        </span>
                      )}
                    </div>
                  ))}
                  {summary.requiredHosts.map((h) => (
                    <div key={h} className="flex items-center gap-2">
                      <Mono className="text-[0.6875rem] text-foreground">{h}</Mono>
                      <span className="text-[0.6875rem] text-muted-foreground">egress</span>
                    </div>
                  ))}
                  <div className="text-[0.6875rem] text-muted-foreground">
                    {ws.kind === "local_dir"
                      ? summary.requiredWrite
                        ? "write access to the directory"
                        : "read-only mount"
                      : "cloned fresh · nothing on this machine is touched"}
                  </div>
                  {summary.requiredSecrets.length > 0 && (
                    <p className="text-[0.6875rem] leading-snug text-muted-foreground">
                      Required secrets are visible here, not re-prompted — trim them on the workspace
                      page, not per run.
                    </p>
                  )}
                </div>
              )}
            </div>
          )}

          {ws && hasOptional && summary && (
            <div className="space-y-1.5">
              <span className="text-xs text-foreground">
                <strong className="font-semibold">Available if you need it:</strong>
              </span>
              <div className="flex flex-wrap items-center gap-4">
                {visibleOptionalSecrets.map((e: RequirementEntry) => (
                  <OptionalCheckbox
                    key={e.key}
                    id={`opt-${sel.workspaceId}-${e.key}`}
                    label={e.name}
                    sub="(secret)"
                    checked={enabledOptional.has(e.key)}
                    onCheckedChange={(c) => toggleOptional(e.key, c)}
                    // TRUST BOUNDARY (wizard-types.ts's secretAutoGrants): a
                    // scan_seeded optional secret NEVER auto-grants — ticking
                    // this box is a no-op server-side (runs_create.go's
                    // applyWorkspaceRequirements skips anything but
                    // operator_set). Same caveat the Required list already
                    // carries, now applied here too — regardless of level.
                    warnUnmetOk={!secretAutoGrants(ws, e.name)}
                  />
                ))}
                {visibleOptionalHosts.map((e: RequirementEntry) => (
                  <OptionalCheckbox
                    key={e.key}
                    id={`opt-${sel.workspaceId}-${e.key}`}
                    label={e.name}
                    sub="(egress)"
                    checked={enabledOptional.has(e.key)}
                    onCheckedChange={(c) => toggleOptional(e.key, c)}
                  />
                ))}
                {summary.optionalWriteKeys.map((key) => (
                  <div key={key} className="flex items-center gap-2">
                    <Switch
                      id={`opt-${sel.workspaceId}-write`}
                      checked={enabledOptional.has(key)}
                      onCheckedChange={(c) => toggleOptionalWrite(key, c)}
                    />
                    <Label
                      htmlFor={`opt-${sel.workspaceId}-write`}
                      className="text-[0.6875rem] font-normal text-muted-foreground"
                    >
                      write to the directory
                    </Label>
                  </div>
                ))}
              </div>
              {!showAllOptional && optionalCount > OPTIONAL_PREVIEW_CAP && (
                <button
                  type="button"
                  onClick={() => setShowAllOptional(true)}
                  className="text-[0.6875rem] font-medium text-primary"
                >
                  Show all {optionalCount}
                </button>
              )}
            </div>
          )}

          {/* Write mode: a Required write is a fact, not a per-run choice — a
              summary chip, never a toggle (the optional case above IS the
              toggle). Repos are never writable at all — cloned fresh, so
              there's nothing on the host to protect. */}
          {summary?.requiredWrite && ws?.kind === "local_dir" && (
            <Chip tone="warning">writable — every run</Chip>
          )}
          {ws?.kind === "repo" && (
            <p className="text-[0.6875rem] leading-snug text-muted-foreground">{C.REPO_RO}</p>
          )}

          <div className="flex items-end gap-3 border-t border-border pt-2.5">
            <div className="flex-1 space-y-1">
              <Label
                htmlFor={`ws-target-${sel.workspaceId}`}
                className="text-[0.6875rem] font-normal text-muted-foreground"
              >
                Target override (optional)
              </Label>
              <Input
                id={`ws-target-${sel.workspaceId}`}
                placeholder={
                  ws?.default_target || (ws?.kind === "repo" ? "~/work/<repo>" : "/home/agent/work")
                }
                value={sel.target ?? ""}
                onChange={(e) => onPatch({ target: e.target.value })}
                className="font-mono text-xs"
              />
            </div>
          </div>
        </div>
    </div>
  );
}

function WorkspaceCombobox({
  workspaces,
  loading,
  onSelect,
  onAddWorkspace,
  open,
  onOpenChange,
}: {
  workspaces: Workspace[];
  loading: boolean;
  onSelect: (id: string) => void;
  onAddWorkspace: () => void;
  open: boolean;
  onOpenChange: (o: boolean) => void;
}) {
  return (
    <Popover open={open} onOpenChange={onOpenChange}>
      <PopoverTrigger asChild>
        <Button
          type="button"
          variant="outline"
          role="combobox"
          // A combobox does NOT take its accessible name from contents (ARIA), so
          // without this the control is nameless to screen readers and locators.
          aria-label="Add a workspace"
          aria-expanded={open}
          className="w-64 justify-between"
        >
          <span className="text-muted-foreground">
            {loading ? "Loading workspaces…" : "Add a workspace…"}
          </span>
          <ChevronsUpDown className="size-4 opacity-50" />
        </Button>
      </PopoverTrigger>
      <PopoverContent className="w-72 p-0" align="start">
        <Command>
          <CommandInput placeholder="Search onboarded workspaces…" />
          <CommandList>
            <CommandEmpty>
              {loading ? (
                "Loading…"
              ) : (
                <div className="flex flex-col items-center gap-2 px-2 py-1 text-center">
                  <span className="text-xs text-muted-foreground">
                    No workspaces added yet — runs can only attach what exists here.
                  </span>
                  <Button
                    type="button"
                    size="sm"
                    onClick={() => {
                      onOpenChange(false);
                      onAddWorkspace();
                    }}
                  >
                    <Plus className="size-4" /> Add a workspace
                  </Button>
                </div>
              )}
            </CommandEmpty>
            <CommandGroup>
              {workspaces.map((w) => (
                <CommandItem
                  key={w.id}
                  value={`${w.name} ${w.source}`}
                  onSelect={() => {
                    onSelect(w.id);
                    onOpenChange(false);
                  }}
                >
                  <div className="flex min-w-0 flex-col">
                    <span className="truncate text-sm">{w.name}</span>
                    <span className="truncate font-mono text-[0.6875rem] text-muted-foreground">
                      {w.source}
                    </span>
                  </div>
                </CommandItem>
              ))}
            </CommandGroup>
          </CommandList>
        </Command>
      </PopoverContent>
    </Popover>
  );
}
