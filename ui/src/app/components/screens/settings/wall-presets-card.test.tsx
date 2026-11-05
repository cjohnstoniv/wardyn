/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The Wall presets card (boards WP-1): what the list shows and where each action is offered, and what a save sends.
// The two api modules are mocked at their routes.
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { setField } from "../../../../test/set-field";
import { HttpError } from "../../../lib/api/core";
import type { WallPreset, WallPresetList } from "../../../lib/types/wall-presets";
import { WALL_PRESETS as W } from "../../wardyn/copy/wall-presets";

const listMock = vi.fn<() => Promise<WallPresetList>>();
const createMock = vi.fn<(req: unknown) => Promise<WallPreset>>();
const updateMock = vi.fn<(id: string, req: unknown) => Promise<WallPreset>>();
const removeMock = vi.fn<(id: string) => Promise<void>>();
vi.mock("../../../lib/api/wall-presets", async (orig) => ({
  ...(await orig<typeof import("../../../lib/api/wall-presets")>()),
  wallPresets: { list: () => listMock(), create: (r: unknown) => createMock(r), update: (id: string, r: unknown) => updateMock(id, r), remove: (id: string) => removeMock(id) },
}));
const wallsGet = vi.fn();
vi.mock("../../../lib/api/walls", async (orig) => ({ ...(await orig<typeof import("../../../lib/api/walls")>()), walls: { get: () => wallsGet() } }));

import { WallPresetsCard } from "./wall-presets-card";

const user = userEvent.setup();
const GH = ["code_read", "issues_read", "actions_read", "packages_read", "org_read", "identity"];
const ADO = ["code_read", "work_read", "wiki_read", "build_read", "release_read", "serviceendpoint_read", "library_read", "packaging_read", "test_read", "project_read", "identity_read"];
const BUILTINS: WallPreset[] = [
  { id: "open", scope: "builtin", name: "Open", description: "Your policy as it stands.", spec: {}, version: 1 },
  { id: "read-only", scope: "builtin", name: "Read-only", description: "Reads only.", version: 1, spec: { github_capabilities: GH, azure_devops_capabilities: ADO, push_rules: { deny_all: true } } },
  {
    id: "sealed", scope: "builtin", name: "Sealed", description: "Sealed.", version: 1,
    spec: { allowed_domains: [], first_use_approval: "always_deny", desktop: { paste_in: "blocked", copy_out: "blocked", upload: "blocked", download: "blocked" }, github_capabilities: GH, azure_devops_capabilities: ADO, push_rules: { deny_all: true } },
  },
];
const ORG: WallPreset = { id: "org-1", scope: "org", name: "Docs triage", description: "Docs only.", spec: { allowed_domains: [], github_capabilities: ["code_read"] }, version: 4, updated_by: "Ana Ruiz", updated_at: new Date(Date.now() - 2 * 86400000).toISOString() };
const GROUP: WallPreset = { id: "grp-1", scope: "group", group_id: "g-platform", name: "Release freeze", spec: { push_rules: { deny_all: true }, min_confinement_class: "CC2" }, version: 2, updated_by: "Sam Okafor", updated_at: new Date(Date.now() - 86400000).toISOString() };
const MINE: WallPreset = { id: "mine-1", scope: "person", name: "No pastebins", description: "Keeps paste sites out.", spec: { denied_domains: ["paste.example.net", "paste.ee"] }, version: 3 };
const publishOf = (over: Partial<WallPresetList["publish"]> = {}): WallPresetList["publish"] => ({ org: false, groups: [], groups_unverified: false, ...over });
const listOf = (presets: WallPreset[], publish = publishOf()): WallPresetList => ({ presets: [...BUILTINS, ...presets], publish });

const open = async () => {
  await user.click(await screen.findByRole("button", { name: /^Wall presets/ }));
  await screen.findByText(W.NEW_PRESET);
};
const row = (id: string) => within(screen.getByTestId(`wall-preset-${id}`));

beforeEach(() => {
  wallsGet.mockResolvedValue({ spec: {} });
  listMock.mockResolvedValue(listOf([ORG, GROUP, MINE], publishOf({ groups: [], group_labels: { "g-platform": "Platform" } })));
  createMock.mockResolvedValue(MINE);
  updateMock.mockResolvedValue(MINE);
  removeMock.mockResolvedValue();
});
afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

describe("Wall presets card", () => {
  it("is collapsed with its summary, then lists Built in, Organisation, a Group, Yours in that order", async () => {
    render(<WallPresetsCard />);
    expect(await screen.findByText(W.SUMMARY)).toBeInTheDocument();
    await open();
    const headings = screen.getAllByRole("heading", { level: 4 }).map((h) => h.textContent);
    expect(headings).toEqual([W.BUILT_IN, W.ORGANISATION, "Platform group", W.YOURS]);
  });

  it("shows the knob chips, and the version and last saver on shared rows only", async () => {
    render(<WallPresetsCard />);
    await open();
    expect(row("open").getByText(W.CHIP_OPEN)).toBeInTheDocument();
    for (const chip of [W.CHIP_SETUP_ONLY, W.CHIP_UNLISTED_REFUSED, W.CHIP_NO_TRANSFERS, W.CHIP_READ_ONLY, W.CHIP_EVERY_PUSH]) expect(row("sealed").getByText(chip)).toBeInTheDocument();
    // One forge's list is named by that forge, never as an unqualified read-only.
    expect(row("org-1").getByText(W.CHIP_FORGE_READ("GitHub"))).toBeInTheDocument();
    expect(row("org-1").queryByText(W.CHIP_READ_ONLY)).not.toBeInTheDocument();
    expect(row("grp-1").getByText(W.CHIP_BARRIER("Wall"))).toBeInTheDocument();
    expect(row("mine-1").getByText(W.CHIP_DENIED(2))).toBeInTheDocument();
    expect(row("org-1").getByText(/^Version 4 · saved by Ana Ruiz, 2d ago$/)).toBeInTheDocument();
    expect(row("mine-1").getByText("Version 3")).toBeInTheDocument();
  });

  it("opens a built-in's knobs: the pushes it refuses and the full read lists, and leaves the rest as not narrowed", async () => {
    render(<WallPresetsCard />);
    await open();
    expect(row("open").queryByRole("button", { name: W.SHOW })).not.toBeInTheDocument();
    await user.click(row("read-only").getByRole("button", { name: W.SHOW }));
    const detail = within(screen.getByTestId("wall-preset-detail"));
    expect(detail.getByText(W.PUSH_EVERY_REFUSED)).toBeInTheDocument();
    for (const c of [...GH, ...ADO]) expect(detail.getAllByText(c).length).toBeGreaterThan(0);
    expect(detail.getAllByText(W.DETAIL_NOT_NARROWED).length).toBeGreaterThanOrEqual(3);
    await user.click(row("read-only").getByRole("button", { name: W.HIDE }));
    expect(screen.queryByTestId("wall-preset-detail")).not.toBeInTheDocument();
  });

  it("offers Edit and Delete only where the caller may write, and Copy to mine on every row", async () => {
    render(<WallPresetsCard />);
    await open();
    for (const id of ["open", "read-only", "sealed", "org-1", "grp-1"]) {
      expect(row(id).queryByRole("button", { name: W.EDIT })).not.toBeInTheDocument();
      expect(row(id).queryByRole("button", { name: W.DELETE })).not.toBeInTheDocument();
    }
    expect(row("mine-1").getByRole("button", { name: W.EDIT })).toBeInTheDocument();
    expect(row("mine-1").getByRole("button", { name: W.DELETE })).toBeInTheDocument();
    expect(screen.getAllByRole("button", { name: W.COPY_TO_MINE })).toHaveLength(6);
  });

  it("makes the organisation's rows editable in the Admin view, and a managed group's for its manager", async () => {
    listMock.mockResolvedValue(listOf([ORG, GROUP], publishOf({ org: true, groups: ["g-platform"] })));
    render(<WallPresetsCard admin />);
    await open();
    expect(row("org-1").getByRole("button", { name: W.EDIT })).toBeInTheDocument();
    expect(row("grp-1").getByRole("button", { name: W.DELETE })).toBeInTheDocument();
  });

  it("Copy to mine from an org preset prefills the form, suffixes the name, defaults the audience to Only me, and creates a person preset", async () => {
    render(<WallPresetsCard />);
    await open();
    await user.click(row("org-1").getByRole("button", { name: W.COPY_TO_MINE }));
    const dialog = within(await screen.findByRole("dialog"));
    expect(dialog.getByRole("heading", { name: W.DIALOG_COPY })).toBeInTheDocument();
    expect(dialog.getByLabelText(/^Name/)).toHaveValue("Docs triage (copy)");
    expect(dialog.getByLabelText(W.DESCRIPTION)).toHaveValue("Docs only.");
    expect(dialog.getByLabelText(W.AUDIENCE)).toHaveValue("person");
    expect(dialog.getByRole("radio", { name: "Only these sites" })).toBeChecked();
    // The capability list is a control: Only these capabilities, one box checked.
    expect(within(dialog.getByTestId("walls-github")).getByRole("checkbox", { name: /Clone, fetch and browse/ })).toBeChecked();
    await user.click(dialog.getByRole("button", { name: W.SAVE }));
    await waitFor(() =>
      expect(createMock).toHaveBeenCalledWith({
        name: "Docs triage (copy)", description: "Docs only.", scope: "person",
        spec: { allowed_domains: [], github_capabilities: ["code_read"] },
      }),
    );
    expect(await screen.findByRole("status")).toHaveTextContent(W.SAVED);
  });

  it("Copy to mine from Open says a limit must be added, and keeps Save off until one is", async () => {
    render(<WallPresetsCard />);
    await open();
    await user.click(row("open").getByRole("button", { name: W.COPY_TO_MINE }));
    const dialog = within(await screen.findByRole("dialog"));
    expect(dialog.getByText(W.COPY_OPEN)).toBeInTheDocument();
    expect(dialog.getByRole("button", { name: W.SAVE })).toBeDisabled();
    await user.click(dialog.getByRole("switch", { name: "Refuse every push" }));
    expect(dialog.queryByText(W.COPY_OPEN)).not.toBeInTheDocument();
    expect(dialog.getByRole("button", { name: W.SAVE })).toBeEnabled();
    await user.click(dialog.getByRole("button", { name: W.SAVE }));
    await waitFor(() => expect(createMock).toHaveBeenCalledWith({ name: "Open (copy)", description: "Your policy as it stands.", scope: "person", spec: { push_rules: { deny_all: true } } }));
  });

  it("New preset offers only where the caller may publish: Only me, a managed group, and Organisation in the Admin view", async () => {
    listMock.mockResolvedValue(listOf([], publishOf({ org: true, groups: ["g-platform"], group_labels: { "g-platform": "Platform" } })));
    render(<WallPresetsCard admin />);
    await open();
    await user.click(screen.getByRole("button", { name: W.NEW_PRESET }));
    const dialog = within(await screen.findByRole("dialog"));
    const select = dialog.getByLabelText(W.AUDIENCE) as HTMLSelectElement;
    expect([...select.options].map((o) => o.textContent)).toEqual([W.ONLY_ME, "Organisation", "Platform group"]);
    expect(dialog.getByText(W.HELP_ORG)).toBeInTheDocument();
    setField(dialog.getByLabelText(/^Name/), "Org wide");
    await user.selectOptions(select, "group:g-platform");
    await user.click(dialog.getByRole("switch", { name: "Refuse every push" }));
    await user.click(dialog.getByRole("button", { name: W.SAVE }));
    await waitFor(() => expect(createMock).toHaveBeenCalledWith({ name: "Org wide", scope: "group", group_id: "g-platform", spec: { push_rules: { deny_all: true } } }));
  });

  it("says groups are unavailable when the sign-in's group list is incomplete", async () => {
    listMock.mockResolvedValue(listOf([], publishOf({ groups: ["g-platform"], groups_unverified: true })));
    render(<WallPresetsCard />);
    await open();
    await user.click(screen.getByRole("button", { name: W.NEW_PRESET }));
    const dialog = within(await screen.findByRole("dialog"));
    expect([...(dialog.getByLabelText(W.AUDIENCE) as HTMLSelectElement).options].map((o) => o.textContent)).toEqual([W.ONLY_ME]);
    expect(dialog.getByText("Your group membership cannot be checked from this sign-in. Sign in again, then retry.")).toBeInTheDocument();
  });

  it("Edit sends the version it read, shows the audience as plain text and the version it edits", async () => {
    render(<WallPresetsCard />);
    await open();
    await user.click(row("mine-1").getByRole("button", { name: W.EDIT }));
    const dialog = within(await screen.findByRole("dialog"));
    expect(dialog.queryByRole("combobox", { name: W.AUDIENCE })).not.toBeInTheDocument();
    expect(dialog.getByText(W.HELP_AUDIENCE_FIXED)).toBeInTheDocument();
    expect(dialog.getByText(W.EDITING_VERSION(3))).toBeInTheDocument();
    await user.click(dialog.getByRole("checkbox", { name: "Block upload" }));
    await user.click(dialog.getByRole("button", { name: W.SAVE }));
    await waitFor(() =>
      expect(updateMock).toHaveBeenCalledWith("mine-1", {
        name: "No pastebins", description: "Keeps paste sites out.", expected_version: 3,
        spec: { denied_domains: ["paste.example.net", "paste.ee"], desktop: { upload: "blocked" } },
      }),
    );
  });

  it("on a version conflict says so and Reload reopens the edit on what is stored now", async () => {
    updateMock.mockRejectedValue(new HttpError(409, "Someone saved this wall preset since you opened it; it is at version 4. Reload it, then apply your edit again.", "wall_preset_version_conflict"));
    render(<WallPresetsCard />);
    await open();
    await user.click(row("mine-1").getByRole("button", { name: W.EDIT }));
    const dialog = within(await screen.findByRole("dialog"));
    await user.click(dialog.getByRole("checkbox", { name: "Block upload" }));
    await user.click(dialog.getByRole("button", { name: W.SAVE }));
    expect(await dialog.findByText(/Someone saved this wall preset since you opened it/)).toBeInTheDocument();
    listMock.mockResolvedValue(listOf([{ ...MINE, version: 4, description: "Newer." }]));
    await user.click(dialog.getByRole("button", { name: W.RELOAD }));
    const again = within(await screen.findByRole("dialog"));
    await waitFor(() => expect(again.getByText(W.EDITING_VERSION(4))).toBeInTheDocument());
    expect(again.getByLabelText(W.DESCRIPTION)).toHaveValue("Newer.");
    expect(again.queryByText(/Someone saved/)).not.toBeInTheDocument();
  });

  it("puts a 422 sentence under the form section its wire path names, and one it cannot place above the buttons", async () => {
    createMock.mockRejectedValueOnce(new HttpError(422, 'invalid wall preset: denied_domains[1]: "https://pastebin.com" must be a DNS name such as api.example.com, or a wildcard such as *.example.com', "wall_preset_invalid"));
    render(<WallPresetsCard />);
    await open();
    await user.click(screen.getByRole("button", { name: W.NEW_PRESET }));
    const dialog = within(await screen.findByRole("dialog"));
    setField(dialog.getByLabelText(/^Name/), "Tight");
    await user.click(dialog.getByRole("switch", { name: "Refuse every push" }));
    await user.click(dialog.getByRole("button", { name: W.SAVE }));
    const alert = await dialog.findByRole("alert");
    expect(alert).toHaveTextContent('denied_domains[1]: "https://pastebin.com" must be a DNS name');
    // Under "Never these sites", not above the buttons.
    expect(alert.parentElement).toHaveTextContent("Never these sites");
    createMock.mockRejectedValueOnce(new HttpError(422, "invalid wall preset: the preset names no limit; Open is the built-in for no preset", "wall_preset_invalid"));
    await user.click(dialog.getByRole("button", { name: W.SAVE }));
    const above = await dialog.findByText("the preset names no limit; Open is the built-in for no preset");
    expect(above.parentElement?.tagName).toBe("FORM");
  });

  it("puts a taken name under the Name field, a built-in's included", async () => {
    createMock.mockRejectedValueOnce(new HttpError(409, "Sealed is the name of a built-in wall preset. Choose another name.", "wall_preset_name_taken"));
    render(<WallPresetsCard />);
    await open();
    await user.click(screen.getByRole("button", { name: W.NEW_PRESET }));
    const dialog = within(await screen.findByRole("dialog"));
    setField(dialog.getByLabelText(/^Name/), "Sealed");
    await user.click(dialog.getByRole("switch", { name: "Refuse every push" }));
    await user.click(dialog.getByRole("button", { name: W.SAVE }));
    const alert = await dialog.findByRole("alert");
    expect(alert).toHaveTextContent("Sealed is the name of a built-in wall preset.");
    expect(dialog.getByLabelText(/^Name/)).toHaveAttribute("aria-invalid", "true");
  });

  it("puts a description refusal on the Description field, with its own aria-invalid and aria-describedby, and not on Name", async () => {
    createMock.mockRejectedValueOnce(new HttpError(422, "invalid wall preset: description must be printable text of at most 512 characters", "wall_preset_invalid"));
    render(<WallPresetsCard />);
    await open();
    await user.click(screen.getByRole("button", { name: W.NEW_PRESET }));
    const dialog = within(await screen.findByRole("dialog"));
    setField(dialog.getByLabelText(/^Name/), "Tight");
    await user.click(dialog.getByRole("switch", { name: "Refuse every push" }));
    await user.click(dialog.getByRole("button", { name: W.SAVE }));
    const alert = await dialog.findByRole("alert");
    expect(alert).toHaveTextContent("description must be printable text");
    const field = dialog.getByLabelText(W.DESCRIPTION);
    expect(field).toHaveAttribute("aria-invalid", "true");
    expect(field.getAttribute("aria-describedby")).toBe(alert.id);
    expect(dialog.getByLabelText(/^Name/)).toHaveAttribute("aria-invalid", "false");
  });

  it("Delete confirms with what happens to runs and to a queued run, then deletes and reads again", async () => {
    render(<WallPresetsCard />);
    await open();
    await user.click(row("mine-1").getByRole("button", { name: W.DELETE }));
    const dialog = within(await screen.findByRole("dialog"));
    expect(dialog.getByRole("heading", { name: W.DELETE_TITLE("No pastebins") })).toBeInTheDocument();
    expect(dialog.getByText(W.DELETE_BODY)).toBeInTheDocument();
    await user.click(dialog.getByRole("button", { name: W.DELETE_CONFIRM }));
    await waitFor(() => expect(removeMock).toHaveBeenCalledWith("mine-1"));
    await waitFor(() => expect(listMock).toHaveBeenCalledTimes(2));
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
  });

  it.each([
    ["no person", () => wallsGet.mockRejectedValue(new HttpError(400, "no", "walls_need_person"))],
    ["no store", () => listMock.mockRejectedValue(new HttpError(501, "no", "wall_presets_unavailable"))],
  ])("is absent for %s", async (_name, arrange) => {
    arrange();
    const { container } = render(<WallPresetsCard />);
    await waitFor(() => expect(wallsGet).toHaveBeenCalled());
    await waitFor(() => expect(container).toBeEmptyDOMElement());
  });

  it("offers a retry when the read fails for another reason", async () => {
    listMock.mockRejectedValueOnce(new HttpError(500, "boom", ""));
    render(<WallPresetsCard />);
    await user.click(await screen.findByRole("button", { name: /^Wall presets/ }));
    await user.click(await screen.findByRole("button", { name: W.RETRY }));
    await waitFor(() => expect(listMock).toHaveBeenCalledTimes(2));
  });
});

describe("Wall presets card: every limit has a control", () => {
  it("a new preset with no limit keeps Save off and says a preset needs one", async () => {
    render(<WallPresetsCard />);
    await open();
    await user.click(screen.getByRole("button", { name: W.NEW_PRESET }));
    const dialog = within(await screen.findByRole("dialog"));
    setField(dialog.getByLabelText(/^Name/), "Tight");
    expect(dialog.getByText(W.NEEDS_A_LIMIT)).toBeInTheDocument();
    expect(dialog.getByRole("button", { name: W.SAVE })).toBeDisabled();
    expect(dialog.queryByTestId("walls-carried")).not.toBeInTheDocument();
  });

  it("Copy to mine from Read-only carries six GitHub and eleven Azure DevOps reads into the checklists", async () => {
    render(<WallPresetsCard />);
    await open();
    await user.click(row("read-only").getByRole("button", { name: W.COPY_TO_MINE }));
    const dialog = within(await screen.findByRole("dialog"));
    const on = (id: string) => within(dialog.getByTestId(id)).getAllByRole("checkbox").filter((c) => c.getAttribute("aria-checked") === "true");
    expect(on("walls-github")).toHaveLength(6);
    expect(on("walls-ado")).toHaveLength(11);
    expect(dialog.getByRole("switch", { name: "Refuse every push" })).toHaveAttribute("aria-checked", "true");
    await user.click(dialog.getByRole("button", { name: W.SAVE }));
    await waitFor(() => expect(createMock).toHaveBeenCalledTimes(1));
    const spec = (createMock.mock.lastCall![0] as { spec: Record<string, unknown> }).spec;
    expect(spec.github_capabilities).toEqual(GH);
    expect(spec.azure_devops_capabilities).toEqual(ADO);
    expect(spec.push_rules).toEqual({ deny_all: true });
  });

  it("a row's disclosure names its tool rules, paths never pushed, recording floor and idle stop, the idle one exactly", async () => {
    const full: WallPreset = {
      id: "full-1", scope: "builtin", name: "Docs fixes", version: 2,
      spec: { tool_rules: [{ tool: "WebFetch", effect: "hold" }, { tool: "mcp__slack__post_message", effect: "deny" }], push_rules: { deny_paths: [".github/workflows/"] }, recording: "terminal", auto_stop_after_sec: 90 },
    };
    listMock.mockResolvedValue(listOf([full]));
    render(<WallPresetsCard />);
    await open();
    await user.click(row("full-1").getByRole("button", { name: W.SHOW }));
    const detail = within(screen.getByTestId("wall-preset-detail"));
    expect(detail.getByText(W.DETAIL_TOOL_RULE("WebFetch", "hold"))).toBeInTheDocument();
    expect(detail.getByText(W.DETAIL_TOOL_RULE("mcp__slack__post_message", "deny"))).toBeInTheDocument();
    expect(detail.getByText(".github/workflows/")).toBeInTheDocument();
    expect(detail.getByText("Terminal")).toBeInTheDocument();
    expect(detail.getByText(W.DETAIL_IDLE_AFTER(90, "seconds"))).toBeInTheDocument();
  });

  it("puts a path refusal under Pushes in the dialog", async () => {
    createMock.mockRejectedValueOnce(new HttpError(422, 'invalid wall preset: push_rules.deny_paths[0]: "a/../b" has an empty, "." or ".." path segment, which no git path contains', "wall_preset_invalid"));
    render(<WallPresetsCard />);
    await open();
    await user.click(screen.getByRole("button", { name: W.NEW_PRESET }));
    const dialog = within(await screen.findByRole("dialog"));
    setField(dialog.getByLabelText(/^Name/), "Paths");
    await user.click(dialog.getByRole("button", { name: "Add path" }));
    setField(dialog.getByRole("textbox", { name: "Path 1" }), "a/../b");
    await user.click(dialog.getByRole("button", { name: W.SAVE }));
    const alert = await dialog.findByRole("alert");
    expect(alert).toHaveTextContent("push_rules.deny_paths[0]");
    expect(alert.parentElement).toHaveTextContent("Pushes");
  });
});
