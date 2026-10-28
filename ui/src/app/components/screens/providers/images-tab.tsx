/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The Images tab (#923, decision 1): one row per image in the base-image
// catalog workspaces already fill, each with its "Available to" (kind image,
// value = the reference), and Add image. An image is admins-only until
// someone is listed (capImage widens), so the control's first choice reads
// Admins only (decision 2). Every string is availability-copy.ts's canon.
//
// Its own resource, like the Agents tab: it reads and writes itself, and its
// one teal button is Add image, so the screen's Save providers is withheld
// while it is open.
//
// A pre-wrapped image (design final §3, the approved final-prewrap mock) runs
// as is: Add image offers it as "How it runs", the row shows its check on the
// cluster, and a refused or cut-short check gets Check again. A security admin
// reaches this tab on its own page and adds pre-wrapped images only.
import * as React from "react";
import { AlertTriangle, Container, Loader2 } from "lucide-react";
import { baseImages } from "../../../lib/api/base-images";
import { getErrorMessage, shortDate } from "../../../lib/format";
import { AVAILABILITY, IMAGES, PREWRAPPED } from "../../../lib/availability-copy";
import { ACCESS_STATE, PEOPLE } from "../../../lib/people-access-copy";
import { PROVIDERS } from "../../../lib/workspace-providers-copy";
import { useDeferredBusy } from "../../../lib/use-deferred-busy";
import type { BaseImageEntry } from "../../../lib/types";
import { Button } from "../../ui/button";
import { Input } from "../../ui/input";
import { Dialog, DialogContent, DialogFooter, DialogHeader, DialogTitle } from "../../ui/dialog";
import {
  AvailabilityControl,
  AvailabilityDraft,
  AvailabilityUserTypes,
  writeAvailability,
  type AvailabilityDraftValue,
} from "../../wardyn/availability-control";
import { makeMono } from "../../wardyn/code-block";
import { Field, OptionCard } from "../../wardyn/form-primitives";
import { useOperator } from "../../wardyn/operator-context";
import { PageContainer } from "../../wardyn/page-container";
import { PageHeader } from "../../wardyn/page-header";
import { Chip } from "../../wardyn/primitives";
import { EmptyState, TableSkeleton } from "../../wardyn/states";

const withMono = makeMono(["--image"]);

// How often the list is read again while a check is running on the cluster.
export const CHECK_POLL_MS = 5000;

// The server's own rule (base_images_prewrapped.go): <repository>[:tag]@sha256:<64 hex>.
const PINNED = /^[^@\s]+@sha256:[0-9a-f]{64}$/;

// The registry and organisation a pull secret must read: the reference up to its last path segment.
function registryOf(ref: string): string {
  const name = ref.split("@")[0];
  return name.slice(0, Math.max(0, name.lastIndexOf("/")));
}

// What a pre-wrapped row's check says, and whether it can be checked again. reason is
// "<code>:<arch>[:<detail>]" (the detail may hold colons), or check_interrupted.
export function prewrappedLine(r: BaseImageEntry, builder: boolean): { text: string; refused: boolean } | null {
  const c = r.check;
  if (!c) return null;
  if (c.status === "checking") return { text: PREWRAPPED.CHECKING, refused: false };
  const [code = "", arch = "", ...rest] = (c.reason ?? "").split(":");
  const detail = rest.join(":");
  if (c.status === "ready") {
    const ok = c.accepted_arches ?? [];
    if (code === "platform_missing") return { text: PREWRAPPED.READY_NO_BUILD(ok[0] ?? "", arch), refused: false };
    if (code === "no_node") return { text: PREWRAPPED.READY_NO_NODE(ok[0] ?? "", arch), refused: false };
    return { text: PREWRAPPED.READY(ok, c.checked_at ? shortDate(c.checked_at) : ""), refused: false };
  }
  switch (code) {
    case "check_interrupted":
    case "timed_out":
      return { text: PREWRAPPED.CUT_SHORT, refused: true };
    case "contract_missing":
      return { text: PREWRAPPED.MISSING_TOOL(detail, builder), refused: true };
    case "contract_old":
      return { text: PREWRAPPED.CONTRACT_OLD, refused: true };
    case "unpullable":
      return { text: PREWRAPPED.UNPULLABLE(detail, registryOf(r.image)), refused: true };
    case "no_node":
      return { text: PREWRAPPED.NO_NODE, refused: true };
    case "platform_missing":
      return { text: PREWRAPPED.NO_BUILD, refused: true };
  }
  return { text: PREWRAPPED.FAILED(detail || code), refused: true };
}

// /admin/images: the tab on its own page, for a security admin, who has no Providers screen.
export function ImagesScreen() {
  return (
    <PageContainer width="list">
      <PageHeader title={IMAGES.TAB} />
      <ImagesTab />
    </PageContainer>
  );
}

// One user-types read for every row's control and the Add image dialog's.
export function ImagesTab() {
  return (
    <AvailabilityUserTypes>
      <ImagesTabBody />
    </AvailabilityUserTypes>
  );
}

function ImagesTabBody() {
  const [rows, setRows] = React.useState<BaseImageEntry[] | null>(null);
  const [builder, setBuilder] = React.useState(true);
  const [status, setStatus] = React.useState<"loading" | "error" | "ready">("loading");
  const [adding, setAdding] = React.useState(false);
  const [retrying, setRetrying] = React.useState<Record<string, string>>({});

  const load = React.useCallback(() => {
    baseImages
      .list()
      .then((list) => {
        setRows(list.rows);
        setBuilder(list.imageBuilder);
        setStatus("ready");
      })
      .catch(() => setStatus("error"));
  }, []);
  React.useEffect(load, [load]);
  // A check runs for up to ten minutes on the cluster: read again until none is running.
  const checking = !!rows?.some((r) => r.check?.status === "checking");
  React.useEffect(() => {
    if (!checking) return;
    const t = setTimeout(load, CHECK_POLL_MS);
    return () => clearTimeout(t);
  }, [checking, rows, load]);

  const checkAgain = (r: BaseImageEntry) => {
    setRetrying((m) => ({ ...m, [r.id]: "" }));
    baseImages
      .checkAgain(r.id)
      .then(() => {
        setRetrying(({ [r.id]: _, ...m }) => m);
        load();
      })
      .catch((e) => setRetrying((m) => ({ ...m, [r.id]: getErrorMessage(e) })));
  };

  if (status === "loading") return <TableSkeleton rows={2} cols={2} />;
  if (status === "error" || !rows) {
    return (
      <EmptyState
        icon={AlertTriangle}
        title={PROVIDERS.FETCH_FAILED_TITLE}
        description={PROVIDERS.FETCH_FAILED_BODY}
        action={
          <Button variant="outline" size="sm" onClick={load}>
            {ACCESS_STATE.FETCH_FAILED_RETRY}
          </Button>
        }
      />
    );
  }

  const addButton = <Button onClick={() => setAdding(true)}>{IMAGES.ADD_CTA}</Button>;
  return (
    <div className="space-y-4">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <p className="max-w-[70ch] text-body text-muted-foreground">{withMono(IMAGES.LEAD)}</p>
        {/* One teal per surface: while empty, the empty state's own Add image is it. */}
        {rows.length > 0 && addButton}
      </div>
      {rows.length === 0 ? (
        <EmptyState icon={Container} title={IMAGES.EMPTY_TITLE} description={IMAGES.EMPTY_BODY} action={addButton} size="compact" />
      ) : (
        rows.map((r) => {
          const prewrapped = r.kind === "prewrapped";
          const line = prewrapped ? prewrappedLine(r, builder) : null;
          const retryError = retrying[r.id];
          return (
            <div key={r.id} data-testid={`image-row-${r.image}`} className="overflow-hidden rounded-lg border border-border">
              <div className="space-y-1 p-3">
                <div className="flex flex-wrap items-center gap-2">
                  <span className="text-body font-medium">{r.name}</span>
                  {prewrapped && <Chip>{PREWRAPPED.BADGE}</Chip>}
                </div>
                <div className="break-all font-mono text-meta text-muted-foreground">{r.image}</div>
                {line && <p className={line.refused ? "text-meta text-danger" : "text-meta text-muted-foreground"}>{line.text}</p>}
                {!prewrapped && !builder && <p className="text-meta text-muted-foreground">{PREWRAPPED.NEEDS_BUILDER}</p>}
                {line?.refused && (
                  <div className="pt-1">
                    <Button variant="outline" size="sm" disabled={retryError === ""} onClick={() => checkAgain(r)}>
                      {PREWRAPPED.CHECK_AGAIN}
                    </Button>
                    {retryError && <p className="mt-1 text-xs leading-snug text-danger">{retryError}</p>}
                  </div>
                )}
              </div>
              {/* A pre-wrapped image is offered to nobody until it is ready, so who gets it waits for that. */}
              {(!prewrapped || r.check?.status === "ready") && (
                <div className="border-t border-border p-3">
                  <AvailabilityControl kind="image" value={r.image} adminsOnly note={prewrapped ? undefined : AVAILABILITY.IMAGE_NOTE} />
                </div>
              )}
            </div>
          );
        })
      )}
      {rows.some((r) => r.kind === "prewrapped") && <p className="max-w-[70ch] text-meta text-muted-foreground">{PREWRAPPED.FOOTNOTE}</p>}
      <AddImageDialog open={adding} onOpenChange={setAdding} onAdded={load} builder={builder} />
    </div>
  );
}

const START: AvailabilityDraftValue = { restricted: false, audiences: [] };

// Add image asks who gets it on the form (decision 4), starting at Admins
// only. Add image creates the catalog row, then writes the list, then Only
// these. A refused list write keeps the dialog open on the saved image, which
// stays admins-only meanwhile, so there is no window where it is open to all.
function AddImageDialog({
  open,
  onOpenChange,
  onAdded,
  builder,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  onAdded: () => void;
  builder: boolean;
}) {
  // A security admin adds pre-wrapped images only, and with no image builder nothing else can run.
  const operator = useOperator();
  const wrapOffered = builder && operator;
  const [kind, setKind] = React.useState<"byo" | "prewrapped">(wrapOffered ? "byo" : "prewrapped");
  const [ref, setRef] = React.useState("");
  const [draft, setDraft] = React.useState<AvailabilityDraftValue>(START);
  const [saving, setSaving] = React.useState(false);
  const [error, setError] = React.useState<string | null>(null);
  const [partial, setPartial] = React.useState<{ entry: BaseImageEntry; message: string } | null>(null);
  const { showSpinner } = useDeferredBusy(saving);

  React.useEffect(() => {
    if (!open) {
      setKind(wrapOffered ? "byo" : "prewrapped");
      setRef("");
      setDraft(START);
      setError(null);
      setPartial(null);
    }
  }, [open, wrapOffered]);

  const submit = async () => {
    if (!ref.trim()) return;
    if (kind === "prewrapped" && !PINNED.test(ref.trim())) {
      setError(PREWRAPPED.NOT_PINNED);
      return;
    }
    setSaving(true);
    setError(null);
    let entry: BaseImageEntry;
    try {
      entry = await baseImages.add(ref.trim(), kind);
    } catch (e) {
      setError(getErrorMessage(e));
      setSaving(false);
      return;
    }
    try {
      await writeAvailability("image", entry.image, draft);
      onOpenChange(false);
    } catch (e) {
      setPartial({ entry, message: getErrorMessage(e) });
    } finally {
      setSaving(false);
      onAdded();
    }
  };

  const cancel = (
    <Button type="button" variant="ghost" onClick={() => onOpenChange(false)}>
      {PEOPLE.CANCEL}
    </Button>
  );
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-lg" aria-describedby={undefined}>
        <DialogHeader>
          <DialogTitle>{partial ? partial.entry.name : IMAGES.ADD_CTA}</DialogTitle>
        </DialogHeader>
        {partial ? (
          <div className="space-y-4">
            <div className="flex flex-col gap-1 rounded-lg bg-danger-subtle p-3 text-body text-danger">
              <b className="font-semibold">{AVAILABILITY.CREATE_PARTIAL_TITLE}</b>
              <span>{partial.message}</span>
              <span>{AVAILABILITY.CREATE_PARTIAL_IMAGE}</span>
            </div>
            <div className="border-t border-border pt-4">
              <AvailabilityControl kind="image" value={partial.entry.image} adminsOnly />
            </div>
            <DialogFooter>{cancel}</DialogFooter>
          </div>
        ) : (
          <form
            className="space-y-4"
            onSubmit={(e) => {
              e.preventDefault();
              void submit();
            }}
          >
            <div className="space-y-2" role="group" aria-label={PREWRAPPED.HOW_LABEL}>
              <div className="text-sm font-medium text-foreground">{PREWRAPPED.HOW_LABEL}</div>
              <div className="grid gap-2 sm:grid-cols-2">
                <OptionCard
                  selected={kind === "byo"}
                  disabled={!wrapOffered || saving}
                  onClick={() => setKind("byo")}
                  title={PREWRAPPED.KIND_WRAP}
                  hint={!operator ? PREWRAPPED.KIND_WRAP_ADMINS_ONLY : !builder ? PREWRAPPED.KIND_WRAP_NO_BUILDER : PREWRAPPED.KIND_WRAP_HINT}
                />
                <OptionCard
                  selected={kind === "prewrapped"}
                  disabled={saving}
                  onClick={() => setKind("prewrapped")}
                  title={PREWRAPPED.KIND}
                  hint={PREWRAPPED.KIND_HINT}
                />
              </div>
            </div>
            <Field
              label={kind === "prewrapped" ? PREWRAPPED.REF : IMAGES.REF}
              htmlFor="add-image-ref"
              hint={
                kind === "prewrapped" ? (
                  <>
                    {PREWRAPPED.REF_HINT}{" "}
                    <a href={PREWRAPPED.HOW_TO_BUILD_URL} target="_blank" rel="noreferrer" className="underline">
                      {PREWRAPPED.HOW_TO_BUILD}
                    </a>
                  </>
                ) : (
                  IMAGES.REF_HINT
                )
              }
              required
            >
              <Input
                id="add-image-ref"
                required
                autoFocus
                value={ref}
                onChange={(e) => setRef(e.target.value)}
                disabled={saving}
                aria-invalid={error ? true : undefined}
                className="font-mono"
              />
            </Field>
            {error && <p className="text-xs leading-snug text-danger">{error}</p>}
            <div className="border-t border-border pt-4">
              <AvailabilityDraft draft={draft} onChange={setDraft} disabled={saving} adminsOnly />
            </div>
            <DialogFooter>
              {cancel}
              <Button type="submit" disabled={saving || !ref.trim() || draft.pending}>
                {showSpinner && <Loader2 className="size-3.5 animate-spin" />}
                {kind === "prewrapped" ? PREWRAPPED.ADD_CTA : IMAGES.ADD_CTA}
              </Button>
            </DialogFooter>
          </form>
        )}
      </DialogContent>
    </Dialog>
  );
}
