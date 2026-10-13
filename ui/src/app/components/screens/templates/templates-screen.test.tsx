/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { beforeEach, describe, expect, it, vi } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { toast } from "sonner";
import { HttpError } from "../../../lib/api/core";
import { SCENARIO_TEMPLATES } from "../../../lib/new-run-fixtures/scenarios/templates";
import { expectNoOwnCopy } from "../../../lib/test-fixtures";
import type { TemplateList, TemplateSummary } from "../../../lib/types/templates";
import { NR_TEMPLATES as T } from "../../wardyn/copy/new-run/templates";
import { NR_RUN } from "../../wardyn/copy/new-run/run";
import { TemplatesScreen } from "./templates-screen";

vi.mock("sonner", () => ({ toast: { error: vi.fn(), success: vi.fn() } }));

const api = vi.hoisted(() => ({
  list: vi.fn(),
  getRevision: vi.fn(),
  update: vi.fn(),
  remove: vi.fn(),
  publishToOrg: vi.fn(),
}));
vi.mock("./templates-api", () => ({ templatesApi: api }));

const ID = (n: number) => `d${n}000000-0000-4000-8000-00000000000${n}`;
const sum = (n: number, over: Partial<TemplateSummary> = {}): TemplateSummary => ({
  id: ID(n),
  scope: "person",
  kind: "template",
  name: `Template ${n}`,
  revision: 2,
  coverage: "full",
  includes: ["runner"],
  fields: ["runner_pool_id"],
  needs_setup: [],
  writable: true,
  updated_at: "2026-10-10T09:00:00Z",
  ...over,
});
const MEMBER = { org: false, groups: [], groups_unverified: false };
const ADMIN = { org: true, groups: [], groups_unverified: false };

const MINE = sum(1, { name: "Nightly tests", description: "Runs the suite overnight.", fields: ["agent", "runner_pool_id", "ends_after_sec", "repo"], includes: ["tools_image", "runner", "repositories"] });
const ORG = sum(2, { scope: "org", kind: "policy", name: "Organisation baseline", writable: false, fields: ["inline_policy.allowed_domains"], includes: ["egress"], needs_setup: ["inline_policy.model_provider"] });
const GROUP = sum(3, { scope: "group", group_id: "g-platform", group_label: "Platform", name: "Package registries", writable: false });

function mount(list: TemplateList, path = "/templates") {
  api.list.mockResolvedValue(list);
  return render(
    <MemoryRouter initialEntries={[path]}>
      <TemplatesScreen />
    </MemoryRouter>,
  );
}
const rowOf = (name: string) => screen.getByText(name).closest("li") as HTMLElement;

beforeEach(() => {
  vi.clearAllMocks();
  api.remove.mockResolvedValue(undefined);
  api.publishToOrg.mockResolvedValue({});
});

describe("Templates screen: rows", () => {
  it("renders the header, the lede, the note and the one primary action from the canon", async () => {
    mount({ templates: [MINE], publish: MEMBER });
    expect(await screen.findByRole("heading", { level: 1, name: T.TITLE })).toBeInTheDocument();
    expect(screen.getByText(T.EYEBROW)).toBeInTheDocument();
    expect(screen.getByText(T.LEDE)).toBeInTheDocument();
    expect(screen.getByText(T.NOTE)).toBeInTheDocument();
    expect(screen.getByRole("link", { name: T.NEW_FROM_SCRATCH })).toHaveAttribute("href", "/runs/new");
    expect(screen.getByRole("region", { name: T.LIST_ARIA })).toBeInTheDocument();
  });

  it("a row shows name, description, who it is for, what it includes, when and which revision", async () => {
    mount({ templates: [MINE], publish: MEMBER });
    const row = within(await screen.findByText("Nightly tests").then((n) => n.closest("li") as HTMLElement));
    expect(row.getByText("Runs the suite overnight.")).toBeInTheDocument();
    expect(row.getByText(T.SCOPE_ME)).toBeInTheDocument();
    for (const part of [T.PART_RUN, T.PART_WHERE, T.PART_FILES, T.PART_LIMITS]) expect(row.getByText(part)).toBeInTheDocument();
    expect(row.queryByText(T.PART_ACCESS)).toBeNull();
    expect(row.getByText(T.REVISION(2))).toBeInTheDocument();
    expect(row.queryByText(T.POLICY_BADGE)).toBeNull();
  });

  it("a policy carries the Policy badge and a missing dependency says Needs setup with the field named", async () => {
    mount({ templates: [ORG], publish: MEMBER });
    const row = within(await screen.findByText("Organisation baseline").then((n) => n.closest("li") as HTMLElement));
    expect(row.getByText(T.POLICY_BADGE)).toBeInTheDocument();
    expect(row.getByText(T.NEEDS_SETUP("Model provider"))).toBeInTheDocument();
  });

  it("names who can change a shared row: organisation admins, or the group's template managers, never a person", async () => {
    mount({ templates: [MINE, ORG, GROUP], publish: MEMBER });
    await screen.findByText("Nightly tests");
    expect(within(rowOf("Organisation baseline")).getByText(T.ORG_ADMINS)).toBeInTheDocument();
    const group = within(rowOf("Package registries"));
    expect(group.getByText(T.GROUP_LABEL("Platform"))).toBeInTheDocument();
    expect(group.getByText(T.TEMPLATE_MANAGERS)).toBeInTheDocument();
    expect(within(rowOf("Nightly tests")).queryByText(T.TEMPLATE_MANAGERS)).toBeNull();
  });

  it("Use opens New run at review with the template and the revision the row showed", async () => {
    mount({ templates: [MINE], publish: MEMBER });
    const use = await screen.findByRole("link", { name: T.USE });
    expect(use).toHaveAttribute("href", `/runs/new?template=${ID(1)}&rev=2`);
    expect(use).toHaveAccessibleDescription("Nightly tests");
  });

  it("renders the fixture scenario's list", async () => {
    mount(SCENARIO_TEMPLATES.admin, "/admin/templates");
    expect(await screen.findByText("Nightly tests")).toBeInTheDocument();
    expect(screen.getByText("Organisation baseline")).toBeInTheDocument();
  });
});

describe("Templates screen: filters and empty states", () => {
  const all: TemplateList = { templates: [MINE, ORG, GROUP], publish: MEMBER };

  it("filters by who it is available to", async () => {
    mount(all);
    await screen.findByText("Nightly tests");
    const scopes = screen.getByRole("group", { name: T.SCOPE_FILTER_ARIA });
    await userEvent.click(within(scopes).getByRole("button", { name: T.SCOPE_GROUPS }));
    expect(screen.queryByText("Nightly tests")).toBeNull();
    expect(screen.getByText("Package registries")).toBeInTheDocument();
    await userEvent.click(within(scopes).getByRole("button", { name: T.SCOPE_ORG }));
    expect(screen.getByText("Organisation baseline")).toBeInTheDocument();
    expect(screen.queryByText("Package registries")).toBeNull();
    await userEvent.click(within(scopes).getByRole("button", { name: T.SCOPE_ME }));
    expect(screen.getByText("Nightly tests")).toBeInTheDocument();
    expect(within(scopes).getByRole("button", { name: T.SCOPE_ME })).toHaveAttribute("aria-pressed", "true");
  });

  it("filters by kind and by name, and the filters combine", async () => {
    mount(all);
    await screen.findByText("Nightly tests");
    const kinds = screen.getByRole("group", { name: T.KIND_FILTER_ARIA });
    await userEvent.click(within(kinds).getByRole("button", { name: T.KIND_POLICIES }));
    expect(screen.queryByText("Nightly tests")).toBeNull();
    expect(screen.getByText("Organisation baseline")).toBeInTheDocument();
    await userEvent.click(within(kinds).getByRole("button", { name: T.KIND_TEMPLATES }));
    expect(screen.queryByText("Organisation baseline")).toBeNull();
    await userEvent.type(screen.getByRole("searchbox", { name: T.SEARCH_LABEL }), "REGISTR");
    expect(screen.queryByText("Nightly tests")).toBeNull();
    expect(screen.getByText("Package registries")).toBeInTheDocument();
  });

  it("nothing matching says so and Clear filters restores the list", async () => {
    mount(all);
    await screen.findByText("Nightly tests");
    await userEvent.type(screen.getByRole("searchbox", { name: T.SEARCH_LABEL }), "zzz");
    expect(screen.getByText(T.NO_MATCH_TITLE)).toBeInTheDocument();
    expect(screen.getByText(T.NO_MATCH_BODY)).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: T.CLEAR_FILTERS }));
    expect(screen.getByText("Nightly tests")).toBeInTheDocument();
    expect(screen.getByRole("searchbox", { name: T.SEARCH_LABEL })).toHaveValue("");
    expect(within(screen.getByRole("group", { name: T.SCOPE_FILTER_ARIA })).getByRole("button", { name: T.SCOPE_ALL })).toHaveAttribute("aria-pressed", "true");
  });

  it("no templates at all: the empty state carries the action that fills it", async () => {
    mount({ templates: [], publish: MEMBER });
    expect(await screen.findByText(T.EMPTY_TITLE)).toBeInTheDocument();
    expect(screen.getByText(T.EMPTY_BODY)).toBeInTheDocument();
    expect(screen.getByRole("link", { name: T.EMPTY_ACTION })).toHaveAttribute("href", "/runs/new");
  });

  it("a failed read is an error with Retry, not an empty list", async () => {
    api.list.mockRejectedValueOnce(new HttpError(501, "Templates are not available on this server yet.", "templates_unavailable"));
    render(
      <MemoryRouter>
        <TemplatesScreen />
      </MemoryRouter>,
    );
    expect(await screen.findByText("Templates are not available on this server yet.")).toBeInTheDocument();
    expect(screen.queryByText(T.EMPTY_TITLE)).toBeNull();
    api.list.mockResolvedValue({ templates: [MINE], publish: MEMBER });
    await userEvent.click(screen.getByRole("button", { name: /retry/i }));
    expect(await screen.findByText("Nightly tests")).toBeInTheDocument();
  });
});

describe("Templates screen: who can change a row", () => {
  it("a row the caller cannot write keeps Rename and Delete focusable but inert, with the reason behind a tap-open info tip", async () => {
    mount({ templates: [ORG, GROUP], publish: MEMBER });
    await screen.findByText("Organisation baseline");
    const org = within(rowOf("Organisation baseline"));
    expect(org.getByText(T.WHY_ORG)).toBeInTheDocument();
    for (const name of [T.RENAME, T.DELETE]) {
      const b = org.getByRole("button", { name });
      expect(b).toHaveAttribute("aria-disabled", "true");
      expect(b).not.toBeDisabled();
      expect(b).toHaveAccessibleDescription(expect.stringContaining(T.WHY_ORG));
      expect(b).not.toHaveAttribute("title");
    }
    await userEvent.click(org.getByRole("button", { name: NR_RUN.ABOUT_ARIA("Organisation baseline") }));
    expect(await screen.findByRole("dialog")).toHaveTextContent(T.WHY_ORG);
    await userEvent.keyboard("{Escape}");
    await userEvent.click(org.getByRole("button", { name: T.RENAME }));
    await userEvent.click(org.getByRole("button", { name: T.DELETE }));
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(screen.queryByRole("alertdialog")).toBeNull();
    expect(within(rowOf("Package registries")).getByText(T.WHY_GROUP(T.GROUP_LABEL("Platform")))).toBeInTheDocument();
  });

  it("a writable row has live Rename and Delete and no reason line", async () => {
    mount({ templates: [MINE], publish: MEMBER });
    const row = within(await screen.findByText("Nightly tests").then((n) => n.closest("li") as HTMLElement));
    expect(row.getByRole("button", { name: T.RENAME })).not.toHaveAttribute("aria-disabled");
    expect(row.getByRole("button", { name: T.DELETE })).not.toHaveAttribute("aria-disabled");
    expect(row.queryByText(T.WHY_ORG)).toBeNull();
  });

  it("the Admin view lets an organisation admin change an organisation row", async () => {
    mount({ templates: [{ ...ORG, writable: true }], publish: ADMIN }, "/admin/templates");
    const row = within(await screen.findByText("Organisation baseline").then((n) => n.closest("li") as HTMLElement));
    expect(row.getByRole("button", { name: T.RENAME })).not.toHaveAttribute("aria-disabled");
    expect(row.queryByText(T.WHY_ORG)).toBeNull();
  });
});

describe("Templates screen: Publish to organisation (A9)", () => {
  it("is offered in the Admin view on rows that are not already the organisation's", async () => {
    mount({ templates: [MINE, { ...ORG, writable: true }], publish: ADMIN }, "/admin/templates");
    await screen.findByText("Nightly tests");
    expect(within(rowOf("Nightly tests")).getByRole("button", { name: T.PUBLISH })).toBeInTheDocument();
    expect(within(rowOf("Organisation baseline")).queryByRole("button", { name: T.PUBLISH })).toBeNull();
  });

  it("is not offered in the User view, even when the server says the caller could publish", async () => {
    mount({ templates: [MINE], publish: ADMIN }, "/templates");
    await screen.findByText("Nightly tests");
    expect(screen.queryByRole("button", { name: T.PUBLISH })).toBeNull();
  });

  it("is not offered to a caller who may not publish to the organisation", async () => {
    mount({ templates: [MINE], publish: MEMBER }, "/admin/templates");
    await screen.findByText("Nightly tests");
    expect(screen.queryByRole("button", { name: T.PUBLISH })).toBeNull();
  });

  it("copies the row to the organisation and reloads; a refusal shows the server's sentence", async () => {
    mount({ templates: [MINE], publish: ADMIN }, "/admin/templates");
    await userEvent.click(await screen.findByRole("button", { name: T.PUBLISH }));
    expect(api.publishToOrg).toHaveBeenCalledWith(ID(1));
    await waitFor(() => expect(api.list).toHaveBeenCalledTimes(2));

    api.publishToOrg.mockRejectedValueOnce(new HttpError(409, "Another template available to the same people already uses this name.", "template_name_taken"));
    await userEvent.click(screen.getByRole("button", { name: T.PUBLISH }));
    await waitFor(() => expect(toast.error).toHaveBeenCalledWith("Another template available to the same people already uses this name."));
  });
});

describe("Templates screen: Delete", () => {
  it("asks first, names the audience, and deletes only on confirm", async () => {
    mount({ templates: [MINE], publish: MEMBER });
    await userEvent.click(await screen.findByRole("button", { name: T.DELETE }));
    const dialog = await screen.findByRole("alertdialog");
    expect(within(dialog).getByText(T.DELETE_TITLE("Nightly tests"))).toBeInTheDocument();
    expect(within(dialog).getByText(T.DELETE_BODY_ME)).toBeInTheDocument();
    expect(api.remove).not.toHaveBeenCalled();
    await userEvent.click(within(dialog).getByRole("button", { name: T.CANCEL }));
    expect(api.remove).not.toHaveBeenCalled();
    expect(screen.queryByRole("alertdialog")).toBeNull();

    api.list.mockResolvedValue({ templates: [], publish: MEMBER });
    await userEvent.click(screen.getByRole("button", { name: T.DELETE }));
    await userEvent.click(within(await screen.findByRole("alertdialog")).getByRole("button", { name: T.DELETE_CONFIRM }));
    await waitFor(() => expect(api.remove).toHaveBeenCalledWith(ID(1)));
    expect(toast.success).toHaveBeenCalledWith(T.DELETED("Nightly tests"));
    expect(await screen.findByText(T.EMPTY_TITLE)).toBeInTheDocument();
    expect(screen.queryByRole("alertdialog")).toBeNull();
  });

  it("a shared row says the people in its audience can no longer start from it", async () => {
    mount({ templates: [{ ...GROUP, writable: true }], publish: MEMBER });
    await userEvent.click(await screen.findByRole("button", { name: T.DELETE }));
    expect(within(await screen.findByRole("alertdialog")).getByText(T.DELETE_BODY_SCOPE(T.GROUP_LABEL("Platform")))).toBeInTheDocument();
  });

  it("a refused delete keeps the dialog open with the server's sentence", async () => {
    api.remove.mockRejectedValueOnce(new HttpError(404, "That template does not exist, or you cannot see it.", "template_not_found"));
    mount({ templates: [MINE], publish: MEMBER });
    await userEvent.click(await screen.findByRole("button", { name: T.DELETE }));
    await userEvent.click(within(await screen.findByRole("alertdialog")).getByRole("button", { name: T.DELETE_CONFIRM }));
    expect(await screen.findByText("That template does not exist, or you cannot see it.")).toBeInTheDocument();
    expect(screen.getByRole("alertdialog")).toBeInTheDocument();
    expect(toast.success).not.toHaveBeenCalled();
  });
});

describe("Templates screen: Rename", () => {
  it("opens the dialog on the row and, once renamed, confirms and reloads", async () => {
    api.getRevision.mockResolvedValue({ description: "", document: { api_version: "wardyn/v1", kind: "RunTemplate", intent: {} } });
    api.update.mockResolvedValue({ name: "Weekly tests" });
    mount({ templates: [MINE], publish: MEMBER });
    await userEvent.click(await screen.findByRole("button", { name: T.RENAME }));
    const name = await screen.findByRole("textbox", { name: T.NAME_LABEL });
    await userEvent.clear(name);
    await userEvent.type(name, "Weekly tests");
    await userEvent.click(screen.getByRole("button", { name: T.RENAME_CONFIRM }));
    await waitFor(() => expect(toast.success).toHaveBeenCalledWith(T.RENAMED("Weekly tests")));
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(api.list).toHaveBeenCalledTimes(2);
  });
});

describe("Templates screen: canon", () => {
  expectNoOwnCopy("src/app/components/screens/templates", ["templates-screen.tsx", "template-row.tsx", "rename-template-dialog.tsx", "template-display.ts"]);
});
