/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { beforeEach, describe, expect, it, vi } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { HttpError } from "../../../lib/api/core";
import { TEMPLATE_REFUSAL } from "../../../lib/template-refusals";
import type { Template, TemplateDocument, TemplateSummary } from "../../../lib/types/templates";
import { NR_TEMPLATES as T } from "../../wardyn/copy/new-run/templates";
import { RenameTemplateDialog } from "./rename-template-dialog";

const api = vi.hoisted(() => ({ getRevision: vi.fn(), update: vi.fn() }));
vi.mock("./templates-api", () => ({ templatesApi: api }));

const summary = (over: Partial<TemplateSummary> = {}): TemplateSummary => ({
  id: "d1000000-0000-4000-8000-000000000001",
  scope: "person",
  kind: "template",
  name: "Nightly tests",
  revision: 2,
  coverage: "full",
  includes: ["runner"],
  fields: ["runner_pool_id"],
  needs_setup: [],
  writable: true,
  updated_at: "2026-10-10T09:00:00Z",
  ...over,
});
const doc = (over: Partial<TemplateDocument> = {}): TemplateDocument => ({ api_version: "wardyn/v1", kind: "RunTemplate", intent: { runner_pool_id: "p1" }, ...over });
const stored = (document: TemplateDocument, description?: string) => ({ document, description }) as Template;

function open(over: Partial<TemplateSummary> = {}, rows: TemplateSummary[] = []) {
  const t = summary(over);
  const handlers = { onClose: vi.fn(), onRenamed: vi.fn(), onStale: vi.fn() };
  const ui = (template: TemplateSummary) => <RenameTemplateDialog template={template} rows={[template, ...rows]} {...handlers} />;
  const view = render(ui(t));
  return { t, ...handlers, rerender: (next: TemplateSummary) => view.rerender(ui(next)) };
}
const nameBox = () => screen.getByRole("textbox", { name: T.NAME_LABEL });
const retype = async (v: string) => {
  await userEvent.clear(nameBox());
  await userEvent.type(nameBox(), v);
};
const submit = () => userEvent.click(screen.getByRole("button", { name: T.RENAME_CONFIRM }));

beforeEach(() => {
  vi.clearAllMocks();
  api.getRevision.mockResolvedValue(stored(doc(), "Overnight"));
  api.update.mockResolvedValue({ name: "Weekly tests" });
});

describe("Rename dialog", () => {
  it("opens on the name field with the current name, the canon title and body", () => {
    open();
    expect(screen.getByRole("dialog", { name: T.RENAME_TITLE })).toBeInTheDocument();
    expect(screen.getByText(T.RENAME_BODY)).toBeInTheDocument();
    expect(nameBox()).toHaveFocus();
    expect(nameBox()).toHaveValue("Nightly tests");
  });

  it("saves a new revision from the revision the person saw, with the trimmed name", async () => {
    const { t, onRenamed } = open();
    await retype("  Weekly tests  ");
    await submit();
    await waitFor(() => expect(onRenamed).toHaveBeenCalledWith("Weekly tests"));
    expect(api.getRevision).toHaveBeenCalledWith(t.id, 2);
    expect(api.update).toHaveBeenCalledWith(t.id, {
      scope: "person",
      group_id: undefined,
      kind: "template",
      name: "Weekly tests",
      description: "Overnight",
      document: doc(),
      expected_revision: 2,
    });
  });

  it("a document that carries its own name is renamed with it, or the server refuses the save", async () => {
    api.getRevision.mockResolvedValue(stored(doc({ name: "Nightly tests" })));
    open();
    await retype("Weekly tests");
    await submit();
    await waitFor(() => expect(api.update).toHaveBeenCalled());
    expect(api.update.mock.calls[0][1].document.name).toBe("Weekly tests");
  });

  it("an empty name is refused on the field before any request", async () => {
    open();
    await userEvent.clear(nameBox());
    await submit();
    expect(screen.getByRole("alert")).toHaveTextContent(T.NAME_EMPTY);
    expect(nameBox()).toHaveAttribute("aria-invalid", "true");
    expect(api.getRevision).not.toHaveBeenCalled();
  });

  it("a name another template of the same audience has is refused, ignoring case; another audience is not", async () => {
    const mine = summary({ id: "d2000000-0000-4000-8000-000000000002", name: "Weekly Tests" });
    const orgs = summary({ id: "d3000000-0000-4000-8000-000000000003", scope: "org", name: "Release checks" });
    const { onRenamed } = open({}, [mine, orgs]);
    await retype("weekly tests");
    await submit();
    expect(screen.getByRole("alert")).toHaveTextContent(TEMPLATE_REFUSAL.NAME_TAKEN());
    expect(api.update).not.toHaveBeenCalled();
    await retype("Release checks");
    await submit();
    await waitFor(() => expect(onRenamed).toHaveBeenCalled());
  });

  it("two groups are two audiences", async () => {
    const other = summary({ id: "d2000000-0000-4000-8000-000000000002", scope: "group", group_id: "g-b", name: "Shared" });
    const { onRenamed } = open({ scope: "group", group_id: "g-a" }, [other]);
    await retype("Shared");
    await submit();
    await waitFor(() => expect(onRenamed).toHaveBeenCalled());
  });

  it("stops at the server's 128 characters", () => {
    open();
    expect(nameBox()).toHaveAttribute("maxlength", "128");
  });

  it("an unchanged name closes without a request", async () => {
    const { onClose } = open();
    await submit();
    expect(onClose).toHaveBeenCalled();
    expect(api.getRevision).not.toHaveBeenCalled();
  });

  it("a revision conflict keeps the dialog and the typed name, asks the list to reload, and the retry uses the new revision", async () => {
    const conflict = "This template changed to revision 3 since you opened it. Reload it, then apply your edit again.";
    api.update.mockRejectedValueOnce(new HttpError(409, conflict, "template_revision_conflict"));
    const { t, onStale, onRenamed, rerender } = open();
    await retype("Weekly tests");
    await submit();
    expect(await screen.findByRole("alert")).toHaveTextContent(conflict);
    expect(nameBox()).toHaveValue("Weekly tests");
    expect(onStale).toHaveBeenCalledTimes(1);
    expect(onRenamed).not.toHaveBeenCalled();

    rerender({ ...t, revision: 3 });
    expect(nameBox()).toHaveValue("Weekly tests");
    await submit();
    await waitFor(() => expect(onRenamed).toHaveBeenCalledWith("Weekly tests"));
    expect(api.getRevision).toHaveBeenLastCalledWith(t.id, 3);
    expect(api.update.mock.calls[1][1].expected_revision).toBe(3);
  });

  it("any other refusal shows the server's sentence and does not reload", async () => {
    api.update.mockRejectedValueOnce(new HttpError(403, TEMPLATE_REFUSAL.SCOPE_FORBIDDEN_ORG(), "template_scope_forbidden"));
    const { onStale } = open();
    await retype("Weekly tests");
    await submit();
    expect(await screen.findByRole("alert")).toHaveTextContent(TEMPLATE_REFUSAL.SCOPE_FORBIDDEN_ORG());
    expect(onStale).not.toHaveBeenCalled();
  });

  it("Cancel closes without a request", async () => {
    const { onClose } = open();
    await userEvent.click(screen.getByRole("button", { name: T.CANCEL }));
    expect(onClose).toHaveBeenCalled();
    expect(api.update).not.toHaveBeenCalled();
  });
});
