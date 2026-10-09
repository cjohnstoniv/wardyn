/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// PolicyEditor — the one place a policy's source is typed. The source is a
// string the caller holds; this renders it, says whether it parses and where it
// does not, and offers the two things that rewrite it on purpose: a format
// conversion and the caller's structured controls. YAML is the format it opens
// in; JSON is a choice.
//
// Nothing here converts, formats or repairs what was typed. A conversion needs
// source that parses and, when it would drop comments, a confirmation.
import * as React from "react";
import { CircleCheck } from "lucide-react";
import type { PolicySourceError, PolicySourceFormat } from "../../../lib/policy-document";
import { POLICY_TAB } from "../../screens/run-detail/policy-tab-copy";
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "../../ui/alert-dialog";
import { Button, buttonVariants } from "../../ui/button";
import { Textarea } from "../../ui/textarea";
import { cn } from "../../ui/utils";
import { toYaml } from "../yaml-block";
import { POLICY_DOCUMENT as D } from "../copy/policy-document";
import { CopyButton } from "../copy-button";
import { Field } from "../form-primitives";
import { Chip, SectionLabel } from "../primitives";
import { Segmented } from "../segmented";
import { POLICY_COPY_BUTTON } from "./policy-document";

const FORMATS: { value: PolicySourceFormat; label: string }[] = [
  { value: "yaml", label: POLICY_TAB.viewYaml },
  { value: "json", label: D.JSON },
];

/** The parse of `source` in `format`: a JSON-compatible mapping, or where and why not. */
export type PolicyEditorParse = { ok: true; value: unknown } | PolicySourceError;

export interface PolicyEditorProps {
  /** The textarea's id; the ids of its described-by lines are derived from it. */
  id: string;
  /** The authored text, exactly as typed. */
  source: string;
  format: PolicySourceFormat;
  /** `source` parsed in `format` by the caller, which also needs the value. */
  parsed: PolicyEditorParse;
  onSourceChange: (source: string) => void;
  /** An explicit conversion: the new format and the source rewritten in it. */
  onFormatChange: (format: PolicySourceFormat, source: string) => void;
  /** Renders "Done editing"; a surface that is nothing but the editor leaves it out. */
  onDone?: () => void;
  /** Chips read off a valid document, beside the validity chip. */
  validExtras?: React.ReactNode;
  /** A structured edit the parser refused on a still-valid document. */
  operationError?: PolicySourceError | null;
  /** Controls that write the source through a parse; held while it does not parse. */
  structured?: React.ReactNode;
  rows?: number;
  className?: string;
}

export function PolicyEditor({
  id,
  source,
  format,
  parsed,
  onSourceChange,
  onFormatChange,
  onDone,
  validExtras,
  operationError,
  structured,
  rows = 10,
  className,
}: PolicyEditorProps) {
  const [confirmJson, setConfirmJson] = React.useState(false);
  const validityId = `${id}-validity`;
  const positionId = `${id}-position`;
  const commentsId = `${id}-comments`;
  const json = format === "json";
  const invalid = !parsed.ok;

  const requestFormat = (next: PolicySourceFormat) => {
    if (next === format || !parsed.ok) return;
    // JSON has nowhere to keep a comment, so that direction asks first.
    if (next === "json") setConfirmJson(true);
    else onFormatChange("yaml", `${toYaml(parsed.value)}\n`);
  };

  return (
    <div
      className={cn("flex min-w-0 flex-col gap-3 rounded-lg border border-border p-3", className)}
      data-testid="policy-source-editor"
      data-format={format}
    >
      <div className="flex flex-wrap items-center gap-2">
        <SectionLabel>{D.SOURCE_POLICY}</SectionLabel>
        <Chip tone="info">{D.EDITING}</Chip>
        {onDone && (
          <Button type="button" variant="outline" size="sm" className="ml-auto" onClick={onDone}>
            {D.DONE}
          </Button>
        )}
      </div>

      <div data-testid="policy-format-switch">
        <Segmented value={format} options={FORMATS} onChange={requestFormat} disabled={invalid} />
      </div>

      <Field label={json ? D.SPEC_JSON : D.SPEC_YAML} htmlFor={id} required>
        <Textarea
          id={id}
          value={source}
          onChange={(e) => onSourceChange(e.target.value)}
          rows={rows}
          spellCheck={false}
          className="font-mono text-xs"
          required
          aria-invalid={invalid || undefined}
          aria-describedby={invalid ? `${validityId} ${positionId} ${commentsId}` : `${validityId} ${commentsId}`}
        />
      </Field>

      {/* One polite region for the changing verdict; it never takes focus. */}
      <div id={validityId} role="status" aria-atomic="true" className="flex flex-wrap items-center gap-1.5">
        {parsed.ok ? (
          <>
            <Chip tone="success" className="gap-1">
              <CircleCheck className="size-3" />
              {json ? D.VALID_JSON : D.VALID_YAML}
            </Chip>
            {validExtras}
          </>
        ) : (
          // The parser's message can quote what was typed: plain text only, selectable.
          <p className="select-text whitespace-pre-wrap break-words text-xs text-danger">
            {json ? D.INVALID_JSON(parsed.message) : D.INVALID_YAML(parsed.message)}
          </p>
        )}
      </div>
      {!parsed.ok && (
        <p id={positionId} className="font-mono text-xs text-muted-foreground">
          {D.SOURCE_POSITION(parsed.line, parsed.column)}
        </p>
      )}
      <p id={commentsId} className="text-xs text-muted-foreground">
        {D.COMMENTS}
      </p>
      <div>
        {/* The text as typed, comments and mistakes included: never held. */}
        <CopyButton text={source} label={D.COPY_SOURCE} className={POLICY_COPY_BUTTON}>
          {D.COPY_SOURCE}
        </CopyButton>
      </div>

      {operationError && <OperationError error={operationError} />}

      {structured && (
        <fieldset disabled={invalid} className="m-0 flex min-w-0 flex-col gap-4 border-0 p-0" data-testid="policy-structured">
          {structured}
        </fieldset>
      )}
      {/* Outside the held group, so there is always a way back to the text. */}
      {invalid && (
        <div>
          <Button type="button" variant="outline" size="sm" onClick={() => document.getElementById(id)?.focus()}>
            {D.EDIT}
          </Button>
        </div>
      )}

      <AlertDialog open={confirmJson} onOpenChange={setConfirmJson}>
        <AlertDialogContent className="sm:max-w-md">
          <AlertDialogHeader>
            <AlertDialogTitle>{D.JSON_TITLE}</AlertDialogTitle>
            <AlertDialogDescription>{D.JSON_BODY}</AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel className={buttonVariants({ variant: "ghost" })}>{D.KEEP_YAML}</AlertDialogCancel>
            <AlertDialogAction
              onClick={() => {
                // Converted from what parses now, never from an earlier valid text.
                if (parsed.ok) onFormatChange("json", JSON.stringify(parsed.value, null, 2));
              }}
            >
              {D.SWITCH_JSON}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </div>
  );
}

/** A structured edit the parser refused on a still-valid source: its own
 *  sentence and position, shown beside the control that asked for it. */
export function OperationError({ error, className }: { error: PolicySourceError; className?: string }) {
  return (
    <p role="status" className={cn("select-text whitespace-pre-wrap break-words text-xs text-warning", className)}>
      {error.message}{" "}
      <span className="font-mono text-muted-foreground">{D.SOURCE_POSITION(error.line, error.column)}</span>
    </p>
  );
}

/**
 * Moves focus when a surface switches between reading and editing: into the
 * source on the way in, back to the control that opened it on the way out.
 * Returns the setter for those two controls, and `expect()` for a change the
 * caller makes some other way. Nothing moves on mount or on an unasked change.
 */
export function useEditModeFocus(
  editing: boolean,
  onEditingChange: (editing: boolean) => void,
  sourceId: string,
  openerId: string,
): { setEditing: (editing: boolean) => void; expect: () => void } {
  const asked = React.useRef(false);
  React.useEffect(() => {
    if (!asked.current) return;
    asked.current = false;
    document.getElementById(editing ? sourceId : openerId)?.focus();
  }, [editing, sourceId, openerId]);
  return {
    setEditing: (next) => {
      asked.current = true;
      onEditingChange(next);
    },
    expect: () => {
      asked.current = true;
    },
  };
}
