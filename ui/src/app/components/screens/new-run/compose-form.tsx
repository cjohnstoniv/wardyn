/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// AI Run Composer — "Describe your task" entry mode. Collects a natural-language
// prompt, optional uploaded attachment TEXT (size-capped client-side to match the
// server caps), optional source-URL hints, and a provider backend. "Compose"
// calls api.compose() and hands the proposal back to the orchestrator for review.
import * as React from "react";
import * as RadioGroupPrimitive from "@radix-ui/react-radio-group";
import { ChevronDown, FileText, Loader2, Sparkles, TriangleAlert, Upload, X } from "lucide-react";
import { RUN_MODE } from "../../wardyn/copy";
import { Button } from "../../ui/button";
import { Input } from "../../ui/input";
import { Textarea } from "../../ui/textarea";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "../../ui/select";
import { Field } from "./step-shell";
import { AskPopover } from "./ask-popover";
import { ModelAccessCard } from "./step-access";
import { WorkspacePicker } from "./workspace-picker";
import { Chip } from "../../wardyn/primitives";
import { cn } from "../../ui/utils";
import { RD } from "../../../lib/workspace-copy";
import { secrets as secretsApi } from "../../../lib/api/secrets";
import { statusTone, statusWord } from "../../../lib/workspace-status";
import { KIND_META } from "../workspaces";
import type { ComposeAttachment, ComposeMode, ComposerBackend, Workspace } from "../../../lib/types";
import {
  compositionSummary,
  unstoredRequiredSecrets,
  type RunWorkspaceSelection,
} from "./wizard-types";

// Mirror the server caps in internal/composer (composer.go):
//   MaxAttachmentBytes = 256 KiB per attachment
//   MaxTotalInputBytes =   1 MiB across prompt + all attachments
//   MaxAttachmentsCount = 32
export const MAX_ATTACHMENT_BYTES = 256 * 1024;
export const MAX_TOTAL_INPUT_BYTES = 1024 * 1024;
export const MAX_ATTACHMENTS_COUNT = 32;

// Byte length of a string as the server measures it (len() over UTF-8 bytes).
export function byteLength(s: string): number {
  return new TextEncoder().encode(s).length;
}

// Decide whether an attachment can be added given the current set + prompt. Pure
// so it's unit-testable. Returns an error string, or null when it fits.
export function attachmentCapError(
  name: string,
  content: string,
  prompt: string,
  existing: ComposeAttachment[],
): string | null {
  if (existing.length >= MAX_ATTACHMENTS_COUNT) {
    return `At most ${MAX_ATTACHMENTS_COUNT} attachments.`;
  }
  const size = byteLength(content);
  if (size > MAX_ATTACHMENT_BYTES) {
    return `"${name}" is ${fmtKiB(size)} — over the ${fmtKiB(MAX_ATTACHMENT_BYTES)} per-file limit.`;
  }
  const total =
    byteLength(prompt) + existing.reduce((n, a) => n + byteLength(a.content), 0) + size;
  if (total > MAX_TOTAL_INPUT_BYTES) {
    return `Adding "${name}" exceeds the ${fmtKiB(MAX_TOTAL_INPUT_BYTES)} total input limit.`;
  }
  return null;
}

function fmtKiB(bytes: number): string {
  return `${Math.round(bytes / 1024)} KiB`;
}

export function ComposeForm({
  prompt,
  workspaceSelections,
  workspaces = [],
  workspacesLoading = false,
  onAddWorkspace,
  attachments,
  sources,
  backend,
  backends,
  mode,
  interactive,
  composing,
  onPromptChange,
  onWorkspaceSelectionsChange,
  onAttachmentsChange,
  onSourcesChange,
  onBackendChange,
  onModeChange,
  onInteractiveChange,
  onCompose,
  error,
  setupHint,
}: {
  prompt: string;
  // Onboarded-workspace multi-select selections (same RunWorkspaceSelection
  // shape the manual wizard's Basics step uses — see workspace-picker.tsx).
  // Empty => ephemeral scratch workspace. api.compose() resolves these against
  // `workspaces` into the wire `workspaces[]` array; onboarded-only, by design
  // (a raw host path is never accepted, mirroring the wizard). enabledOptional
  // now reaches the wire too (Stage 2's workspace_selections + Stage 4's
  // optionalRequirementsEnabled=true below).
  workspaceSelections: RunWorkspaceSelection[];
  // The onboarded workspaces (listWorkspaces()) the picker offers.
  workspaces?: Workspace[];
  workspacesLoading?: boolean;
  // Opens the "Add workspace" onboarding dialog (owned by the parent, mirrors
  // the wizard's Basics step).
  onAddWorkspace: () => void;
  attachments: ComposeAttachment[];
  sources: string[];
  backend: string;
  backends: ComposerBackend[];
  mode: ComposeMode;
  // Operator's run-mode choice, captured UPFRONT: true = interactive, false = background.
  interactive: boolean;
  composing: boolean;
  onPromptChange: (v: string) => void;
  onWorkspaceSelectionsChange: (s: RunWorkspaceSelection[]) => void;
  onAttachmentsChange: (a: ComposeAttachment[]) => void;
  onSourcesChange: (s: string[]) => void;
  onBackendChange: (b: string) => void;
  onModeChange: (m: ComposeMode) => void;
  onInteractiveChange: (v: boolean) => void;
  onCompose: () => void;
  // Persistent inline error from the LAST compose attempt (a transient toast is
  // easy to miss); shown above the footer so a failed compose never looks like
  // "nothing happened". null when the last attempt did not error.
  error?: string | null;
  // Pre-compose readiness hint (new-run-dialog.tsx's own state) — rendered here
  // now, between the model-access card and "More options" (§1 body order),
  // instead of above the whole form. null/absent renders nothing.
  setupHint?: { composerReady: boolean } | null;
}) {
  const fileInputRef = React.useRef<HTMLInputElement>(null);
  const [attachError, setAttachError] = React.useState<string | null>(null);
  const [sourceDraft, setSourceDraft] = React.useState("");
  // Self-fetched, mirroring WorkspacePicker's own identical effect (its
  // storedSecrets state is internal to that component, not reachable from
  // here) — needed so the collapsed context line's unstored-secrets chip (the
  // one safety-relevant fact on the summary — see the honesty note below) can
  // render without opening the picker.
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

  // Read each picked file's TEXT and add it as an attachment, enforcing the
  // per-file + total byte caps. A rejected file surfaces an inline error rather
  // than silently dropping (or sending an oversize body the server would 413).
  const onFiles = async (files: FileList | null) => {
    if (!files?.length) return;
    setAttachError(null);
    let next = [...attachments];
    for (const file of Array.from(files)) {
      let text: string;
      try {
        text = await file.text();
      } catch {
        setAttachError(`Could not read "${file.name}".`);
        continue;
      }
      const err = attachmentCapError(file.name, text, prompt, next);
      if (err) {
        setAttachError(err);
        continue;
      }
      next = [...next, { name: file.name, content: text }];
    }
    onAttachmentsChange(next);
    if (fileInputRef.current) fileInputRef.current.value = "";
  };

  const removeAttachment = (name: string) => {
    onAttachmentsChange(attachments.filter((a) => a.name !== name));
    setAttachError(null);
  };

  const addSource = () => {
    const v = sourceDraft.trim();
    if (!v) return;
    if (!sources.includes(v)) onSourcesChange([...sources, v]);
    setSourceDraft("");
  };
  const removeSource = (s: string) => onSourcesChange(sources.filter((x) => x !== s));

  const totalBytes =
    byteLength(prompt) + attachments.reduce((n, a) => n + byteLength(a.content), 0);
  // No workspace selected is valid — it just means ephemeral.
  const canCompose = prompt.trim().length > 0 && !composing;
  // Always surface which provider/model is in use: a dropdown when there's a real
  // choice (>1 backend), else a read-only display of the single configured backend
  // (so a single Anthropic/Opus or Claude-CLI backend is never invisible).
  const multipleBackends = backends.length > 1;
  // The backend named on "More options"' collapsed summary (§6) — the
  // operator's current pick, else the configured default, else the first
  // configured one. `backends` is non-empty whenever this form is even
  // mounted (new-run-dialog.tsx only renders it once composerEnabled), but the
  // fallback chain stays defensive rather than assuming that from here.
  const activeBackend =
    backends.find((b) => b.name === backend) ?? backends.find((b) => b.is_default) ?? backends[0];

  // The context line's summary facts (§5) — computed here (not inside
  // WorkspacePicker) so the collapsed <summary> can show them without opening
  // the picker.
  const primaryWorkspace = workspaceSelections[0]
    ? workspaces.find((w) => w.id === workspaceSelections[0].workspaceId)
    : undefined;
  // Aggregated across EVERY resolved selection, not just the primary — an
  // attached secondary's unstored secrets are exactly as launch-relevant, and
  // the collapsed summary is the only place they'd otherwise vanish.
  const unstoredAll = workspaceSelections
    .map((sel) => workspaces.find((w) => w.id === sel.workspaceId))
    .flatMap((w) => (w ? unstoredRequiredSecrets(w, storedSecrets) : []));

  return (
    <div className="space-y-5">
      {/* #5: the workspace picker collapsed to a context line — the decision was
          made on screen 1 (or the picker below); keep the ANSWER visible on the
          summary, put the editing UI one click away. Capability (multi-attach,
          optional opt-ins, target override) is fully preserved inside — this
          only collapses the display. Same <details>/<summary>/ChevronDown idiom
          compose-review.tsx already uses (twice) — no new component. The
          unstored-secrets chip is the one safety-relevant fact, so it stays on
          the summary even collapsed (honesty — §6). */}
      <details className="group rounded-lg border border-border bg-card">
        <summary className="flex cursor-pointer list-none flex-wrap items-center gap-2 rounded-lg p-3 text-sm hover:bg-accent/40">
          {workspaceSelections.length > 0 ? (
            <>
              {/* A selection whose id doesn't resolve (workspace list fetch
                  failed mid-session) must never read as "No workspace" — it IS
                  attached and will be mounted; degrade to the raw id like the
                  picker below does. Guarded KIND_META index: a pre-migration
                  "container" row is a live wire value outside the TS union. */}
              <span className="font-medium text-foreground">
                Workspace: {primaryWorkspace?.name ?? workspaceSelections[0].workspaceId}
                {workspaceSelections.length > 1 ? ` +${workspaceSelections.length - 1} more` : ""}
              </span>
              {primaryWorkspace && (
                <Chip tone={statusTone(primaryWorkspace.status).tone}>
                  {statusWord(primaryWorkspace.status)}
                </Chip>
              )}
              {primaryWorkspace && (
                <Chip tone="neutral" mono>
                  {compositionSummary(primaryWorkspace) ??
                    (KIND_META[primaryWorkspace.kind] ?? KIND_META.local_dir).label}
                </Chip>
              )}
              {unstoredAll.length > 0 && (
                <Chip tone="warning">
                  <TriangleAlert className="size-3" aria-hidden="true" />
                  {unstoredAll.length} secret{unstoredAll.length > 1 ? "s" : ""}{" "}
                  {unstoredAll.length > 1 ? "aren't" : "isn't"} stored
                </Chip>
              )}
            </>
          ) : (
            <span className="font-medium text-foreground">
              No workspace — an empty scratch directory inside the sandbox.
            </span>
          )}
          <ChevronDown
            className="ml-auto size-4 shrink-0 text-muted-foreground transition-transform group-open:rotate-180"
            aria-hidden="true"
          />
        </summary>
        <div className="space-y-3 border-t border-border p-3">
          <p className="text-[0.6875rem] leading-snug text-muted-foreground">
            The first workspace is the primary. Attach another if this task needs it, or remove it to
            run in an ephemeral scratch directory.
          </p>
          <WorkspacePicker
            selections={workspaceSelections}
            onChange={onWorkspaceSelectionsChange}
            workspaces={workspaces}
            loading={workspacesLoading}
            onAddWorkspace={onAddWorkspace}
            optionalRequirementsEnabled={true}
          />
        </div>
      </details>

      <Field
        label="Describe your task"
        htmlFor="compose-prompt"
        hint="Describe what you want the agent to do, in plain language. Wardyn proposes a confined run setup for you to review before launch — this feature is in beta."
      >
        <Textarea
          id="compose-prompt"
          placeholder="e.g. Triage the failing CI on acme/payments-service, find the flaky test, and open a PR with a fix."
          value={prompt}
          onChange={(e) => onPromptChange(e.target.value)}
          rows={5}
        />
      </Field>

      {/* Run mode gets ONE home (§4): captured upfront, here, as a real input —
          Review renders the result as a neutral fact chip, never a second
          control (overriding it after grading would invalidate the displayed
          risk grade). */}
      <Field
        label="Run mode"
        hint="Interactive comes up idle so you attach and drive it over a terminal; Autonomous runs the task unattended and stops when done."
      >
        <ModeToggle interactive={interactive} onChange={onInteractiveChange} disabled={composing} />
      </Field>

      {/* The agent isn't known until the proposal returns — "claude-code" here
          only narrows the tier-3 compatibility display; Review shows the
          server's authoritative llm_access once the proposal resolves it. */}
      <ModelAccessCard agent="claude-code" primaryWorkspaceId={workspaceSelections[0]?.workspaceId} />

      {/* Halved (H8): composerReady is the only fact left here — the card above
          states the llmReady fact more precisely. Lives here, not above the
          whole form, to hold body order (§1): after model access, before "More
          options". */}
      {setupHint && !setupHint.composerReady && (
        <div className="flex items-start gap-2 rounded-lg border border-warning/30 bg-warning-subtle p-3 text-xs leading-relaxed text-warning">
          <TriangleAlert className="mt-0.5 size-3.5 shrink-0" aria-hidden="true" />
          <div className="space-y-1.5">
            <p>No integration powers Wardyn&apos;s own AI features yet — this composer included.</p>
            <p>
              {/* Plain <a>, not react-router's <Link>: ComposeForm's own unit
                  tests render it with no Router ancestor (same rationale as
                  ModelAccessCard / workspace-picker's unstored-secret CTAs). */}
              <a
                href="/integrations"
                className="font-medium underline underline-offset-2 hover:text-warning"
              >
                Add an integration
              </a>
              .
            </p>
          </div>
        </div>
      )}

      {/* #6: one "More options" disclosure — clarify mode, backend (only a real
          choice with >1), attachments, source URLs. The summary suffix keeps
          analyzer provenance visible without opening it. */}
      <details className="group rounded-lg border border-border bg-card">
        <summary className="flex cursor-pointer list-none flex-wrap items-center gap-2 rounded-lg p-3 text-sm hover:bg-accent/40">
          <span className="font-medium text-foreground">More options</span>
          {activeBackend && (
            <span className="text-[0.6875rem] text-muted-foreground">
              analyzed by {activeBackend.name}
              {activeBackend.model ? ` · ${activeBackend.model}` : ""}
            </span>
          )}
          <ChevronDown
            className="ml-auto size-4 shrink-0 text-muted-foreground transition-transform group-open:rotate-180"
            aria-hidden="true"
          />
        </summary>
        <div className="space-y-5 border-t border-border p-3">
          <Field label="Clarifying questions" htmlFor="compose-clarify-mode">
            <Select
              value={mode}
              onValueChange={(v) => onModeChange(v as ComposeMode)}
              disabled={composing}
            >
              <SelectTrigger id="compose-clarify-mode" className="w-full sm:w-[220px]">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value="auto">Auto (ask if needed)</SelectItem>
                <SelectItem value="always">Always ask first</SelectItem>
                <SelectItem value="skip">Skip questions</SelectItem>
              </SelectContent>
            </Select>
          </Field>

          {/* DELETED: the single-backend read-only row — its name+model now live
              in this disclosure's OWN summary suffix above (same fact, one line
              instead of fourteen). Only a real choice (>1 backend) gets a control. */}
          {multipleBackends && (
            <Field
              label="Which integration analyzes your task"
              htmlFor="compose-backend"
              hint={RD.ADVISORY}
            >
              <Select value={backend} onValueChange={onBackendChange}>
                <SelectTrigger id="compose-backend">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  {backends.map((b) => (
                    <SelectItem key={b.name} value={b.name}>
                      {b.name}
                      {b.model ? ` — ${b.model}` : ""}
                      {b.is_default ? " (default)" : ""}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            </Field>
          )}

          <Field
            label={
              <span>
                Attachments{" "}
                <span className="font-normal text-muted-foreground">(optional, text files)</span>
              </span>
            }
            hint={`Read locally as text and sent as analysis hints — never executed. Up to ${fmtKiB(
              MAX_ATTACHMENT_BYTES,
            )} per file, ${fmtKiB(MAX_TOTAL_INPUT_BYTES)} total.`}
          >
            <div className="space-y-2">
              <input
                ref={fileInputRef}
                type="file"
                multiple
                className="sr-only"
                aria-label="Attach files"
                onChange={(e) => onFiles(e.target.files)}
              />
              <div className="flex items-center gap-2">
                <Button
                  type="button"
                  variant="outline"
                  size="sm"
                  onClick={() => fileInputRef.current?.click()}
                >
                  <Upload className="size-4" /> Attach files
                </Button>
                {/* The KiB budget, moved beside the control it measures instead
                    of sitting alone in the footer (§2). */}
                <span className="text-[0.6875rem] text-muted-foreground">
                  {fmtKiB(totalBytes)} / {fmtKiB(MAX_TOTAL_INPUT_BYTES)} used
                </span>
              </div>
              {attachments.length > 0 && (
                <ul className="space-y-1">
                  {attachments.map((a) => (
                    <li
                      key={a.name}
                      className="flex items-center gap-2 rounded-md border border-border bg-surface-2 px-2 py-1 text-xs"
                    >
                      <FileText className="size-3.5 shrink-0 text-muted-foreground" />
                      <span className="truncate font-mono text-foreground">{a.name}</span>
                      <span className="ml-auto shrink-0 text-muted-foreground">
                        {fmtKiB(byteLength(a.content))}
                      </span>
                      <button
                        type="button"
                        onClick={() => removeAttachment(a.name)}
                        className="text-muted-foreground transition-colors hover:text-foreground"
                        aria-label={`Remove ${a.name}`}
                      >
                        <X className="size-3.5" />
                      </button>
                    </li>
                  ))}
                </ul>
              )}
              {attachError && <p className="text-[0.6875rem] text-danger">{attachError}</p>}
            </div>
          </Field>

          <Field
            label={
              <span>
                Source URLs{" "}
                <span className="font-normal text-muted-foreground">(optional)</span>
              </span>
            }
            hint="URL hints for the analyzer. Wardyn never fetches them — they add no egress surface."
          >
            <div className="space-y-2">
              <div className="flex items-center gap-2">
                <Input
                  placeholder="https://github.com/acme/payments-service/issues/42"
                  value={sourceDraft}
                  aria-label="Source URL"
                  onChange={(e) => setSourceDraft(e.target.value)}
                  onKeyDown={(e) => {
                    if (e.key === "Enter") {
                      e.preventDefault();
                      addSource();
                    }
                  }}
                  className="font-mono"
                />
                <Button type="button" variant="outline" size="sm" onClick={addSource}>
                  Add
                </Button>
              </div>
              {sources.length > 0 && (
                <div className="flex flex-wrap gap-1.5">
                  {sources.map((s) => (
                    <span
                      key={s}
                      className="inline-flex items-center gap-1 rounded-md border border-border bg-surface-2 px-2 py-0.5 font-mono text-[0.6875rem] text-foreground"
                    >
                      {s}
                      <button
                        type="button"
                        onClick={() => removeSource(s)}
                        className="text-muted-foreground transition-colors hover:text-foreground"
                        aria-label={`Remove ${s}`}
                      >
                        <X className="size-3" />
                      </button>
                    </span>
                  ))}
                </div>
              )}
            </div>
          </Field>
        </div>
      </details>

      {error && (
        <div
          role="alert"
          className="flex items-start gap-2 rounded-lg border border-danger/30 bg-danger-subtle/40 px-3 py-2 text-[0.75rem] leading-snug text-danger"
        >
          <span className="font-semibold">Compose failed:</span>
          <span className="text-foreground/90">{error}</span>
        </div>
      )}
      <div className="flex items-center justify-end gap-2 border-t border-border pt-4">
        <AskPopover context={{ step: "describe", prompt, backend }} triggerLabel="Ask a question" />
        <Button onClick={onCompose} disabled={!canCompose}>
          {composing ? (
            <Loader2 className="size-4 animate-spin" />
          ) : (
            <Sparkles className="size-4" />
          )}
          Compose
        </Button>
      </div>
    </div>
  );
}

// Compact segmented control to choose Interactive vs Autonomous — captured
// UPFRONT here (§4: one home for run mode); Review renders the result as a
// neutral fact chip, never a second control (overriding it after grading
// would silently invalidate the risk grade already on screen). Moved from
// compose-review.tsx's deleted ModeToggle, unchanged. Radix supplies the APG
// radiogroup keyboard behaviour (roving tabindex, arrows move selection AND
// focus, Home/End); the primitive Item is used directly because the shadcn
// wrapper hardcodes a dot indicator this segmented pill doesn't have.
function ModeToggle({
  interactive,
  onChange,
  disabled,
}: {
  interactive: boolean;
  onChange: (v: boolean) => void;
  disabled?: boolean;
}) {
  return (
    <RadioGroupPrimitive.Root
      aria-label="Run mode"
      className="inline-flex rounded-md border border-border p-0.5"
      value={interactive ? "interactive" : "autonomous"}
      onValueChange={(v) => onChange(v === "interactive")}
      disabled={disabled}
    >
      {(["interactive", "autonomous"] as const).map((m) => {
        const active = interactive === (m === "interactive");
        return (
          <RadioGroupPrimitive.Item
            key={m}
            value={m}
            title={RUN_MODE[m].blurb}
            className={cn(
              "rounded px-2 py-0.5 text-xs font-medium transition-colors disabled:opacity-50",
              active ? "bg-primary/15 text-primary" : "text-muted-foreground hover:text-foreground",
            )}
          >
            {RUN_MODE[m].label}
          </RadioGroupPrimitive.Item>
        );
      })}
    </RadioGroupPrimitive.Root>
  );
}
