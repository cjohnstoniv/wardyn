/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The Tools step's Wall preset control (boards WP-2) and its line: what is offered, what a pick sends, and that a
// preset that is gone stays chosen as an error rather than falling back to Open.
import * as React from "react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { WallPreset, WallPresetList } from "../../../../../lib/types/wall-presets";
import { WALL_PRESETS as W } from "../../../../wardyn/copy/wall-presets";
import { WallPresetApplyLine, wallPresetClauses } from "./totals";
import { chosenPreset, presetUnavailable, WallPresetPicker } from "./wall-preset-picker";

const user = userEvent.setup();
const GH = ["code_read", "issues_read"];
const ADO = ["code_read", "work_read"];
const SEALED: WallPreset = {
  id: "sealed", scope: "builtin", name: "Sealed", version: 1,
  spec: { allowed_domains: [], first_use_approval: "always_deny", desktop: { paste_in: "blocked", copy_out: "blocked", upload: "blocked", download: "blocked" }, github_capabilities: GH, azure_devops_capabilities: ADO, push_rules: { deny_all: true } },
};
const LIST: WallPresetList = {
  presets: [
    { id: "open", scope: "builtin", name: "Open", spec: {}, version: 1 },
    { id: "read-only", scope: "builtin", name: "Read-only", version: 1, spec: { github_capabilities: GH, azure_devops_capabilities: ADO, push_rules: { deny_all: true } } },
    SEALED,
    { id: "no-network", scope: "builtin", name: "No network", version: 1, spec: { ...SEALED.spec } },
    { id: "org-1", scope: "org", name: "Docs triage", description: "Keeps read capabilities and only setup sites.", spec: { allowed_domains: [] }, version: 4 },
    { id: "grp-1", scope: "group", group_id: "g1", name: "Release freeze", spec: { push_rules: { deny_all: true } }, version: 2 },
    { id: "me-1", scope: "person", name: "No pastebins", spec: { denied_domains: ["paste.ee"] }, version: 1 },
  ],
  publish: { org: false, groups: [], groups_unverified: false, group_labels: { g1: "Platform" } },
};

afterEach(cleanup);

describe("WallPresetPicker", () => {
  const show = (value: string | undefined, over: Partial<Parameters<typeof WallPresetPicker>[0]> = {}) => {
    const onChange = vi.fn();
    render(<WallPresetPicker list={LIST} value={value} onChange={onChange} {...over} />);
    return onChange;
  };

  it("offers Open, Read-only, Sealed and No network with Open chosen by default, and says what the choice does", () => {
    show(undefined);
    const group = within(screen.getByRole("group", { name: W.CONTROL }));
    expect(group.getAllByRole("button").map((b) => b.textContent)).toEqual(["Open", "Read-only", "Sealed", "No network"]);
    expect(group.getByRole("button", { name: "Open" })).toHaveAttribute("aria-pressed", "true");
    expect(screen.getByText(W.CONTROL_HELP)).toBeInTheDocument();
  });

  it("stacks the options on a phone so No network is not cut off, and sends its id on a pick", async () => {
    const onChange = show(undefined);
    const group = screen.getByRole("group", { name: W.CONTROL });
    expect(group.firstElementChild).toHaveClass("max-[399px]:flex-col");
    await user.click(screen.getByRole("button", { name: "No network" }));
    expect(onChange).toHaveBeenCalledWith("no-network");
  });

  it("shows No network as the chosen built-in, not as a custom pill", () => {
    show("no-network");
    expect(screen.getByRole("button", { name: "No network" })).toHaveAttribute("aria-pressed", "true");
    expect(screen.queryByTestId("wall-preset-pill")).toBeNull();
  });

  it("sends the built-in's id on a pick, including Open as a deliberate choice", async () => {
    const onChange = show(undefined);
    await user.click(screen.getByRole("button", { name: "Sealed" }));
    await user.click(screen.getByRole("button", { name: "Open" }));
    expect(onChange.mock.calls).toEqual([["sealed"], ["open"]]);
  });

  it("lists the organisation's, a group's and the person's presets under More presets, each with its description", async () => {
    const onChange = show("open");
    await user.click(screen.getByRole("button", { name: new RegExp(W.MORE) }));
    expect(screen.getAllByRole("heading", { level: 4 }).map((h) => h.textContent)).toEqual([W.ORGANISATION, "Platform group", W.YOURS]);
    expect(screen.getByText("Keeps read capabilities and only setup sites.")).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: /Release freeze/ }));
    expect(onChange).toHaveBeenCalledWith("grp-1");
  });

  it("shows a chosen custom preset as a pill with its audience, and clearing it goes back to Open", async () => {
    const onChange = show("org-1");
    const pill = screen.getByTestId("wall-preset-pill");
    expect(pill).toHaveTextContent("Docs triage · organisation");
    expect(within(screen.getByRole("group", { name: W.CONTROL })).queryByRole("button", { pressed: true })).toBeNull();
    await user.click(within(pill).getByRole("button", { name: W.CLEAR_PRESET("Docs triage") }));
    expect(onChange).toHaveBeenCalledWith("open");
  });

  it("returns focus to More presets after a saved preset is chosen, and to Open after the pill is cleared", async () => {
    // A controlled parent: the pill shows once a saved preset is chosen.
    function Host() {
      const [value, setValue] = React.useState<string | undefined>("open");
      return <WallPresetPicker list={LIST} value={value} onChange={setValue} />;
    }
    render(<Host />);
    await user.click(screen.getByRole("button", { name: new RegExp(W.MORE) }));
    await user.click(screen.getByRole("button", { name: /Release freeze/ }));
    expect(screen.getByRole("button", { name: new RegExp(W.MORE) })).toHaveFocus();
    expect(screen.getByTestId("wall-preset-pill")).toHaveTextContent("Release freeze");
    // The same by keyboard: Enter on the focused list button.
    await user.click(screen.getByRole("button", { name: new RegExp(W.MORE) }));
    screen.getByRole("button", { name: /No pastebins/ }).focus();
    await user.keyboard("{Enter}");
    expect(screen.getByRole("button", { name: new RegExp(W.MORE) })).toHaveFocus();
    const clear = screen.getByRole("button", { name: W.CLEAR_PRESET("No pastebins") });
    expect(clear.className).toContain("size-6");
    clear.focus();
    await user.keyboard("{Enter}");
    expect(screen.getByRole("button", { name: "Open" })).toHaveFocus();
    expect(screen.queryByTestId("wall-preset-pill")).toBeNull();
  });

  it("keeps an unavailable preset chosen as an error, presses nothing, and offers no fallback to Open", () => {
    show("org-9", { unavailable: { name: "Docs triage", audience: "organisation" } });
    expect(screen.getByRole("alert")).toHaveTextContent("Docs triage is no longer available. Choose another wall preset to continue.");
    expect(screen.getByTestId("wall-preset-pill")).toHaveTextContent("Docs triage · organisation");
    expect(within(screen.getByRole("group", { name: W.CONTROL })).queryByRole("button", { pressed: true })).toBeNull();
  });

  it("names an unavailable preset it knows nothing of without inventing one", () => {
    show("org-9", { unavailable: { name: "", audience: "" } });
    expect(screen.getByRole("alert")).toHaveTextContent(W.UNAVAILABLE(W.UNAVAILABLE_UNNAMED));
  });
});

describe("chosenPreset and presetUnavailable", () => {
  it("finds the chosen preset, and says unavailable for a launch's gone mark or a list that lacks it, never for Open or none", () => {
    expect(chosenPreset(LIST, "sealed")?.name).toBe("Sealed");
    expect(chosenPreset(LIST, undefined)).toBeUndefined();
    expect(presetUnavailable(LIST, "org-9", undefined)).toBe(true);
    expect(presetUnavailable(LIST, "org-1", { id: "org-1", name: "Docs triage" })).toBe(true);
    expect(presetUnavailable(LIST, "org-1", undefined)).toBe(false);
    expect(presetUnavailable(LIST, "open", undefined)).toBe(false);
    expect(presetUnavailable(LIST, undefined, undefined)).toBe(false);
    // The list has not been read: nothing is claimed gone.
    expect(presetUnavailable(undefined, "org-1", undefined)).toBe(false);
  });
});

describe("the preset line", () => {
  it("says what Sealed narrows, in the walls line's clauses", () => {
    render(<WallPresetApplyLine name="Sealed" spec={SEALED.spec} />);
    expect(screen.getByTestId("wall-preset-apply")).toHaveTextContent(
      "The Sealed wall preset applies: only the sites the run's setup needs, anything unlisted refused, no paste in, copy out, upload or download, read capabilities only, every push refused.",
    );
  });

  it("names the forge for a list that covers one, and draws nothing for Open", () => {
    expect(wallPresetClauses({ github_capabilities: GH })).toEqual([W.CLAUSE_FORGE_READ("GitHub")]);
    expect(wallPresetClauses({ azure_devops_capabilities: ADO })).toEqual([W.CLAUSE_FORGE_READ("Azure DevOps")]);
    expect(wallPresetClauses({ github_capabilities: ["code_write"] })).toEqual([]);
    const { container } = render(<WallPresetApplyLine name="Open" spec={{}} />);
    expect(container).toBeEmptyDOMElement();
  });
});
