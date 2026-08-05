/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Tier 2 — the BASE IMAGE catalog: shared, reusable images (registry ref /
// custom recipe / bring-your-own) referenced by any number of workspaces.
// "Recommended — built for this workspace" is deliberately NOT a row here: it
// is derived per workspace from its sources' merged profile. Deleting an
// image workspaces still use is a loud refusal naming them; the detach escape
// is honest rather than destructive — a detached workspace falls back to the
// derived recommended build.
import * as React from "react";
import { Box, Loader2, MoreHorizontal, Plus, Trash2 } from "lucide-react";
import { toast } from "sonner";
import { baseImagesApi } from "../../lib/api/sources";
import { HttpError } from "../../lib/api/core";
import { getErrorMessage } from "../../lib/format";
import type { BaseImageEntry, Workspace } from "../../lib/types";
import { Button } from "../ui/button";
import { Input } from "../ui/input";
import { Label } from "../ui/label";
import { RadioGroup, RadioGroupItem } from "../ui/radio-group";
import { Textarea } from "../ui/textarea";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "../ui/table";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "../ui/dialog";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuTrigger,
} from "../ui/dropdown-menu";
import { Mono } from "../wardyn/code-block";
import { Chip } from "../wardyn/primitives";
import { EmptyState, ErrorState, TableSkeleton } from "../wardyn/states";
import { OPERATOR_ONLY_REASON } from "../wardyn/copy";
import { useOperator } from "../wardyn/operator-context";

const KIND_LABEL: Record<BaseImageEntry["kind"], string> = {
  registry: "Registry image",
  custom: "Custom recipe",
  byo: "Bring your own",
};

// How many workspaces reference each catalog image (base_image_id).
export function imageUsage(workspaces: Workspace[]): Map<string, number> {
  const used = new Map<string, number>();
  for (const ws of workspaces) {
    if (ws.base_image_id) used.set(ws.base_image_id, (used.get(ws.base_image_id) ?? 0) + 1);
  }
  return used;
}

export function AddBaseImageDialog({
  open,
  onOpenChange,
  onSaved,
}: {
  open: boolean;
  onOpenChange: (o: boolean) => void;
  onSaved: (img: BaseImageEntry) => void;
}) {
  const operator = useOperator();
  const [kind, setKind] = React.useState<BaseImageEntry["kind"]>("registry");
  const [image, setImage] = React.useState("");
  const [name, setName] = React.useState("");
  const [steps, setSteps] = React.useState("");
  const [saving, setSaving] = React.useState(false);
  const [error, setError] = React.useState<string | null>(null);

  React.useEffect(() => {
    if (!open) return;
    setKind("registry");
    setImage("");
    setName("");
    setSteps("");
    setSaving(false);
    setError(null);
  }, [open]);

  const save = async () => {
    setSaving(true);
    setError(null);
    try {
      const img = await baseImagesApi.createBaseImage({
        kind,
        image: image.trim(),
        name: name.trim() || undefined,
        steps:
          kind === "custom"
            ? steps
                .split("\n")
                .map((l) => l.trim())
                .filter(Boolean)
            : undefined,
      });
      onSaved(img);
      onOpenChange(false);
      toast.success(`"${img.name}" is in your catalog`);
    } catch (e) {
      setError(getErrorMessage(e));
    } finally {
      setSaving(false);
    }
  };

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-lg">
        <DialogHeader>
          <DialogTitle>Add a base image</DialogTitle>
          <DialogDescription>
            Shared across workspaces. “Recommended” never lives here — it is built per workspace from
            what its sources need.
          </DialogDescription>
        </DialogHeader>
        <div className="space-y-3">
          <RadioGroup
            value={kind}
            onValueChange={(v) => setKind(v as BaseImageEntry["kind"])}
            className="flex flex-wrap gap-4"
          >
            {(Object.keys(KIND_LABEL) as BaseImageEntry["kind"][]).map((k) => (
              <label key={k} className="flex items-center gap-1.5 text-xs">
                <RadioGroupItem value={k} id={`img-kind-${k}`} />
                <Label htmlFor={`img-kind-${k}`} className="cursor-pointer font-normal">
                  {KIND_LABEL[k]}
                </Label>
              </label>
            ))}
          </RadioGroup>
          <div className="space-y-1.5">
            <Label htmlFor="img-ref">{kind === "custom" ? "Base image" : "Image ref"}</Label>
            <Input
              id="img-ref"
              value={image}
              onChange={(e) => setImage(e.target.value)}
              placeholder="ubuntu:24.04"
              className="font-mono"
              autoComplete="off"
            />
            {kind === "byo" && (
              <p className="text-[0.6875rem] leading-snug text-muted-foreground">
                Pulled and used as-is — Wardyn doesn’t inspect it or layer anything onto it.
              </p>
            )}
          </div>
          {kind === "custom" && (
            <div className="space-y-1.5">
              <Label htmlFor="img-steps">Build steps (one per line)</Label>
              <Textarea
                id="img-steps"
                value={steps}
                onChange={(e) => setSteps(e.target.value)}
                placeholder={"RUN apt-get update && apt-get install -y build-essential\nENV CGO_ENABLED=1"}
                className="min-h-24 font-mono text-xs"
              />
              <p className="text-[0.6875rem] leading-snug text-muted-foreground">
                Dockerfile RUN/ENV/ARG lines layered on the base.
              </p>
            </div>
          )}
          <div className="space-y-1.5">
            <Label htmlFor="img-name">Name (optional)</Label>
            <Input
              id="img-name"
              value={name}
              onChange={(e) => setName(e.target.value)}
              placeholder="go-build-base"
              autoComplete="off"
            />
          </div>
          {error && <p className="text-xs font-medium text-danger">{error}</p>}
          {!operator && <p className="text-xs font-medium text-warning">{OPERATOR_ONLY_REASON}</p>}
        </div>
        <DialogFooter>
          <Button variant="ghost" onClick={() => onOpenChange(false)}>
            Cancel
          </Button>
          <Button onClick={save} disabled={!operator || saving || !image.trim()}>
            {saving && <Loader2 className="size-4 animate-spin" />}
            Add to catalog
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

function DeleteBaseImageDialog({
  target,
  onOpenChange,
  onDeleted,
}: {
  target: BaseImageEntry | null;
  onOpenChange: (o: boolean) => void;
  onDeleted: () => void;
}) {
  const [busy, setBusy] = React.useState(false);
  const [inUseDetail, setInUseDetail] = React.useState<string | null>(null);

  React.useEffect(() => {
    setBusy(false);
    setInUseDetail(null);
  }, [target]);

  const attempt = async (force: boolean) => {
    if (!target) return;
    setBusy(true);
    try {
      await baseImagesApi.deleteBaseImage(target.id, force);
      toast.success(`"${target.name}" removed from the catalog`);
      onDeleted();
      onOpenChange(false);
    } catch (e) {
      if (!force && e instanceof HttpError && e.status === 409) {
        setInUseDetail(getErrorMessage(e));
      } else {
        toast.error("Failed to delete image", { description: getErrorMessage(e) });
      }
    } finally {
      setBusy(false);
    }
  };

  return (
    <Dialog open={!!target} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-lg">
        <DialogHeader>
          <DialogTitle>Delete “{target?.name}”?</DialogTitle>
          <DialogDescription>Removes the catalog entry.</DialogDescription>
        </DialogHeader>
        {inUseDetail && (
          <div className="space-y-1.5 rounded-lg border border-warning/30 bg-warning-subtle p-2.5">
            <p className="text-xs leading-snug text-warning">{inUseDetail}</p>
            <p className="text-[0.6875rem] leading-snug text-warning/90">
              Detached workspaces fall back to the derived recommended build — a working state.
            </p>
          </div>
        )}
        <DialogFooter>
          <Button variant="ghost" onClick={() => onOpenChange(false)}>
            Cancel
          </Button>
          {inUseDetail ? (
            <Button variant="destructive" onClick={() => void attempt(true)} disabled={busy}>
              {busy && <Loader2 className="size-4 animate-spin" />}
              Detach everywhere & delete
            </Button>
          ) : (
            <Button variant="destructive" onClick={() => void attempt(false)} disabled={busy}>
              {busy && <Loader2 className="size-4 animate-spin" />}
              Delete
            </Button>
          )}
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

export function ImageCatalog({
  workspaces,
  embedded = false,
}: {
  workspaces: Workspace[];
  embedded?: boolean;
}) {
  const operator = useOperator();
  const [images, setImages] = React.useState<BaseImageEntry[]>([]);
  const [status, setStatus] = React.useState<"loading" | "error" | "ready">("loading");
  const [addOpen, setAddOpen] = React.useState(false);
  const [toDelete, setToDelete] = React.useState<BaseImageEntry | null>(null);

  const load = React.useCallback(() => {
    setStatus("loading");
    baseImagesApi
      .listBaseImages()
      .then((rows) => {
        setImages(rows);
        setStatus("ready");
      })
      .catch(() => setStatus("error"));
  }, []);
  React.useEffect(load, [load]);

  const usage = imageUsage(workspaces);

  return (
    <div className={embedded ? "space-y-3" : "space-y-4"}>
      <div className="flex items-center justify-between gap-3">
        <p className="text-xs leading-snug text-muted-foreground">
          Shared base images — registry, custom recipe, or bring-your-own. A workspace without one
          uses its derived recommended build.
        </p>
        <Button size={embedded ? "sm" : "default"} onClick={() => setAddOpen(true)} disabled={!operator}>
          <Plus className="size-4" /> Add base image
        </Button>
      </div>

      {status === "loading" && <TableSkeleton rows={3} />}
      {status === "error" && <ErrorState onRetry={load} />}
      {status === "ready" && images.length === 0 && (
        <EmptyState
          icon={Box}
          title="No catalog images yet"
          description="Workspaces run on their derived recommended builds until you save a shared image here."
        />
      )}
      {status === "ready" && images.length > 0 && (
        <div className="overflow-x-auto rounded-xl border border-border">
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>Name</TableHead>
                <TableHead>Kind</TableHead>
                <TableHead>Image</TableHead>
                <TableHead>Used by</TableHead>
                <TableHead className="w-[44px]" />
              </TableRow>
            </TableHeader>
            <TableBody>
              {images.map((img) => {
                const usedBy = usage.get(img.id) ?? 0;
                return (
                  <TableRow key={img.id}>
                    <TableCell>
                      <span className="inline-flex items-center gap-2.5">
                        <span className="inline-flex size-7 shrink-0 items-center justify-center rounded-md border border-border text-muted-foreground">
                          <Box className="size-3.5" />
                        </span>
                        <span className="text-sm font-medium text-foreground">{img.name}</span>
                      </span>
                    </TableCell>
                    <TableCell>
                      <Chip tone="neutral">{KIND_LABEL[img.kind]}</Chip>
                    </TableCell>
                    <TableCell>
                      <Mono className="text-xs">
                        {img.image}
                        {img.steps?.length ? ` · ${img.steps.length} step${img.steps.length === 1 ? "" : "s"}` : ""}
                      </Mono>
                    </TableCell>
                    <TableCell>
                      <span className="text-xs text-muted-foreground">
                        {usedBy === 0 ? "No workspaces" : `${usedBy} workspace${usedBy === 1 ? "" : "s"}`}
                      </span>
                    </TableCell>
                    <TableCell>
                      <DropdownMenu>
                        <DropdownMenuTrigger asChild>
                          <Button variant="ghost" size="icon" className="size-8" aria-label={`Actions for ${img.name}`}>
                            <MoreHorizontal className="size-4" />
                          </Button>
                        </DropdownMenuTrigger>
                        <DropdownMenuContent align="end">
                          <DropdownMenuItem
                            disabled={!operator}
                            onClick={() => setToDelete(img)}
                            className="text-danger focus:text-danger"
                          >
                            <Trash2 className="size-3.5" /> Delete
                          </DropdownMenuItem>
                        </DropdownMenuContent>
                      </DropdownMenu>
                    </TableCell>
                  </TableRow>
                );
              })}
            </TableBody>
          </Table>
        </div>
      )}

      <AddBaseImageDialog open={addOpen} onOpenChange={setAddOpen} onSaved={() => load()} />
      <DeleteBaseImageDialog target={toDelete} onOpenChange={(o) => !o && setToDelete(null)} onDeleted={load} />
    </div>
  );
}
