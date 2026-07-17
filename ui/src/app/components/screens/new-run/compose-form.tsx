/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// AI Run Composer — "Describe your task" entry mode. Collects a natural-language
// prompt, optional uploaded attachment TEXT (size-capped client-side to match the
// server caps), optional source-URL hints, and a provider backend. "Compose"
// calls api.compose() and hands the proposal back to the orchestrator for review.
import * as React from "react";
import { FileText, Loader2, Sparkles, Upload, X } from "lucide-react";
import { RUN_MODE } from "../../wardyn/copy";
import { Button } from "../../ui/button";
import { Input } from "../../ui/input";
import { Switch } from "../../ui/switch";
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
import { WorkspacePicker } from "./workspace-picker";
import { Mono } from "../../wardyn/code-block";
import { cn } from "../../ui/utils";
import type {
  ComposeAttachment,
  ComposeMode,
  ComposerBackend,
  Workspace,
  WorkspaceSelection,
} from "../../../lib/types";

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
  useSubscription,
  composing,
  onPromptChange,
  onWorkspaceSelectionsChange,
  onAttachmentsChange,
  onSourcesChange,
  onBackendChange,
  onModeChange,
  onInteractiveChange,
  onUseSubscriptionChange,
  onCompose,
  error,
}: {
  prompt: string;
  // Onboarded-workspace multi-select selections (same WorkspaceSelection shape
  // the manual wizard's Basics step uses — see workspace-picker.tsx). Empty =>
  // ephemeral scratch workspace. api.compose() resolves these against
  // `workspaces` into the wire `workspaces[]` array; onboarded-only, by design
  // (a raw host path is never accepted, mirroring the wizard).
  workspaceSelections: WorkspaceSelection[];
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
  // Explicit PER-RUN opt-in to Claude subscription mode (see ComposeRequest.useSubscription).
  useSubscription: boolean;
  composing: boolean;
  onPromptChange: (v: string) => void;
  onWorkspaceSelectionsChange: (s: WorkspaceSelection[]) => void;
  onAttachmentsChange: (a: ComposeAttachment[]) => void;
  onSourcesChange: (s: string[]) => void;
  onBackendChange: (b: string) => void;
  onModeChange: (m: ComposeMode) => void;
  onInteractiveChange: (v: boolean) => void;
  onUseSubscriptionChange: (v: boolean) => void;
  onCompose: () => void;
  // Persistent inline error from the LAST compose attempt (a transient toast is
  // easy to miss); shown above the footer so a failed compose never looks like
  // "nothing happened". null when the last attempt did not error.
  error?: string | null;
}) {
  const fileInputRef = React.useRef<HTMLInputElement>(null);
  const [attachError, setAttachError] = React.useState<string | null>(null);
  const [sourceDraft, setSourceDraft] = React.useState("");

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

  return (
    <div className="space-y-5">
      <Field
        label="Describe your task"
        htmlFor="compose-prompt"
        hint="Describe what you want the agent to do, in plain language. Wardyn proposes a confined run setup for you to review before launch."
      >
        <Textarea
          id="compose-prompt"
          placeholder="e.g. Triage the failing CI on acme/payments-service, find the flaky test, and open a PR with a fix."
          value={prompt}
          onChange={(e) => onPromptChange(e.target.value)}
          rows={5}
        />
      </Field>

      <Field
        label="Run mode"
        hint="Interactive comes up idle so you attach and drive it over a terminal; Autonomous runs the task unattended and stops when done. You can still change this on the proposal."
      >
        <div className="grid grid-cols-2 gap-2" role="radiogroup" aria-label="Run mode">
          {[
            { v: true, label: RUN_MODE.interactive.label, desc: RUN_MODE.interactive.blurb },
            { v: false, label: RUN_MODE.autonomous.label, desc: RUN_MODE.autonomous.blurb },
          ].map((opt) => {
            const active = interactive === opt.v;
            return (
              <button
                key={opt.label}
                type="button"
                role="radio"
                aria-checked={active}
                tabIndex={active ? 0 : -1}
                onClick={() => onInteractiveChange(opt.v)}
                onKeyDown={(e) => {
                  // APG radiogroup: arrows move selection AND focus (roving
                  // tabindex). Two options, so any arrow selects the other;
                  // the sibling button is the only other child of the group.
                  if (["ArrowRight", "ArrowDown", "ArrowLeft", "ArrowUp"].includes(e.key)) {
                    e.preventDefault();
                    onInteractiveChange(!opt.v);
                    const sib =
                      e.currentTarget.nextElementSibling ??
                      e.currentTarget.previousElementSibling;
                    (sib as HTMLButtonElement | null)?.focus();
                  }
                }}
                className={cn(
                  "flex flex-col items-start gap-0.5 rounded-lg border p-2.5 text-left transition-colors",
                  active
                    ? "border-primary/50 bg-primary/10"
                    : "border-border hover:bg-surface-2",
                )}
              >
                <span className="text-sm font-medium text-foreground">{opt.label}</span>
                <span className="text-[11px] leading-snug text-muted-foreground">{opt.desc}</span>
              </button>
            );
          })}
        </div>
      </Field>

      {/* Per-run subscription opt-in. The AGENT isn't known until the proposal
          returns, so the toggle is always offered here; the server applies it
          only to Claude agents with an operator-blessed ceiling, and the
          proposal's warnings state honestly which model-access mode resulted. */}
      <div className="flex items-center justify-between rounded-lg border border-border p-2.5">
        <div className="flex flex-col gap-0.5 pr-3">
          <label htmlFor="compose-use-subscription" className="text-sm font-medium text-foreground">
            Use my Claude subscription
          </label>
          <span className="text-[11px] leading-snug text-muted-foreground">
            Uses your Claude subscription for this run (Claude agents only). By default the token is
            injected proxy-side — a live, host-refreshed token, so nothing sensitive stays resident in the
            sandbox. Off = a brokered API key instead.
          </span>
        </div>
        <Switch
          id="compose-use-subscription"
          checked={useSubscription}
          onCheckedChange={onUseSubscriptionChange}
          aria-label="Use my Claude subscription"
        />
      </div>

      <Field
        label="Workspaces"
        hint="Only onboarded local directories and repos can be attached — a raw host path is never accepted. The first one selected is the primary (drives the analyzer); leave empty to run in an ephemeral scratch directory."
      >
        <WorkspacePicker
          selections={workspaceSelections}
          onChange={onWorkspaceSelectionsChange}
          workspaces={workspaces}
          loading={workspacesLoading}
          onAddWorkspace={onAddWorkspace}
        />
      </Field>

      {backends.length > 0 && (
        <Field
          label="Provider"
          htmlFor="compose-backend"
          hint="Which configured LLM backend analyzes your task. This is advisory only — it never gets the run's credentials."
        >
          {multipleBackends ? (
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
          ) : (
            <div
              id="compose-backend"
              data-testid="compose-backend-single"
              className="flex items-center gap-2 rounded-md border border-border bg-surface-2 px-2.5 py-1.5 text-sm"
            >
              <span className="font-medium text-foreground">{backends[0].name}</span>
              {backends[0].model && (
                <Mono className="text-muted-foreground">{backends[0].model}</Mono>
              )}
              <span className="ml-auto text-[11px] uppercase tracking-wide text-muted-foreground">
                {backends[0].provider}
              </span>
            </div>
          )}
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
          <Button
            type="button"
            variant="outline"
            size="sm"
            onClick={() => fileInputRef.current?.click()}
          >
            <Upload className="size-4" /> Attach files
          </Button>
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
          {attachError && <p className="text-[11px] text-danger">{attachError}</p>}
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
                  className="inline-flex items-center gap-1 rounded-md border border-border bg-surface-2 px-2 py-0.5 font-mono text-[11px] text-foreground"
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

      {error && (
        <div
          role="alert"
          className="flex items-start gap-2 rounded-lg border border-danger/30 bg-danger-subtle/40 px-3 py-2 text-[12px] leading-snug text-danger"
        >
          <span className="font-semibold">Compose failed:</span>
          <span className="text-foreground/90">{error}</span>
        </div>
      )}
      <div className="flex items-center justify-between border-t border-border pt-4">
        <span className="text-[11px] text-muted-foreground">
          {fmtKiB(totalBytes)} / {fmtKiB(MAX_TOTAL_INPUT_BYTES)} used
        </span>
        <div className="flex items-center gap-2">
          <AskPopover
            context={{ step: "describe", prompt, backend }}
            triggerLabel="Ask a question"
          />
          {/* Clarify mode: Auto (model decides), Always ask, or Skip (one-shot). */}
          <Select value={mode} onValueChange={(v) => onModeChange(v as ComposeMode)} disabled={composing}>
            <SelectTrigger className="h-9 w-[160px]" aria-label="Clarify mode">
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              <SelectItem value="auto">Auto (ask if needed)</SelectItem>
              <SelectItem value="always">Always ask first</SelectItem>
              <SelectItem value="skip">Skip questions</SelectItem>
            </SelectContent>
          </Select>
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
    </div>
  );
}
