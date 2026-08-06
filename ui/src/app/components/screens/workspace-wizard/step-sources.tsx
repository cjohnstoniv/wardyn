/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Step ① Sources — mock frames V2S1Fresh / V2S1Composed / V2S1SshGate
// (mockup/wardyn-workspaces.js's AddWorkspaceWizardV2, "---------- ① Sources
// ----------" section). Name + a source list (local dir / repo / ephemeral,
// multiples of the same type allowed) + "Add source". The wizard always holds
// the state; this component is a pure controlled view over it.
import * as React from "react";
import { AlertTriangle, FolderGit2, FolderOpen, Hourglass, KeyRound, X } from "lucide-react";
import { sourcesApi } from "../../../lib/api/sources";
import type { Source } from "../../../lib/types";
import { Button } from "../../ui/button";
import { Input } from "../../ui/input";
import { Field } from "../new-run/step-shell";
import { Chip } from "../../wardyn/primitives";
import { Mono } from "../../wardyn/code-block";
import { AddSecretDialog } from "../secrets";
import { deriveProviders, LANE_META, slugHost, type Lane } from "../../../lib/scm-provider";
import { C, V2C } from "../../../lib/workspace-copy";
import {
  defaultTargetFor,
  isRemovable,
  isSshRemote,
  parseRepoSource,
  type SourceRow,
  type WorkspaceSourceKind,
} from "./wizard-types";

// Exported so step-base-image.tsx's Phase A rows use the same icon/title per
// source kind instead of a second copy of this table.
export const SOURCE_META: Record<WorkspaceSourceKind, { title: string; desc: string; Icon: React.ElementType }> = {
  local_dir: { title: "Local directory", desc: "Mounted from this machine", Icon: FolderOpen },
  repo: { title: "Repository", desc: "Cloned fresh for every run", Icon: FolderGit2 },
  ephemeral: { title: "Ephemeral directory", desc: "Scratch space, discarded after the run", Icon: Hourglass },
};
const ADD_SOURCE_TYPES: WorkspaceSourceKind[] = ["local_dir", "repo", "ephemeral"];

interface CredTarget {
  name: string;
  host: string;
  lane: Lane;
}

// The repo row's credential/SSH-gate block — derives lanes with deriveProviders
// (scm-provider.ts) scoped to just this row's host, so a lane the operator
// stores for OTHER rows never leaks into a host that doesn't have it.
function AccessBlock({
  host,
  sshRemote,
  secretNames,
  githubApp,
  onOpenCredential,
}: {
  host: string;
  sshRemote: boolean;
  secretNames: string[];
  githubApp: boolean;
  onOpenCredential: (target: CredTarget) => void;
}) {
  const lanes = deriveProviders(secretNames, [host], githubApp).find((r) => r.host === host)?.lanes ?? [];
  const sshGate = sshRemote && !lanes.includes("ssh");
  const slug = slugHost(host);

  if (sshGate) {
    return (
      <div className="space-y-2 rounded-lg border border-danger/40 bg-danger-subtle p-3" data-testid="ssh-gate">
        <div className="flex items-center gap-2 text-danger">
          <AlertTriangle className="size-4 shrink-0" />
          <span className="text-xs font-semibold">SSH key needed first</span>
        </div>
        <p className="text-[0.6875rem] leading-snug text-danger/90">{C.SSH_GATE}</p>
        <Button
          type="button"
          size="sm"
          variant="outline"
          onClick={() => onOpenCredential({ name: `ssh-key-${slug}`, host, lane: "ssh" })}
        >
          <KeyRound className="size-3.5" /> Add SSH key
        </Button>
      </div>
    );
  }

  return (
    <div className="space-y-1.5 rounded-lg border border-border p-3">
      <div className="flex flex-wrap items-center gap-2">
        <span className="text-[0.6875rem] font-semibold uppercase tracking-wide text-muted-foreground">Access</span>
        {lanes.length === 0 ? (
          <Chip tone="neutral">no credential</Chip>
        ) : (
          lanes.map((lane) => (
            <Chip key={lane} tone={LANE_META[lane].tone} title={LANE_META[lane].tooltip}>
              {LANE_META[lane].label}
            </Chip>
          ))
        )}
        <Mono className="text-foreground">{host}</Mono>
      </div>
      {/* Neutral-informational when there's no credential at all — never a warning: a
          public repo clones fine without one. */}
      {lanes.length === 0 ? (
        <>
          <p className="text-[0.6875rem] leading-snug text-muted-foreground">
            No credential stored for {host}. Public repos clone without one — a private repo&apos;s clone will
            fail at scan time.
          </p>
          <Button
            type="button"
            size="sm"
            variant="outline"
            onClick={() => onOpenCredential({ name: `git-pat-${slug}`, host, lane: "pat" })}
          >
            <KeyRound className="size-3.5" /> Add credential
          </Button>
        </>
      ) : (
        lanes.map((lane) => (
          <p key={lane} className="text-[0.6875rem] leading-snug text-muted-foreground">
            {LANE_META[lane].tooltip}
          </p>
        ))
      )}
    </div>
  );
}

function SourceRowCard({
  row,
  rows,
  onUpdate,
  onRemove,
  secretNames,
  githubApp,
  onOpenCredential,
}: {
  row: SourceRow;
  rows: SourceRow[];
  onUpdate: (patch: Partial<SourceRow>) => void;
  onRemove: () => void;
  secretNames: string[];
  githubApp: boolean;
  onOpenCredential: (target: CredTarget) => void;
}) {
  const meta = SOURCE_META[row.type];
  const removable = isRemovable(row, rows);
  const target = defaultTargetFor(row, rows);
  const info = row.type === "repo" ? parseRepoSource(row.source) : null;
  const targetHint = row.target.trim() ? "Where the sandbox sees it." : `Where the sandbox sees it — defaults to ${target}.`;

  return (
    <div className="space-y-3 rounded-lg border border-border p-3" data-testid="source-row">
      <div className="flex items-start gap-2.5">
        <meta.Icon className="mt-0.5 size-4 shrink-0 text-muted-foreground" aria-hidden />
        <div className="min-w-0 flex-1">
          <p className="text-sm font-medium text-foreground">{meta.title}</p>
          <p className="text-[0.6875rem] leading-snug text-muted-foreground">
            {row.type === "ephemeral" && row.seeded ? V2C.FLOOR : meta.desc}
          </p>
        </div>
        <Button
          type="button"
          size="sm"
          variant="ghost"
          className="size-7 shrink-0 p-0 text-muted-foreground"
          disabled={!removable}
          title={removable ? undefined : "Every workspace has at least one source"}
          aria-label={`Remove ${meta.title.toLowerCase()}`}
          onClick={onRemove}
        >
          <X className="size-3.5" />
        </Button>
      </div>

      <div className="space-y-3 border-t border-border pt-3">
        {row.type === "local_dir" && (
          <>
            <Field label="Path on this host" htmlFor={`${row.id}-path`} hint={C.DIR_HELPER}>
              <Input
                id={`${row.id}-path`}
                value={row.path}
                placeholder="/home/me/projects/payments"
                className="font-mono"
                onChange={(e) => onUpdate({ path: e.target.value })}
              />
            </Field>
            <Field label="Mount target" htmlFor={`${row.id}-target`} hint={targetHint}>
              <Input
                id={`${row.id}-target`}
                value={row.target}
                placeholder={target}
                className="font-mono"
                onChange={(e) => onUpdate({ target: e.target.value })}
              />
            </Field>
            {/* Read-only is the safe default (WorkspaceSource.Writable).
                Without this opt-in an imported workspace can never be
                written, so an agent cannot install deps, build, or edit a
                file — the whole point of onboarding it. Granting it is
                real: changes land on the host. This is the only UI that can
                set it (the legacy AddWorkspaceDialog had the equivalent
                checkbox before it was retired). */}
            <label className="flex items-start gap-2.5 rounded-lg border border-border p-3 text-xs">
              <input
                type="checkbox"
                checked={row.writable}
                onChange={(e) => onUpdate({ writable: e.target.checked })}
                className="mt-0.5 size-3.5 shrink-0 accent-primary"
              />
              <span>
                <span className="font-medium text-foreground">Let agents write to this directory</span>
                <span className="block text-[0.6875rem] leading-snug text-muted-foreground">
                  Required to install dependencies, build, or have an agent change code. Leave
                  unticked and the workspace mounts read-only — <span className="font-mono">install</span>{" "}
                  and <span className="font-mono">build</span> steps will fail.
                </span>
              </span>
            </label>
            {row.writable && (
              <p className="rounded-md bg-warning-subtle px-2 py-1.5 text-[0.6875rem] leading-snug text-warning">
                The agent&apos;s changes persist to the host directory{" "}
                <span className="font-mono">{row.path.trim() || "…"}</span>. Point this at a
                disposable clone, not a working tree you care about. If your Docker runtime
                runs inside a VM (Rancher Desktop / Colima / Docker Desktop on macOS), the
                stronger barriers may still deny writes here regardless of host permissions —
                see docs/adoption.
              </p>
            )}
          </>
        )}

        {row.type === "repo" && (
          <>
            <Field
              label="Source"
              htmlFor={`${row.id}-source`}
              hint="A slug (acme/payments-service), an https URL, or git@host:org/repo.git. Cloned fresh into the sandbox for every run — nothing on this machine is touched."
            >
              <Input
                id={`${row.id}-source`}
                value={row.source}
                placeholder="acme/payments-service"
                className="font-mono"
                onChange={(e) => onUpdate({ source: e.target.value })}
              />
            </Field>
            <div className="flex flex-wrap items-start gap-3">
              <Field label="Ref (optional)" htmlFor={`${row.id}-ref`} className="min-w-40 flex-1">
                <Input
                  id={`${row.id}-ref`}
                  value={row.ref}
                  placeholder="main"
                  className="font-mono"
                  onChange={(e) => onUpdate({ ref: e.target.value })}
                />
              </Field>
              <Field label="Clone target" htmlFor={`${row.id}-clone-target`} className="min-w-40 flex-1" hint={targetHint}>
                <Input
                  id={`${row.id}-clone-target`}
                  value={row.target}
                  placeholder={target}
                  className="font-mono"
                  onChange={(e) => onUpdate({ target: e.target.value })}
                />
              </Field>
            </div>
            {info && (
              <AccessBlock
                host={info.host}
                sshRemote={isSshRemote(row)}
                secretNames={secretNames}
                githubApp={githubApp}
                onOpenCredential={onOpenCredential}
              />
            )}
          </>
        )}

        {row.type === "ephemeral" && (
          <Field label="Mount target" htmlFor={`${row.id}-target`} hint={V2C.EPH_HELP}>
            <Input
              id={`${row.id}-target`}
              value={row.target}
              placeholder={target}
              className="font-mono"
              onChange={(e) => onUpdate({ target: e.target.value })}
            />
          </Field>
        )}
      </div>
    </div>
  );
}

export function StepSources({
  name,
  onNameChange,
  sources,
  onAddSource,
  onAttachLibrarySource,
  onUpdateSource,
  onRemoveSource,
  secretNames,
  githubApp,
  onSecretStored,
}: {
  name: string;
  onNameChange: (name: string) => void;
  sources: SourceRow[];
  onAddSource: (type: WorkspaceSourceKind) => void;
  onAttachLibrarySource: (src: Source) => void;
  onUpdateSource: (id: string, patch: Partial<SourceRow>) => void;
  onRemoveSource: (id: string) => void;
  secretNames: string[];
  githubApp: boolean;
  onSecretStored: (name: string) => void;
}) {
  const [credTarget, setCredTarget] = React.useState<CredTarget | null>(null);
  // The tier-1 library: attach an already-configured dir/repo in one click.
  // Best-effort fetch — an empty/failed library simply hides the section, the
  // type-a-new-one flow below is never blocked on it.
  const [library, setLibrary] = React.useState<Source[]>([]);
  React.useEffect(() => {
    let live = true;
    sourcesApi
      .listSources()
      .then((rows) => live && setLibrary(rows))
      .catch(() => {});
    return () => {
      live = false;
    };
  }, []);
  // Already-attached identities: hide their library rows (attach is idempotent
  // server-side, but offering a second attach of the same entry reads as a bug).
  const attachedKeys = new Set(
    sources.map((r) => (r.type === "repo" ? `repo:${r.source.trim().toLowerCase()}` : `dir:${r.path.trim()}`)),
  );
  const attachable = library.filter(
    (src) =>
      !attachedKeys.has(src.kind === "repo" ? `repo:${src.locator.toLowerCase()}` : `dir:${src.locator}`),
  );

  return (
    <div className="space-y-5">
      <Field label="Name" htmlFor="ws-name" required>
        <Input
          id="ws-name"
          value={name}
          required
          placeholder="e.g. payments"
          onChange={(e) => onNameChange(e.target.value)}
        />
      </Field>

      <div className="space-y-2">
        <p className="text-xs font-medium text-foreground">Sources</p>
        <div className="space-y-2">
          {sources.map((row) => (
            <SourceRowCard
              key={row.id}
              row={row}
              rows={sources}
              onUpdate={(patch) => onUpdateSource(row.id, patch)}
              onRemove={() => onRemoveSource(row.id)}
              secretNames={secretNames}
              githubApp={githubApp}
              onOpenCredential={setCredTarget}
            />
          ))}
        </div>
      </div>

      {attachable.length > 0 && (
        <div className="space-y-2" data-testid="library-attach">
          <p className="text-xs font-medium text-foreground">From your library</p>
          <p className="text-[0.6875rem] leading-snug text-muted-foreground">
            Already configured — attaching reuses the entry, its contract and its scan.
          </p>
          <div className="space-y-2">
            {attachable.map((src) => {
              const Icon = src.kind === "repo" ? FolderGit2 : FolderOpen;
              return (
                <div key={src.id} className="flex items-center gap-3 rounded-lg border border-border p-3">
                  <Icon className="size-4 shrink-0 text-muted-foreground" aria-hidden />
                  <span className="min-w-0 flex-1">
                    <span className="block truncate text-[0.8125rem] font-medium text-foreground">{src.name}</span>
                    <span className="block truncate font-mono text-[0.6875rem] text-muted-foreground">
                      {src.kind === "repo" ? "repo" : "dir"} · {src.locator}
                      {src.ref ? ` @${src.ref}` : ""}
                    </span>
                  </span>
                  <Button type="button" size="sm" variant="outline" onClick={() => onAttachLibrarySource(src)}>
                    Attach
                  </Button>
                </div>
              );
            })}
          </div>
        </div>
      )}

      <div className="space-y-2">
        <p className="text-xs font-medium text-foreground">Add source</p>
        <div className="grid grid-cols-1 gap-2 sm:grid-cols-3">
          {ADD_SOURCE_TYPES.map((type) => {
            const meta = SOURCE_META[type];
            return (
              <button
                key={type}
                type="button"
                aria-label={`Add ${meta.title}`}
                onClick={() => onAddSource(type)}
                className="rounded-lg border border-border p-3 text-left transition-colors hover:border-border-strong focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
              >
                <div className="flex items-center justify-between">
                  <meta.Icon className="size-4 text-muted-foreground" aria-hidden />
                  <span className="font-mono text-sm text-primary">+</span>
                </div>
                <p className="mt-1.5 text-[0.8125rem] font-medium text-foreground">{meta.title}</p>
                <p className="text-[0.6875rem] leading-snug text-muted-foreground">{meta.desc}</p>
              </button>
            );
          })}
        </div>
        <p className="text-[0.6875rem] leading-snug text-muted-foreground">
          New directories and repos join your library automatically — the next workspace attaches them
          from the list above.
        </p>
      </div>

      <AddSecretDialog
        open={!!credTarget}
        onOpenChange={(o) => {
          if (!o) setCredTarget(null);
        }}
        lockName
        host={credTarget?.host}
        lane={credTarget?.lane}
        initialName={credTarget?.name ?? ""}
        onSaved={(savedName) => {
          onSecretStored(savedName);
          setCredTarget(null);
        }}
      />
    </div>
  );
}
