/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The ONE "Add workspace" dialog — replaces the retired 7-step
// workspace-wizard/ directory (Sources -> Base image -> Integrations -> Build
// -> Requirements -> Verify -> Done). A workspace is usable the instant it's
// created (internal/api/runs_create.go: "onboarding is a convenience, not a
// gate") — so this dialog does exactly one thing: POST /workspaces, then let
// the caller decide what happens next (navigate to the detail page, or
// attach it to a run already being built). No scan call, no build call, no
// progress state.
import * as React from "react";
import { ChevronDown } from "lucide-react";
import { toast } from "sonner";
import { Button } from "../ui/button";
import { Input } from "../ui/input";
import { Checkbox } from "../ui/checkbox";
import { cn } from "../ui/utils";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "../ui/dialog";
import { Field, OptionCard } from "../wardyn/form-primitives";
import { OPERATOR_ONLY_REASON } from "../wardyn/copy";
import { useOperator } from "../wardyn/operator-context";
import { getErrorMessage } from "../../lib/format";
import { workspaces as workspacesApi } from "../../lib/api/workspaces";
import { useK8sRunner } from "../../lib/use-k8s-runner";
import type { Workspace, WorkspaceSourceInput } from "../../lib/types";

const DEFAULT_TARGET = "/home/agent/work";

type SourceKind = "repo" | "local_dir" | "ephemeral";
type ImageChoice = "devcontainer" | "pinned" | "standard";

// Best-effort basename off a repo slug/URL or a local path — good enough to
// pre-fill Name; the operator can always type over it.
function baseNameFrom(value: string): string {
  const cleaned = value.trim().replace(/\.git$/, "").replace(/\/+$/, "");
  const parts = cleaned.split(/[/:]/).filter(Boolean);
  return parts[parts.length - 1] ?? "";
}

function deriveName(kind: SourceKind, sourceValue: string): string {
  return kind === "ephemeral" ? "" : baseNameFrom(sourceValue);
}

// Name is never required — it derives, or falls back to a numbered
// workspace-N that doesn't collide with an existing row.
function fallbackName(existingNames: string[]): string {
  const taken = new Set(existingNames.map((n) => n.toLowerCase()));
  let i = 1;
  while (taken.has(`workspace-${i}`)) i++;
  return `workspace-${i}`;
}

// A small collapsed/expanded disclosure — nothing like it existed in the
// codebase (ui/accordion, ui/collapsible) to reuse.
function Disclosure({
  summary,
  children,
}: {
  summary: React.ReactNode;
  children: React.ReactNode;
}) {
  const [open, setOpen] = React.useState(false);
  return (
    <div className="rounded-lg border border-border">
      <button
        type="button"
        onClick={() => setOpen((o) => !o)}
        aria-expanded={open}
        className="flex w-full items-center justify-between gap-2 px-3 py-2 text-left"
      >
        <span className="text-xs font-medium text-foreground">Advanced</span>
        <span className="flex items-center gap-1.5 text-[0.6875rem] text-muted-foreground">
          {!open && <span className="font-mono">{summary}</span>}
          <ChevronDown className={cn("size-3.5 transition-transform", open && "rotate-180")} />
        </span>
      </button>
      {open && <div className="space-y-3 border-t border-border p-3">{children}</div>}
    </div>
  );
}

export function AddWorkspaceDialog({
  existingNames,
  onClose,
  onCreated,
}: {
  /** Existing workspace names — feeds the workspace-N fallback + avoids collisions. */
  existingNames: string[];
  onClose: () => void;
  /** Fired once POST /workspaces succeeds. The caller decides what happens
   *  next (navigate to the detail page, or attach it to a run being built). */
  onCreated: (workspace: Workspace) => void;
}) {
  // Every mutation this dialog makes is operator-only server-side. It mounts
  // from more than one place (the Workspaces list, and the New Run wizard's
  // "Add a workspace" — neither of which gates a MEMBER out client-side on
  // its own), so it carries its own check rather than trusting every caller
  // to have one.
  const operator = useOperator();

  const [kind, setKind] = React.useState<SourceKind>("repo");
  const k8s = useK8sRunner();
  // If the runner turns out to be k8s while local_dir is selected (the probe
  // resolves after mount), fall back rather than leaving an unsubmittable kind
  // selected with no visible control.
  React.useEffect(() => {
    if (k8s && kind === "local_dir") setKind("repo");
  }, [k8s, kind]);
  const [sourceValue, setSourceValue] = React.useState("");
  const [name, setName] = React.useState("");
  const [nameTouched, setNameTouched] = React.useState(false);
  const [branch, setBranch] = React.useState("");
  const [imageChoice, setImageChoice] = React.useState<ImageChoice>("standard");
  const [pinnedRef, setPinnedRef] = React.useState("");
  const [mountPath, setMountPath] = React.useState(DEFAULT_TARGET);
  const [writable, setWritable] = React.useState(false);
  const [submitting, setSubmitting] = React.useState(false);

  // Name auto-derives from the source and stops tracking the instant the
  // operator types their own.
  const derived = deriveName(kind, sourceValue);
  React.useEffect(() => {
    if (!nameTouched) setName(derived);
  }, [derived, nameTouched]);

  const sourceRequired = kind !== "ephemeral";
  const canSubmit = !sourceRequired || sourceValue.trim().length > 0;

  const mountPathOrDefault = mountPath.trim() || DEFAULT_TARGET;
  const summaryImage =
    imageChoice === "devcontainer"
      ? "devcontainer.json"
      : imageChoice === "pinned"
        ? pinnedRef.trim() || "pinned image"
        : "standard sandbox image";

  const submit = async () => {
    if (!canSubmit || submitting) return;
    setSubmitting(true);
    const effectiveName = name.trim() || fallbackName(existingNames);
    const target = mountPathOrDefault;
    const sourceInput: WorkspaceSourceInput =
      kind === "repo"
        ? { type: "repo", source: sourceValue.trim(), ref: branch.trim() || undefined, target, writable: writable || undefined }
        : kind === "local_dir"
          ? { type: "local_dir", path: sourceValue.trim(), target, writable: writable || undefined }
          : { type: "ephemeral", target, writable: writable || undefined };
    const base_image =
      imageChoice === "devcontainer"
        ? ({ kind: "recommended" } as const)
        : imageChoice === "pinned" && pinnedRef.trim()
          ? ({ kind: "byo" as const, image: pinnedRef.trim() })
          : undefined;
    try {
      const created = await workspacesApi.createWorkspace({
        name: effectiveName,
        sources: [sourceInput],
        ...(base_image ? { base_image } : {}),
      });
      onClose();
      onCreated(created);
    } catch (e) {
      toast.error("Failed to add workspace", { description: getErrorMessage(e) });
    } finally {
      setSubmitting(false);
    }
  };

  if (!operator) {
    return (
      <Dialog open onOpenChange={(o) => !o && onClose()}>
        <DialogContent className="sm:max-w-md">
          <DialogHeader>
            <DialogTitle>Add workspace</DialogTitle>
            <DialogDescription>{OPERATOR_ONLY_REASON}</DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <Button type="button" onClick={onClose}>
              Close
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    );
  }

  return (
    <Dialog open onOpenChange={(o) => !o && onClose()}>
      <DialogContent className="sm:max-w-lg">
        <DialogHeader>
          <DialogTitle>Add workspace</DialogTitle>
          <DialogDescription>
            A workspace is a repo or directory a run can attach. You can change everything later.
          </DialogDescription>
        </DialogHeader>

        <div className="space-y-4">
          {/* A k8s control plane runs sandboxes as pods, where a host-directory
              mount is structurally impossible (internal/runner/substrate) — so
              Local directory is not offered at all rather than accepted and then
              failing at launch. */}
          <div
            className={cn("grid gap-2", k8s ? "grid-cols-2" : "grid-cols-3")}
            role="radiogroup"
            aria-label="Source"
          >
            <OptionCard selected={kind === "repo"} onClick={() => setKind("repo")} title="Repository" />
            {!k8s && (
              <OptionCard
                selected={kind === "local_dir"}
                onClick={() => setKind("local_dir")}
                title="Local directory"
              />
            )}
            <OptionCard selected={kind === "ephemeral"} onClick={() => setKind("ephemeral")} title="Empty" />
          </div>

          {kind === "repo" && (
            <Field
              label="Repository URL"
              htmlFor="aw-source"
              hint="Cloned into the sandbox when a run starts. Private repos use the credential from Settings → Git host."
            >
              <Input
                id="aw-source"
                value={sourceValue}
                onChange={(e) => setSourceValue(e.target.value)}
                placeholder="acme/payments-service"
              />
            </Field>
          )}
          {kind === "local_dir" && (
            <Field label="Path on this host" htmlFor="aw-source" hint="Mounted from this machine into the sandbox.">
              <Input
                id="aw-source"
                value={sourceValue}
                onChange={(e) => setSourceValue(e.target.value)}
                placeholder="/home/me/projects/payments"
                className="font-mono"
              />
            </Field>
          )}
          {kind === "ephemeral" && (
            <p className="text-[0.6875rem] leading-snug text-muted-foreground">
              An empty scratch directory at <span className="font-mono">/home/agent/work</span>, discarded when the
              run ends. Use this when the container is the point.
            </p>
          )}

          <div className="grid grid-cols-2 gap-3">
            <Field label="Name" htmlFor="aw-name">
              <Input
                id="aw-name"
                value={name}
                onChange={(e) => {
                  setNameTouched(true);
                  setName(e.target.value);
                }}
                placeholder="workspace-1"
              />
            </Field>
            <Field label="Branch" htmlFor="aw-branch">
              <Input
                id="aw-branch"
                value={branch}
                onChange={(e) => setBranch(e.target.value)}
                placeholder="default branch"
              />
            </Field>
          </div>

          <Disclosure summary={`${summaryImage}, ${mountPathOrDefault}`}>
            <div className="space-y-2" role="radiogroup" aria-label="Container image">
              <OptionCard
                selected={imageChoice === "devcontainer"}
                onClick={() => setImageChoice("devcontainer")}
                title="devcontainer.json"
                hint="Built exactly as written in this repo — we don't modify it. Falls back to the standard sandbox image if there isn't one."
              />
              <OptionCard
                selected={imageChoice === "standard"}
                onClick={() => setImageChoice("standard")}
                title="Standard sandbox image"
                hint="Wardyn's baseline container — every tool a run needs, nothing project-specific."
              />
              <OptionCard
                selected={imageChoice === "pinned"}
                onClick={() => setImageChoice("pinned")}
                title="Pinned image ref"
                hint="An exact image Wardyn pulls as given — no build."
              />
              {imageChoice === "pinned" && (
                <Input
                  aria-label="Image ref"
                  value={pinnedRef}
                  onChange={(e) => setPinnedRef(e.target.value)}
                  placeholder="ghcr.io/acme/dev@sha256:ab12…"
                  className="font-mono text-xs"
                />
              )}
            </div>

            <Field label="Mount path" htmlFor="aw-mount">
              <Input
                id="aw-mount"
                value={mountPath}
                onChange={(e) => setMountPath(e.target.value)}
                className="font-mono text-xs"
              />
            </Field>

            <label htmlFor="aw-writable" className="flex items-center gap-2 text-xs text-foreground">
              <Checkbox
                id="aw-writable"
                checked={writable}
                onCheckedChange={(v) => setWritable(v === true)}
              />
              Allow writes to this directory
            </label>
          </Disclosure>
        </div>

        <DialogFooter>
          <Button type="button" variant="outline" onClick={onClose}>
            Cancel
          </Button>
          <Button type="button" disabled={!canSubmit || submitting} onClick={() => void submit()}>
            Add workspace
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
