/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The run page's Preview URLs tile (internal/api/run_previews.go): the previews of the run's ports, a form that
// creates one, and a one-click revoke. A preview opens in its own tab through a single-use ticket, the way a UI app
// does. The tile shows for the run's person or one who may enter it, and for a security admin, who lists and revokes.
// A background run has no web application gateway, so there the tile says that and nothing else.
//
// Every refusal under a form or an Open is the server's sentence, shown as it came. Only a 404 with reason
// run_not_found from the ticket route is the console's own: that preview has ended.
import * as React from "react";
import { Check, Copy, ExternalLink, Globe, Lock, Plus, Trash2, Users, X } from "lucide-react";
import { HttpError } from "../../../lib/api/core";
import { health as healthApi } from "../../../lib/api/health";
import { previews as previewsApi } from "../../../lib/api/previews";
import { basePath } from "../../../lib/base-path";
import { relativeTime } from "../../../lib/format";
import { isBackgroundRun, mayEnterRunOrUnknown } from "../../../lib/run-entry";
import { RUN_MODE_REFUSAL } from "../../../lib/run-mode-refusals";
import { PREVIEW_ENDED_REASON, type PreviewVisibility, type RunPreview } from "../../../lib/types/previews";
import { useCopyToClipboard } from "../../../lib/use-copy-to-clipboard";
import type { RunDetail } from "../../../lib/types";
import { Button } from "../../ui/button";
import { Input } from "../../ui/input";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "../../ui/select";
import { Mono } from "../../wardyn/code-block";
import { PREVIEW_TTL_DEFAULT, PREVIEW_TTL_OPTIONS, PREVIEW_VISIBILITY_LABEL, RUN_PREVIEWS as T } from "../../wardyn/copy/run-previews";
import { DANGER_OUTLINE } from "../../wardyn/form-actions";
import { Field } from "../../wardyn/form-primitives";
import { useShellSetupStatus } from "../../wardyn/model-access-context";
import { useOperator, useOperatorResolved, usePrincipal } from "../../wardyn/operator-context";
import { Chip, WidgetCard } from "../../wardyn/primitives";
import { RadioCard, RadioCardGroup } from "../../wardyn/radio-card";
import { enterUIApp, openEnterTab } from "../run-detail-ssh";

/** The name of the app the UI gateway presents for a preview of one port (types.PreviewAppPrefix + port). */
const previewApp = (port: number) => `__preview-${port}`;

/** The door a background run lacks, in the sentence BACKGROUND_ONLY builds. */
const BACKGROUND_DOOR = "web application gateway";

/** The reasons of a create refusal that is about the port field. */
const PORT_REASONS = new Set(["preview_port_reserved", "preview_port_declared"]);

type Failure = { port: number; message: string };

/** What an Open or a copy says when it fails: the server's sentence, except a 404 run_not_found (the preview has ended). */
export function openFailureText(e: unknown): string {
  if (e instanceof HttpError && e.status === 404 && e.reason === PREVIEW_ENDED_REASON) return T.ENDED;
  return e instanceof Error && e.message.trim() ? e.message : String(e);
}

const VisibilityChip = ({ visibility }: { visibility: PreviewVisibility }) => (
  <Chip tone={visibility === "private" ? "neutral" : "info"}>
    {visibility === "private" ? <Lock className="size-3" aria-hidden="true" /> : <Users className="size-3" aria-hidden="true" />}
    {PREVIEW_VISIBILITY_LABEL[visibility]}
  </Chip>
);

function CopyLinkButton({ runId, port }: { runId: string; port: number }) {
  const { copied, copy } = useCopyToClipboard(2000);
  // The link is a console route, not a server one: it opens the preview for whoever follows it and is admitted.
  const link = `${window.location.origin}${basePath()}/previews/${encodeURIComponent(runId)}/${port}`;
  return (
    <>
      <Button size="sm" variant="outline" onClick={() => void copy(link)}>
        {copied ? <Check className="size-3.5 text-success" aria-hidden="true" /> : <Copy className="size-3.5" aria-hidden="true" />}
        {copied ? T.LINK_COPIED : T.COPY_LINK}
      </Button>
      {/* The console's usual polite announcement, beside the button so it stays out of the button's name. */}
      <span aria-live="polite" className="sr-only">
        {copied ? "Copied" : ""}
      </span>
    </>
  );
}

function PreviewRow({
  runId,
  preview,
  revoked,
  admin,
  busy,
  failure,
  onOpen,
  onRevoke,
}: {
  runId: string;
  preview: RunPreview;
  revoked: boolean;
  /** A security admin on a run they cannot enter: Open only on an Organisation row, no Copy link. */
  admin: boolean;
  busy: boolean;
  failure: string | null;
  onOpen: () => void;
  onRevoke: () => void;
}) {
  const { port, visibility } = preview;
  const address = <Mono className="text-meta text-muted-foreground">{`localhost:${port}`}</Mono>;
  if (revoked) {
    return (
      <div className="border-t border-border py-3 first-of-type:border-t-0 first-of-type:pt-0 opacity-70" data-testid={`preview-${port}`}>
        <p className="flex flex-wrap items-center gap-2 text-sm font-medium text-muted-foreground">
          {T.ROW_TITLE(port)}
          <Chip>
            <X className="size-3" aria-hidden="true" />
            {T.REVOKED_CHIP}
          </Chip>
        </p>
        <p className="mt-0.5 text-meta text-muted-foreground">
          {address} · {T.REVOKED_NOTE}
        </p>
      </div>
    );
  }
  const mayOpen = !admin || visibility !== "private";
  return (
    <div className="border-t border-border py-3 first-of-type:border-t-0 first-of-type:pt-0" data-testid={`preview-${port}`}>
      <div className="flex flex-wrap items-start justify-between gap-x-3 gap-y-2">
        <div className="min-w-0">
          <p className="flex flex-wrap items-center gap-2 text-sm font-medium text-foreground">
            {T.ROW_TITLE(port)}
            <VisibilityChip visibility={visibility} />
          </p>
          <p className="mt-0.5 text-meta text-muted-foreground">
            {address} · {T.EXPIRES(relativeTime(preview.expires_at))}
          </p>
        </div>
        <div className="flex flex-wrap gap-2">
          {mayOpen && (
            <Button size="sm" variant="outline" disabled={busy} onClick={onOpen}>
              <ExternalLink className="size-3.5" aria-hidden="true" />
              {T.OPEN}
            </Button>
          )}
          {!admin && visibility === "org" && <CopyLinkButton runId={runId} port={port} />}
          <Button size="sm" variant="outline" className={DANGER_OUTLINE} onClick={onRevoke}>
            <Trash2 className="size-3.5" aria-hidden="true" />
            {T.REVOKE}
          </Button>
        </div>
      </div>
      {failure && (
        <div className="mt-2 rounded-lg border border-warning/30 bg-warning-subtle px-3 py-2.5">
          <p className="text-xs font-medium text-foreground">{T.OPEN_FAILED(port)}</p>
          <p className="mt-1.5 font-mono text-meta leading-relaxed text-muted-foreground">{failure}</p>
        </div>
      )}
    </div>
  );
}

export function PreviewsPanel({ run }: { run: RunDetail }) {
  const principal = usePrincipal();
  const operator = useOperator();
  const operatorResolved = useOperatorResolved();
  const { status: setup } = useShellSetupStatus();
  const background = isBackgroundRun(run);
  // The run's person, or one who may enter it, creates; any other viewer here is a security admin, who lists and revokes.
  const canCreate = mayEnterRunOrUnknown(run, principal, operator, operatorResolved);
  const admin = !canCreate;
  // The site ceiling the page loaded with. Public is never offered: a ceiling of public reads as Organisation here.
  const maxVisibility = setup?.features?.preview_max_visibility;
  const ceiling: "private" | "org" = maxVisibility === "org" || maxVisibility === "public" ? "org" : "private";

  const [rows, setRows] = React.useState<RunPreview[] | null>(null);
  const [loadError, setLoadError] = React.useState<string | null>(null);
  const [revoked, setRevoked] = React.useState<ReadonlySet<number>>(new Set());
  const [formOpen, setFormOpen] = React.useState<boolean | null>(null);
  const [port, setPort] = React.useState("");
  const [ttl, setTtl] = React.useState(PREVIEW_TTL_DEFAULT);
  const [visibility, setVisibility] = React.useState<PreviewVisibility>("private");
  const [creating, setCreating] = React.useState(false);
  const [createError, setCreateError] = React.useState<{ message: string; reason: string } | null>(null);
  const [opening, setOpening] = React.useState<number | null>(null);
  const [failure, setFailure] = React.useState<Failure | null>(null);

  const runId = run.id;
  React.useEffect(() => {
    if (background) return;
    let alive = true;
    setRows(null);
    setLoadError(null);
    previewsApi
      .list(runId)
      .then((list) => alive && setRows(list))
      .catch((e: unknown) => alive && setLoadError(openFailureText(e)));
    return () => {
      alive = false;
    };
  }, [runId, background]);

  const upsert = (p: RunPreview) =>
    setRows((prev) => [...(prev ?? []).filter((r) => r.port !== p.port), p].sort((a, b) => a.port - b.port));

  async function create(e: React.FormEvent) {
    e.preventDefault();
    if (!/^\d+$/.test(port)) return;
    setCreating(true);
    setCreateError(null);
    try {
      const p = await previewsApi.put(runId, Number(port), { visibility, ttl_seconds: Number(ttl) });
      upsert(p);
      setRevoked((prev) => new Set([...prev].filter((n) => n !== p.port)));
      setPort("");
      setFormOpen(false);
    } catch (err) {
      setCreateError({ message: openFailureText(err), reason: err instanceof HttpError ? err.reason : "" });
    } finally {
      setCreating(false);
    }
  }

  async function open(p: RunPreview) {
    setFailure(null);
    // The tab opens here, before the ticket request, so the browser still counts it as the click's own.
    const tab = openEnterTab();
    if (!tab) {
      setFailure({ port: p.port, message: T.POPUP_BLOCKED });
      return;
    }
    setOpening(p.port);
    try {
      const ticket = await previewsApi.ticket(runId, p.port);
      const h = await healthApi.health();
      await enterUIApp(runId, previewApp(p.port), h.ui_sandbox ?? null, { ticket, tab });
    } catch (err) {
      tab.close();
      setFailure({ port: p.port, message: openFailureText(err) });
    } finally {
      setOpening(null);
    }
  }

  async function revoke(p: RunPreview) {
    setFailure(null);
    try {
      await previewsApi.revoke(runId, p.port);
      setRevoked((prev) => new Set(prev).add(p.port));
    } catch (err) {
      setFailure({ port: p.port, message: openFailureText(err) });
    }
  }

  const live = rows ?? [];
  const showForm = canCreate && (formOpen ?? live.length === 0) && rows !== null;
  const capped = (v: PreviewVisibility): { reason: string } | undefined =>
    v === "public" || (v === "org" && ceiling === "private") ? { reason: T.CAPPED(PREVIEW_VISIBILITY_LABEL[ceiling]) } : undefined;

  return (
    <WidgetCard title={T.TITLE} Icon={Globe} grow bodyClassName="scroll-thin overflow-y-auto p-4" data-testid="previews-panel">
      <section aria-label={T.TITLE}>
        {background ? (
          <p className="text-meta leading-relaxed text-muted-foreground">{RUN_MODE_REFUSAL.BACKGROUND_ONLY(BACKGROUND_DOOR)}</p>
        ) : (
          <>
            <p className="mb-3 text-meta leading-relaxed text-muted-foreground">{admin ? T.ADMIN_INTRO : T.INTRO}</p>
            {loadError && (
              <p role="alert" className="text-meta leading-relaxed text-muted-foreground">
                {loadError}
              </p>
            )}
            {rows !== null && live.length === 0 && <p className={`text-sm text-muted-foreground ${showForm ? "mb-3" : ""}`}>{T.EMPTY}</p>}
            {live.map((p) => (
              <PreviewRow
                key={p.id}
                runId={runId}
                preview={p}
                revoked={revoked.has(p.port)}
                admin={admin}
                busy={opening === p.port}
                failure={failure?.port === p.port ? failure.message : null}
                onOpen={() => void open(p)}
                onRevoke={() => void revoke(p)}
              />
            ))}
            {showForm && (
              <form onSubmit={(e) => void create(e)} className="mt-1 space-y-4" aria-label={T.CREATE}>
                <div className="grid grid-cols-2 gap-3">
                  <Field label={T.PORT_LABEL} htmlFor="preview-port">
                    <Input
                      id="preview-port"
                      inputMode="numeric"
                      maxLength={5}
                      placeholder="3000"
                      value={port}
                      aria-invalid={createError ? PORT_REASONS.has(createError.reason) : undefined}
                      onChange={(e) => setPort(e.target.value.replace(/\D/g, ""))}
                    />
                  </Field>
                  <Field label={T.TTL_LABEL} htmlFor="preview-ttl">
                    <Select value={ttl} onValueChange={setTtl}>
                      <SelectTrigger id="preview-ttl" className="w-full">
                        <SelectValue />
                      </SelectTrigger>
                      <SelectContent>
                        {PREVIEW_TTL_OPTIONS.map((o) => (
                          <SelectItem key={o.value} value={o.value}>
                            {o.label}
                          </SelectItem>
                        ))}
                      </SelectContent>
                    </Select>
                  </Field>
                </div>
                <div>
                  <p id="preview-visibility-label" className="mb-2 text-sm font-medium text-foreground">
                    {T.VISIBILITY_LABEL}
                  </p>
                  <RadioCardGroup
                    name="preview-visibility"
                    aria-labelledby="preview-visibility-label"
                    value={visibility}
                    onValueChange={(v) => setVisibility(v as PreviewVisibility)}
                    className="gap-2"
                  >
                    <RadioCard value="private" title={T.PRIVATE} description={T.PRIVATE_BODY} unavailable={capped("private")} />
                    <RadioCard value="org" title={T.ORG} description={T.ORG_BODY} unavailable={capped("org")} />
                    <RadioCard value="public" title={T.PUBLIC} description={T.PUBLIC_BODY} unavailable={capped("public")} />
                  </RadioCardGroup>
                </div>
                {createError && (
                  <p role="alert" className="text-xs text-danger">
                    {createError.message}
                  </p>
                )}
                <Button type="submit" disabled={creating || port === ""}>
                  {T.CREATE}
                </Button>
                <p className="text-meta leading-relaxed text-muted-foreground">{T.CHANGE_HINT}</p>
              </form>
            )}
            {canCreate && rows !== null && !showForm && (
              <Button size="sm" variant="outline" className="mt-1" onClick={() => setFormOpen(true)}>
                <Plus className="size-3.5" aria-hidden="true" />
                {T.NEW}
              </Button>
            )}
            <p className="mt-3 text-meta leading-relaxed text-muted-foreground">{T.FOOT}</p>
          </>
        )}
      </section>
    </WidgetCard>
  );
}
