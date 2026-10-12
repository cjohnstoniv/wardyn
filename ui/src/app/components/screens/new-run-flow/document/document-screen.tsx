/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The run document editor: the draft's intent as YAML or JSON, checked by the
// server's import door. Open in Guided and Continue to review commit the
// door's answer to the draft whole, so nothing the editor held is dropped.

import * as React from "react";
import { setup } from "../../../../lib/api/setup";
import { parsePolicySource } from "../../../../lib/policy-document";
import { RUN_DOCUMENT_CANON } from "../../../../lib/run-document-canon";
import type { SetupHarnessTool } from "../../../../lib/types/setup";
import type { TemplateDocument } from "../../../../lib/types/templates";
import { Button } from "../../../ui/button";
import { DocumentEditor, type DocumentDiagnostic } from "../../../wardyn/policy-document/document-editor";
import type { NewRunLocation } from "../draft/new-run-url";
import type { DocBuffer, RunIntent } from "../draft/run-intent";
import { useNewRunDispatch, useNewRunDraft, type NewRunScreenProps } from "../new-run-route";
import { StepShell, useAnnounce } from "../steps/step-shell";
import { DOCUMENT_COPY, type DocumentCopy } from "./copy-seam";
import { importRunDocument, type ImportDocument } from "./import-door";
import { exampleDocumentText, MAX_DOCUMENT_BYTES, placeDiagnostics, runDocumentText, tooLarge, type DocumentFormat } from "./run-document";
import { ValidationPanel, type DocumentStatus } from "./validation-panel";

export interface DocumentDeps {
  importDocument: ImportDocument;
  /** The harness roster; the example uses the first one that is enabled. */
  loadHarnesses: () => Promise<SetupHarnessTool[]>;
}

const defaultLoadHarnesses = async () => (await setup.getSetupStatus()).harnesses ?? [];

const EDITOR_ID = "run-document";
const STATUS_ID = `${EDITOR_ID}-status`;
const NO_HARNESS_ID = `${EDITOR_ID}-no-harness`;

export function DocumentScreen({ location, go, copy = DOCUMENT_COPY, deps }: NewRunScreenProps & { copy?: DocumentCopy; deps?: Partial<DocumentDeps> }) {
  const draft = useNewRunDraft();
  const dispatch = useNewRunDispatch();
  const announce = useAnnounce();
  const importDocument = deps?.importDocument ?? importRunDocument;
  const loadHarnesses = deps?.loadHarnesses ?? defaultLoadHarnesses;

  const format: DocumentFormat = draft.doc?.format ?? (location.screen === "document" ? location.format : "yaml");
  const text = draft.doc?.text ?? runDocumentText(draft.intent, format);
  const latest = React.useRef({ text, format, intent: draft.intent });
  latest.current = { text, format, intent: draft.intent };

  const [lines, setLines] = React.useState<string[]>([]);
  const [valid, setValid] = React.useState(false);
  const [busy, setBusy] = React.useState(false);
  // undefined while the roster loads, null when it holds no enabled harness.
  const [harness, setHarness] = React.useState<string | null | undefined>(undefined);
  const check = React.useRef<{ id: number; abort?: AbortController }>({ id: 0 });
  const fileInput = React.useRef<HTMLInputElement>(null);

  React.useEffect(() => {
    let off = false;
    loadHarnesses().then(
      (roster) => !off && setHarness(roster.find((h) => h.enabled !== false)?.id ?? null),
      () => !off && setHarness(null),
    );
    return () => { off = true; };
  }, [loadHarnesses]);
  React.useEffect(() => () => check.current.abort?.abort(), []);

  /** Drops any check in flight: its answer is about text that is no longer there. */
  const supersede = () => {
    check.current.abort?.abort();
    check.current = { id: check.current.id + 1 };
    setBusy(false);
    setValid(false);
    setLines([]);
  };

  /** The editor buffer exists only while the text differs from the serialised intent. */
  const buffer = (next: string, as: DocumentFormat, rest: Pick<DocBuffer, "diagnostics" | "validated">): DocBuffer | undefined =>
    next === runDocumentText(latest.current.intent, as) ? undefined : { format: as, text: next, ...rest };
  const setSource = (next: string, as: DocumentFormat) => {
    supersede();
    dispatch({ type: "doc", doc: buffer(next, as, { diagnostics: [], validated: false }) });
    if (as !== format) go({ screen: "document", format: as }, { replace: true });
  };

  const refuse = (id: number, reasons: string[]) => {
    if (id !== check.current.id) return;
    setLines(reasons);
    announce(copy.announceIssues(reasons.length));
  };

  /** Checks the text through the server's door; the document it accepted, or undefined. */
  const validate = async (): Promise<TemplateDocument | undefined> => {
    const { text: source, format: as } = latest.current;
    supersede();
    const mine = { id: check.current.id, abort: new AbortController() };
    check.current = mine;
    setBusy(true);
    try {
      if (tooLarge(source)) return void refuse(mine.id, [copy.tooLarge]);
      // A document that does not parse is refused here, without a request.
      if (!parsePolicySource(source, as).ok) return void refuse(mine.id, [as === "json" ? RUN_DOCUMENT_CANON.JSON_INVALID : copy.yamlInvalid]);
      const answer = await importDocument(as, source, mine.abort.signal);
      if (mine.id !== check.current.id) return undefined;
      if (answer.diagnostics.length > 0) {
        const placed = placeDiagnostics(source, as, answer.diagnostics);
        dispatch({ type: "doc", doc: { format: as, text: source, diagnostics: placed, validated: false } });
        announce(copy.announceIssues(placed.length));
        return undefined;
      }
      dispatch({ type: "doc", doc: buffer(source, as, { diagnostics: [], validated: true }) });
      setValid(true);
      announce(copy.valid);
      return answer.document;
    } catch (error) {
      if (mine.id === check.current.id) refuse(mine.id, [error instanceof Error ? error.message : String(error)]);
      return undefined;
    } finally {
      if (mine.id === check.current.id) setBusy(false);
    }
  };

  const proceed = async (to: NewRunLocation) => {
    const document = await validate();
    if (!document) return;
    dispatch({ type: "commitDoc", intent: document.intent as RunIntent });
    go(to);
  };

  const importFile = (file: File | undefined) => {
    if (!file) return;
    if (file.size > MAX_DOCUMENT_BYTES) {
      supersede();
      return refuse(check.current.id, [copy.tooLarge]);
    }
    const reader = new FileReader();
    reader.onload = () => setSource(String(reader.result), /\.json$/i.test(file.name) ? "json" : "yaml");
    reader.onerror = () => refuse(check.current.id, [copy.fileUnreadable]);
    reader.readAsText(file);
  };

  const diagnostics: DocumentDiagnostic[] = (draft.doc?.diagnostics ?? []).map((d) => ({ line: d.line ?? 1, column: d.column ?? 1, text: d.text }));
  const count = diagnostics.length + lines.length;
  const status: DocumentStatus = count > 0 ? { kind: "issues", count, lines } : valid ? { kind: "valid" } : { kind: "ready" };

  return (
    <StepShell
      eyebrow={copy.eyebrow}
      title={copy.title}
      lede={copy.lede}
      back={{ label: copy.back, onBack: () => go({ screen: "start" }) }}
      primary={{ label: copy.continueToReview, onActivate: () => void proceed({ screen: "review", view: "pretty" }), disabled: busy }}
    >
      <div className="flex flex-col gap-3">
        <div className="flex flex-wrap items-center gap-2">
          <Button type="button" variant="outline" onClick={() => fileInput.current?.click()}>{copy.importFile}</Button>
          <input
            ref={fileInput}
            type="file"
            accept=".yaml,.yml,.json"
            data-testid="document-file"
            aria-label={copy.importFile}
            tabIndex={-1}
            className="sr-only"
            onChange={(e) => {
              const file = e.target.files?.[0];
              e.target.value = "";
              importFile(file);
            }}
          />
          <Button type="button" variant="outline" disabled={!harness} aria-describedby={harness === null ? NO_HARNESS_ID : undefined}
            onClick={() => harness && setSource(exampleDocumentText(harness, format), format)}>
            {copy.loadExample}
          </Button>
          <Button type="button" variant="outline" disabled={busy} onClick={() => void validate()}>{copy.validate}</Button>
          <Button type="button" variant="outline" disabled={busy} onClick={() => void proceed({ screen: "step", step: "shape" })}>{copy.openInGuided}</Button>
        </div>
        {harness === null && <p id={NO_HARNESS_ID} className="text-xs text-muted-foreground">{copy.noHarness}</p>}
        <DocumentEditor
          id={EDITOR_ID}
          value={text}
          format={format}
          onChange={(next) => setSource(next, format)}
          onFormatChange={(as, next) => setSource(next, as)}
          diagnostics={diagnostics}
          label={copy.editorLabel(format)}
          describedBy={STATUS_ID}
          rows={16}
        />
        <ValidationPanel id={STATUS_ID} status={status} copy={copy} />
      </div>
    </StepShell>
  );
}
