/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Onboarded-workspace multi-select for the New Run wizard's Basics step.
// Composed from the SAME primitives the rest of the wizard already uses — a
// search combobox (mirrors StepAccess's SecretCombobox), a removable-chip list
// (mirrors DomainPillList), and a disclosed per-item config card (mirrors
// StepAccess's AuthOption) — rather than inventing new interaction patterns.
import * as React from "react";
import { ChevronsUpDown, FolderGit2, FolderOpen, Plus, X } from "lucide-react";
import { Button } from "../../ui/button";
import { Input } from "../../ui/input";
import { Label } from "../../ui/label";
import { Switch } from "../../ui/switch";
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
import type { Workspace, WorkspaceSelection } from "../../../lib/types";
// Reuse the single widened status vocabulary so this picker can't drift from the
// /workspaces screen (and covers every import-flow status, not just three).
import { STATUS_TONE, STATUS_LABEL } from "../workspaces";

export function WorkspacePicker({
  selections,
  onChange,
  workspaces,
  loading,
  onAddWorkspace,
}: {
  selections: WorkspaceSelection[];
  onChange: (s: WorkspaceSelection[]) => void;
  workspaces: Workspace[];
  loading: boolean;
  onAddWorkspace: () => void;
}) {
  const byId = new Map(workspaces.map((w) => [w.id, w]));
  const selectedIds = new Set(selections.map((s) => s.workspaceId));
  const available = workspaces.filter((w) => !selectedIds.has(w.id));

  const add = (id: string) => onChange([...selections, { workspaceId: id }]);
  const remove = (id: string) => onChange(selections.filter((s) => s.workspaceId !== id));
  const patch = (id: string, p: Partial<WorkspaceSelection>) =>
    onChange(selections.map((s) => (s.workspaceId === id ? { ...s, ...p } : s)));

  return (
    <div className="space-y-3">
      <div className="flex items-center gap-2">
        <WorkspaceCombobox workspaces={available} loading={loading} onSelect={add} />
        <Button type="button" variant="ghost" size="sm" onClick={onAddWorkspace}>
          <Plus className="size-4" /> Add workspace
        </Button>
      </div>

      {selections.length === 0 ? (
        <p className="text-[11px] text-muted-foreground">
          No workspace selected yet. Only onboarded local directories and repos can be attached —
          use "Add workspace" to onboard a new one.
        </p>
      ) : (
        <div className="space-y-2">
          {selections.map((sel, i) => {
            const w = byId.get(sel.workspaceId);
            return (
              <div key={sel.workspaceId} className="rounded-lg border border-border p-2.5">
                <div className="flex items-center gap-2.5">
                  {w?.kind === "repo" ? (
                    <FolderGit2 className="size-4 shrink-0 text-muted-foreground" />
                  ) : (
                    <FolderOpen className="size-4 shrink-0 text-muted-foreground" />
                  )}
                  <div className="min-w-0 flex-1">
                    <div className="flex items-center gap-1.5">
                      <span className="text-sm font-medium text-foreground">
                        {w?.name ?? sel.workspaceId}
                      </span>
                      {i === 0 && <Chip tone="primary">primary</Chip>}
                      {w && <Chip tone={STATUS_TONE[w.status]}>{STATUS_LABEL[w.status]}</Chip>}
                    </div>
                    {w && (
                      <Mono className="text-[11px] text-muted-foreground" title={w.source}>
                        {w.source}
                      </Mono>
                    )}
                  </div>
                  <button
                    type="button"
                    onClick={() => remove(sel.workspaceId)}
                    className="shrink-0 text-muted-foreground transition-colors hover:text-foreground"
                    aria-label={`Remove ${w?.name ?? sel.workspaceId}`}
                  >
                    <X className="size-4" />
                  </button>
                </div>
                <div className="mt-2.5 flex items-end gap-3 border-t border-border pl-[26px] pt-2.5">
                  <div className="flex-1 space-y-1">
                    <Label
                      htmlFor={`ws-target-${sel.workspaceId}`}
                      className="text-[11px] font-normal text-muted-foreground"
                    >
                      Target override (optional)
                    </Label>
                    <Input
                      id={`ws-target-${sel.workspaceId}`}
                      placeholder={
                        w?.default_target || (w?.kind === "repo" ? "~/work/<repo>" : "/home/agent/work")
                      }
                      value={sel.target ?? ""}
                      onChange={(e) => patch(sel.workspaceId, { target: e.target.value })}
                      className="font-mono text-xs"
                    />
                  </div>
                  {w?.kind !== "repo" && (
                    <div className="flex items-center gap-2 pb-1.5">
                      <Switch
                        id={`ws-ro-${sel.workspaceId}`}
                        checked={!!sel.readOnly}
                        onCheckedChange={(c) => patch(sel.workspaceId, { readOnly: c })}
                      />
                      <Label
                        htmlFor={`ws-ro-${sel.workspaceId}`}
                        className="text-[11px] font-normal text-muted-foreground"
                      >
                        Read-only
                      </Label>
                    </div>
                  )}
                </div>
              </div>
            );
          })}
        </div>
      )}
    </div>
  );
}

function WorkspaceCombobox({
  workspaces,
  loading,
  onSelect,
}: {
  workspaces: Workspace[];
  loading: boolean;
  onSelect: (id: string) => void;
}) {
  const [open, setOpen] = React.useState(false);
  return (
    <Popover open={open} onOpenChange={setOpen}>
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
              {loading ? "Loading…" : "No onboarded workspaces available."}
            </CommandEmpty>
            <CommandGroup>
              {workspaces.map((w) => (
                <CommandItem
                  key={w.id}
                  value={`${w.name} ${w.source}`}
                  onSelect={() => {
                    onSelect(w.id);
                    setOpen(false);
                  }}
                >
                  <div className="flex min-w-0 flex-col">
                    <span className="truncate text-sm">{w.name}</span>
                    <span className="truncate font-mono text-[11px] text-muted-foreground">
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
