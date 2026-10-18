/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import type * as React from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter, Route, Routes, useLocation } from "react-router-dom";
import { SCENARIO_HARNESSES } from "../../../../lib/new-run-fixtures/scenarios/harnesses";
import { RUN_DOCUMENT_CANON } from "../../../../lib/run-document-canon";
import type { TemplateDiagnostic, TemplateImportResult } from "../../../../lib/types/templates";
import { OperatorProvider } from "../../../wardyn/operator-context";
import { NEW_RUN_SCREENS, NewRunRoute, useNewRunDraft, type NewRunScreenName, type NewRunScreenProps } from "../new-run-route";
import { NR_DOCUMENT as C } from "../../../wardyn/copy/new-run/document";
import { NR_SHARED } from "../../../wardyn/copy/new-run/shared";
import { DocumentScreen, type DocumentDeps } from "./document-screen";
import { MAX_DOCUMENT_BYTES } from "./run-document";

const ROSTER = SCENARIO_HARNESSES.member;

const EXCLUDED = "template_field_excluded";
const CONFLICT = "run_end_conflicting_fields";
const MOCK_YAML = `api_version: wardyn/v1
kind: Run
title: Carry this title
intent:
  agent: claude-code
  ends_after_sec: 28800
  no_end: true
`;

function Probe() {
  const draft = useNewRunDraft();
  const where = useLocation();
  return (
    <>
      <p data-testid="path">{where.pathname + where.search}</p>
      <p data-testid="origin">{draft.origin.kind}</p>
      <p data-testid="intent">{JSON.stringify(draft.intent)}</p>
      <p data-testid="doc">{JSON.stringify(draft.doc ?? null)}</p>
    </>
  );
}

const answers = (diagnostics: TemplateDiagnostic[], intent: Record<string, unknown> = { agent: "claude-code" }): TemplateImportResult =>
  diagnostics.length ? { diagnostics } : { diagnostics, document: { api_version: "wardyn/v1", kind: "Run", intent } };

function open(deps: Partial<DocumentDeps> = {}, entry = "/runs/new?step=document&format=yaml") {
  const importDocument = vi.fn(deps.importDocument ?? (async () => answers([])));
  const loadHarnesses = deps.loadHarnesses ?? (async () => ROSTER);
  const Screen = (props: NewRunScreenProps) => <><DocumentScreen {...props} deps={{ importDocument, loadHarnesses }} /><Probe /></>;
  // Every other screen is only a probe of where the flow went and what the draft holds.
  const screens = Object.fromEntries(
    (Object.keys(NEW_RUN_SCREENS) as NewRunScreenName[]).map((name) => [name, name === "document" ? Screen : Probe]),
  ) as Record<NewRunScreenName, React.ComponentType<NewRunScreenProps>>;
  render(
    <MemoryRouter initialEntries={[entry]}>
      <OperatorProvider operator={false} principal="alice">
        <Routes><Route path="/runs/new" element={<NewRunRoute screens={screens} />} /></Routes>
      </OperatorProvider>
    </MemoryRouter>,
  );
  return { importDocument };
}

const editor = () => screen.getByRole("textbox", { name: C.EDITOR_ARIA("YAML") }) as HTMLTextAreaElement;
const type = (value: string) => fireEvent.change(screen.getByRole("textbox"), { target: { value } });
const press = (name: string) => fireEvent.click(screen.getByRole("button", { name }));
const text = (id: string) => screen.getByTestId(id).textContent;
const markedLines = () => Array.from(screen.getByTestId("document-gutter").querySelectorAll("[data-error]")).map((n) => n.getAttribute("data-line"));
const file = (name: string, body: string) => new File([body], name);
const upload = (f: File) => fireEvent.change(screen.getByTestId("document-file"), { target: { files: [f] } });

afterEach(cleanup);

describe("run document editor", () => {
  it("says the four canon errors in the mock's words, each on its line, and the server's sentence for the rest", async () => {
    const { importDocument } = open({
      importDocument: async () =>
        answers([
          { path: "title", reason: EXCLUDED, message: "title is never part of a template" },
          { path: "intent", reason: CONFLICT, message: "server end sentence" },
          { path: "api_version", reason: "template_version_unsupported", message: "server version sentence" },
          { path: "intent.agent", reason: "template_field_unknown", message: "agent is not a field the server reads" },
        ]),
    });
    await screen.findByRole("textbox");
    type(MOCK_YAML);
    press(C.VALIDATE);
    expect(await screen.findByText(RUN_DOCUMENT_CANON.TITLE_OR_DESCRIPTION)).toBeInTheDocument();
    // the end conflict marks both keys but is one problem, said once
    expect(screen.getAllByText(RUN_DOCUMENT_CANON.END_CONFLICT)).toHaveLength(1);
    expect(screen.getByText(RUN_DOCUMENT_CANON.API_VERSION)).toBeInTheDocument();
    expect(screen.getByText("agent is not a field the server reads")).toBeInTheDocument();
    expect(screen.queryByText("server end sentence")).toBeNull();
    // title, api_version, the agent key, and both end keys
    expect(markedLines()).toEqual(["1", "3", "5", "6", "7"]);
    expect(screen.getByText(C.ISSUES(4))).toBeInTheDocument();
    expect(editor()).toHaveAttribute("aria-invalid", "true");
    // Make's panel: the headline, then each problem once, and no pending-copy line
    expect(screen.getByTestId("validation-panel").querySelectorAll("p")).toHaveLength(5);
    expect(importDocument).toHaveBeenCalledWith("yaml", MOCK_YAML, expect.any(AbortSignal));
  });

  it("refuses JSON that does not parse before any request, with the canon line once", async () => {
    const { importDocument } = open({}, "/runs/new?step=document&format=json");
    await screen.findByRole("textbox");
    type('{"api_version":"wardyn/v1","kind":"Run","intent":{');
    press(C.VALIDATE);
    expect(await screen.findByText(RUN_DOCUMENT_CANON.JSON_INVALID)).toBeInTheDocument();
    expect(screen.getAllByText(RUN_DOCUMENT_CANON.JSON_INVALID)).toHaveLength(1);
    expect(importDocument).not.toHaveBeenCalled();
    // the editor adds no second line of its own
    expect(screen.queryByTestId("document-gutter")?.querySelector("[data-error]")).toBeNull();
  });

  it("refuses YAML that does not parse before any request", async () => {
    const { importDocument } = open();
    await screen.findByRole("textbox");
    type("intent: [unclosed");
    press(C.VALIDATE);
    expect(await screen.findByText(C.YAML_INVALID)).toBeInTheDocument();
    expect(importDocument).not.toHaveBeenCalled();
  });

  it("Import file refuses a file over 1 MiB before reading it or asking the server", async () => {
    const { importDocument } = open();
    await screen.findByRole("textbox");
    const before = editor().value;
    const big = file("big.yaml", "a: 1");
    Object.defineProperty(big, "size", { value: MAX_DOCUMENT_BYTES + 1 });
    upload(big);
    await waitFor(() => expect(screen.getByTestId("validation-panel")).toHaveAttribute("data-status", "issues"));
    expect(editor().value).toBe(before);
    expect(importDocument).not.toHaveBeenCalled();
    expect(screen.getByTestId("validation-panel")).toHaveTextContent(C.TOO_LARGE);
  });

  it("Import file says so when the browser cannot read the file (FR-0098)", async () => {
    open();
    await screen.findByRole("textbox");
    const real = globalThis.FileReader;
    class Unreadable {
      onerror: (() => void) | null = null;
      readAsText() { queueMicrotask(() => this.onerror?.()); }
      abort() {}
    }
    vi.stubGlobal("FileReader", Unreadable);
    try {
      upload(file("run.yaml", "a: 1"));
      await waitFor(() => expect(screen.getByTestId("validation-panel")).toHaveTextContent(C.FILE_UNREADABLE));
    } finally {
      vi.stubGlobal("FileReader", real);
    }
  });

  it("Import file loads a .json file as JSON and moves the URL to it", async () => {
    open();
    await screen.findByRole("textbox");
    upload(file("run.json", '{"api_version":"wardyn/v1","kind":"Run","intent":{"agent":"codex-cli"}}'));
    await waitFor(() => expect(text("path")).toContain("format=json"));
    expect((screen.getByRole("textbox", { name: C.EDITOR_ARIA("JSON") }) as HTMLTextAreaElement).value).toContain('"agent":"codex-cli"');
    expect(JSON.parse(text("doc")!).format).toBe("json");
  });

  it("Load example writes a run of the roster's first enabled harness", async () => {
    open({ loadHarnesses: async () => [{ ...ROSTER[0], enabled: false }, { ...ROSTER[1], id: "second", enabled: true }] });
    await waitFor(() => expect(screen.getByRole("button", { name: C.LOAD_EXAMPLE })).toBeEnabled());
    press(C.LOAD_EXAMPLE);
    expect(editor().value).toContain("kind: harness");
    expect(editor().value).toContain("id: second");
    expect(editor().value).not.toContain(ROSTER[0].id);
  });

  // DR-0217: a file read lands whenever the browser finishes it. These readers finish only when told, and still
  // call onload after abort, so the screen's own guard (not the browser's abort) is what is pinned.
  describe("a file read that finishes late", () => {
    class SlowReader {
      static all: SlowReader[] = [];
      result: string | null = null;
      aborted = false;
      onload: (() => void) | null = null;
      onerror: (() => void) | null = null;
      constructor() {
        SlowReader.all.push(this);
      }
      readAsText() {}
      abort() {
        this.aborted = true;
      }
      finish(body: string) {
        this.result = body;
        act(() => this.onload?.());
      }
    }
    const YAML_A = "api_version: wardyn/v1\nkind: Run\nintent:\n  agent: from-file-a\n";
    const YAML_B = "api_version: wardyn/v1\nkind: Run\nintent:\n  agent: from-file-b\n";

    beforeEach(() => {
      SlowReader.all = [];
      vi.stubGlobal("FileReader", SlowReader);
    });
    afterEach(() => vi.unstubAllGlobals());

    it("does not overwrite what was typed after the file was chosen", async () => {
      open();
      await screen.findByRole("textbox");
      upload(file("a.yaml", YAML_A));
      type("intent: typed-later");
      SlowReader.all[0].finish(YAML_A);
      expect(editor().value).toBe("intent: typed-later");
      expect(SlowReader.all[0].aborted).toBe(true);
    });

    it("does not overwrite a Load example pressed after the file was chosen", async () => {
      open();
      await waitFor(() => expect(screen.getByRole("button", { name: C.LOAD_EXAMPLE })).toBeEnabled());
      upload(file("a.yaml", YAML_A));
      press(C.LOAD_EXAMPLE);
      const example = editor().value;
      SlowReader.all[0].finish(YAML_A);
      expect(editor().value).toBe(example);
    });

    it("lets only the newest of two chosen files land", async () => {
      open();
      await screen.findByRole("textbox");
      upload(file("a.yaml", YAML_A));
      upload(file("b.yaml", YAML_B));
      SlowReader.all[1].finish(YAML_B);
      SlowReader.all[0].finish(YAML_A);
      expect(editor().value).toBe(YAML_B);
    });

    it("is aborted when the screen is left, and a callback that still arrives changes nothing", async () => {
      open();
      await screen.findByRole("textbox");
      upload(file("a.yaml", YAML_A));
      cleanup();
      expect(SlowReader.all[0].aborted).toBe(true);
      expect(() => SlowReader.all[0].finish(YAML_A)).not.toThrow();
    });
  });

  it("holds Load example without a reason when the roster could not be read", async () => {
    open({ loadHarnesses: async () => { throw new Error("setup is offline"); } });
    await waitFor(() => expect(screen.getByRole("button", { name: C.LOAD_EXAMPLE })).toBeDisabled());
    expect(screen.queryByText(C.NO_HARNESS)).toBeNull();
    expect(screen.getByRole("button", { name: C.LOAD_EXAMPLE })).not.toHaveAttribute("aria-describedby");
  });

  it("holds Load example when no harness is enabled", async () => {
    open({ loadHarnesses: async () => [{ ...ROSTER[0], enabled: false }] });
    await waitFor(() => expect(screen.getByRole("button", { name: C.LOAD_EXAMPLE })).toBeDisabled());
    // FR-0098: the reason is written where the button is off, and the button points at it.
    expect(screen.getByText("Choose a harness to load its example.")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: C.LOAD_EXAMPLE })).toHaveAccessibleDescription("Choose a harness to load its example.");
  });

  it("announces one error in the singular", async () => {
    open({}, "/runs/new?step=document&format=json");
    await screen.findByRole("textbox");
    type('{"api_version":"wardyn/v1","kind":"Run","intent":{');
    press(C.VALIDATE);
    await waitFor(() => expect(screen.getByRole("status")).toHaveTextContent("1 error found."));
    expect(screen.getByText("1 issue must be fixed")).toBeInTheDocument();
  });

  it("Validate alone marks a clean document valid, announces it, and stays put", async () => {
    open();
    await screen.findByRole("textbox");
    type(MOCK_YAML);
    press(C.VALIDATE);
    await waitFor(() => expect(screen.getByTestId("validation-panel")).toHaveAttribute("data-status", "valid"));
    expect(JSON.parse(text("doc")!).validated).toBe(true);
    expect(text("origin")).toBe("scratch");
    expect(screen.getByRole("status")).toHaveTextContent(C.ANNOUNCE_VALID);
    expect(text("path")).toContain("step=document");
  });

  it("Open in Guided commits the door's whole intent, document-only keys included, and opens the first step", async () => {
    const intent = { shape: "agent_task", experience: "interactive", image: "registry.example/tools:1", inline_policy: { auto_stop_after_sec: 600 } };
    open({ importDocument: async () => answers([], intent) });
    await screen.findByRole("textbox");
    type(MOCK_YAML);
    press(C.OPEN_IN_GUIDED);
    await waitFor(() => expect(text("path")).toContain("step=shape"));
    expect(JSON.parse(text("intent")!)).toEqual(intent);
    expect(text("origin")).toBe("import");
    expect(text("doc")).toBe("null");
  });

  it("Continue to review commits and opens Review", async () => {
    open({ importDocument: async () => answers([], { agent: "claude-code" }) });
    await screen.findByRole("textbox");
    type(MOCK_YAML);
    press(C.CONTINUE_REVIEW);
    await waitFor(() => expect(text("path")).toContain("step=review"));
    expect(text("intent")).toBe('{"agent":"claude-code"}');
  });

  it("Ctrl+Enter validates and continues", async () => {
    open();
    await screen.findByRole("textbox");
    type(MOCK_YAML);
    fireEvent.keyDown(document, { key: "Enter", ctrlKey: true });
    await waitFor(() => expect(text("path")).toContain("step=review"));
  });

  it("does not commit or move on while the door reports problems", async () => {
    open({ importDocument: async () => answers([{ path: "title", reason: EXCLUDED, message: "x" }]) });
    await screen.findByRole("textbox");
    type(MOCK_YAML);
    press(C.OPEN_IN_GUIDED);
    expect(await screen.findByText(RUN_DOCUMENT_CANON.TITLE_OR_DESCRIPTION)).toBeInTheDocument();
    expect(text("path")).toContain("step=document");
    expect(text("intent")).toBe("{}");
    expect(JSON.parse(text("doc")!).validated).toBe(false);
  });

  it("shows a failed request as its own text and commits nothing", async () => {
    open({ importDocument: async () => { throw new Error("The server did not answer."); } });
    await screen.findByRole("textbox");
    type(MOCK_YAML);
    press(C.CONTINUE_REVIEW);
    expect(await screen.findByText("The server did not answer.")).toBeInTheDocument();
    expect(text("path")).toContain("step=document");
    expect(text("intent")).toBe("{}");
  });

  it("drops an answer about text that has since changed", async () => {
    let release: (r: TemplateImportResult) => void = () => {};
    open({ importDocument: () => new Promise<TemplateImportResult>((resolve) => { release = resolve; }) });
    await screen.findByRole("textbox");
    type(MOCK_YAML);
    press(C.CONTINUE_REVIEW);
    type(`${MOCK_YAML}# edited\n`);
    await act(async () => release(answers([])));
    expect(text("path")).toContain("step=document");
    expect(text("intent")).toBe("{}");
    expect(screen.getByTestId("validation-panel")).toHaveAttribute("data-status", "ready");
  });

  it("switches format at once, with no confirm, and keeps the URL on the format shown", async () => {
    open();
    await screen.findByRole("textbox");
    type("agent: claude-code\n");
    fireEvent.click(within(screen.getByRole("group", { name: C.FORMAT_ARIA })).getByRole("button", { name: C.FORMAT_JSON }));
    expect(screen.queryByRole("alertdialog")).toBeNull();
    await waitFor(() => expect(text("path")).toContain("format=json"));
    expect((screen.getByRole("textbox", { name: C.EDITOR_ARIA("JSON") }) as HTMLTextAreaElement).value).toBe('{\n  "agent": "claude-code"\n}');
  });

  it("leaves text that does not parse in its format and says why", async () => {
    const { importDocument } = open();
    await screen.findByRole("textbox");
    type("a: [1,\n");
    fireEvent.click(screen.getByRole("button", { name: C.FORMAT_JSON }));
    expect(editor().value).toBe("a: [1,\n");
    expect(text("path")).toContain("format=yaml");
    expect(screen.getByTestId("validation-panel")).toHaveTextContent(C.YAML_INVALID);
    expect(importDocument).not.toHaveBeenCalled();
  });

  it("lays out Make's heading, toolbar and footer", async () => {
    open();
    await screen.findByRole("textbox");
    const footer = screen.getByRole("contentinfo");
    expect(within(footer).getAllByRole("button").map((b) => b.textContent)).toEqual([NR_SHARED.BACK, C.VALIDATE, `${C.CONTINUE_REVIEW}→`]);
    const guided = screen.getByRole("button", { name: C.OPEN_IN_GUIDED });
    expect(footer.contains(guided)).toBe(false);
    expect(guided.compareDocumentPosition(screen.getByRole("group", { name: C.FORMAT_ARIA })) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  });

  it("Back returns to Start", async () => {
    open();
    await screen.findByRole("textbox");
    press(NR_SHARED.BACK);
    await waitFor(() => expect(text("path")).not.toContain("step="));
  });

  it("the FR-0098 sentences are the approved words", () => {
    expect([C.TOO_LARGE, C.FILE_UNREADABLE, C.NO_HARNESS]).toEqual(["This file is over 1 MiB, the most the editor reads.", "The browser could not read this file.", "Choose a harness to load its example."]);
  });

  it("starts from the draft's intent written as a kind Run document", async () => {
    open();
    expect(editor().value).toBe("api_version: wardyn/v1\nkind: Run\nintent: {}\n");
    expect(text("doc")).toBe("null");
  });
});
