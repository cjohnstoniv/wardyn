/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// T4-a: the Your walls card. The api module is mocked at its three routes; what is pinned is what the
// card sends (only narrowing values), what it shows back, and the strings of the owner-approved board.
import * as React from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { setField } from "../../../../test/set-field";
import { HttpError } from "../../../lib/api/core";
import type { PersonalWalls } from "../../../lib/api/walls";
import { GITHUB_CAPABILITIES, GITHUB_PUSH_NAME, YOUR_WALLS } from "../../wardyn/copy/personal-walls";
import { toForm, toSpec, WallsForm, type Form } from "./walls-form";
import { ADO_CAPABILITIES } from "../../../lib/ado-capabilities";

const getMock = vi.fn<() => Promise<PersonalWalls>>();
const putMock = vi.fn<(spec: unknown) => Promise<PersonalWalls>>();
const clearMock = vi.fn<() => Promise<void>>();
vi.mock("../../../lib/api/walls", async (orig) => ({
  ...(await orig<typeof import("../../../lib/api/walls")>()),
  walls: { get: () => getMock(), put: (s: unknown) => putMock(s), clear: () => clearMock() },
}));

import { YourWallsCard } from "./your-walls-card";

const user = userEvent.setup();
const open = async () => {
  await user.click(await screen.findByRole("button", { name: /^Your walls/ }));
  await screen.findByText(YOUR_WALLS.SITES);
};
const echo = (spec: unknown): Promise<PersonalWalls> => Promise.resolve({ spec: spec as PersonalWalls["spec"] });

beforeEach(() => {
  getMock.mockResolvedValue({ spec: {} });
  putMock.mockImplementation(echo);
  clearMock.mockResolvedValue();
});
afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

describe("Your walls card", () => {
  it("is collapsed with the lede as its summary, and opens to the board's fields", async () => {
    render(<YourWallsCard />);
    expect(await screen.findByText(YOUR_WALLS.LEDE)).toBeInTheDocument();
    expect(screen.queryByText(YOUR_WALLS.SITES)).not.toBeInTheDocument();
    await open();
    for (const text of [YOUR_WALLS.SITES_ANY, YOUR_WALLS.SITES_ONLY, YOUR_WALLS.NEVER, YOUR_WALLS.UNLISTED, YOUR_WALLS.BARRIER, YOUR_WALLS.CLIPBOARD, YOUR_WALLS.CREDENTIALS, YOUR_WALLS.RESTART_NOTE]) {
      expect(screen.getByText(text)).toBeInTheDocument();
    }
    for (const name of [YOUR_WALLS.UNLISTED_BLOCK, YOUR_WALLS.UNLISTED_ASK, YOUR_WALLS.UNLISTED_HOLD, YOUR_WALLS.SAVE, YOUR_WALLS.REMOVE_ALL]) {
      expect(screen.getByRole("button", { name })).toBeInTheDocument();
    }
    expect(screen.getByRole("radio", { name: "Fence" })).toBeInTheDocument();
    expect(screen.getByRole("radio", { name: "Wall" })).toBeInTheDocument();
    expect(screen.getByRole("radio", { name: "Vault" })).toBeInTheDocument();
  });

  // jsdom does no layout: this pins the classes that stack the 551px unlisted-sites switch in any card or dialog narrower than 35rem.
  it("the unlisted-sites switch stacks while its group is narrower than it", async () => {
    render(<YourWallsCard />);
    await open();
    const group = screen.getByRole("group", { name: YOUR_WALLS.UNLISTED });
    expect(group).toHaveClass("@container/segmented-wide");
    expect(within(group).getByRole("button", { name: YOUR_WALLS.UNLISTED_BLOCK }).parentElement).toHaveClass("@max-[35rem]/segmented-wide:flex-col");
  });

  it("shows the walls it loaded", async () => {
    getMock.mockResolvedValue({
      spec: {
        allowed_domains: ["packages.example.com"],
        denied_domains: ["paste.example.net"],
        first_use_approval: "wait_for_review",
        min_confinement_class: "CC2",
        desktop: { copy_out: "blocked", download: "blocked" },
        withheld_grants: [{ kind: "ssh_key" }, { kind: "cloud_sts" }],
      },
    });
    render(<YourWallsCard />);
    await open();
    expect(screen.getByRole("radio", { name: YOUR_WALLS.SITES_ONLY })).toBeChecked();
    expect(screen.getByText("packages.example.com")).toBeInTheDocument();
    expect(screen.getByText("paste.example.net")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: YOUR_WALLS.UNLISTED_HOLD })).toHaveAttribute("aria-pressed", "true");
    expect(screen.getByRole("radio", { name: "Wall" })).toBeChecked();
    expect(screen.getByRole("checkbox", { name: YOUR_WALLS.COPY_OUT })).toBeChecked();
    expect(screen.getByRole("checkbox", { name: YOUR_WALLS.PASTE_IN })).not.toBeChecked();
    expect(screen.getByText("SSH key")).toBeInTheDocument();
    expect(screen.getByText("Cloud credentials")).toBeInTheDocument();
  });

  it("choosing Only these sites also selects Block it, and Save sends only narrowing values", async () => {
    render(<YourWallsCard />);
    await open();
    await user.click(screen.getByRole("radio", { name: YOUR_WALLS.SITES_ONLY }));
    expect(screen.getByRole("button", { name: YOUR_WALLS.UNLISTED_BLOCK })).toHaveAttribute("aria-pressed", "true");
    const [only, never] = screen.getAllByPlaceholderText(YOUR_WALLS.ADD_SITE);
    setField(only, "api.example.com");
    fireEvent.keyDown(only, { key: "Enter" });
    setField(never, "paste.example.net");
    await user.click(screen.getByRole("checkbox", { name: YOUR_WALLS.COPY_OUT }));
    await user.click(screen.getByRole("radio", { name: "Wall" }));
    await user.selectOptions(screen.getByRole("combobox", { name: YOUR_WALLS.CREDENTIALS }), "ssh_key");
    await user.click(screen.getByRole("button", { name: YOUR_WALLS.SAVE }));
    // A site typed and not entered is still saved.
    await waitFor(() =>
      expect(putMock).toHaveBeenCalledWith({
        allowed_domains: ["api.example.com"],
        denied_domains: ["paste.example.net"],
        first_use_approval: "always_deny",
        min_confinement_class: "CC2",
        desktop: { copy_out: "blocked" },
        withheld_grants: [{ kind: "ssh_key" }],
      }),
    );
    expect(await screen.findByRole("status")).toHaveTextContent(YOUR_WALLS.SAVED);
  });

  it("going back to Any site clears the unlisted-sites choice Only these sites filled in, and keeps one the person made", async () => {
    render(<YourWallsCard />);
    await open();
    await user.click(screen.getByRole("radio", { name: YOUR_WALLS.SITES_ONLY }));
    expect(screen.getByRole("button", { name: YOUR_WALLS.UNLISTED_BLOCK })).toHaveAttribute("aria-pressed", "true");
    await user.click(screen.getByRole("radio", { name: YOUR_WALLS.SITES_ANY }));
    expect(screen.getByRole("button", { name: YOUR_WALLS.UNLISTED_BLOCK })).toHaveAttribute("aria-pressed", "false");
    await user.click(screen.getByRole("button", { name: YOUR_WALLS.SAVE }));
    await waitFor(() => expect(putMock).toHaveBeenCalledWith({}));

    // A choice the person made themselves stays when they go back to Any site.
    await user.click(screen.getByRole("radio", { name: YOUR_WALLS.SITES_ONLY }));
    await user.click(screen.getByRole("button", { name: YOUR_WALLS.UNLISTED_ASK }));
    await user.click(screen.getByRole("radio", { name: YOUR_WALLS.SITES_ANY }));
    await user.click(screen.getByRole("button", { name: YOUR_WALLS.SAVE }));
    await waitFor(() => expect(putMock).toHaveBeenLastCalledWith({ first_use_approval: "deny_with_review" }));
  });

  it("Any site sends no allowlist, and an empty form sends an empty spec", async () => {
    getMock.mockResolvedValue({ spec: { allowed_domains: ["a.example.com"] } });
    render(<YourWallsCard />);
    await open();
    await user.click(screen.getByRole("radio", { name: YOUR_WALLS.SITES_ANY }));
    await user.click(screen.getByRole("button", { name: YOUR_WALLS.SAVE }));
    await waitFor(() => expect(putMock).toHaveBeenCalledWith({}));
  });

  it("keeps an allow_all_egress: false the CLI wrote, and a scoped withheld entry", async () => {
    const scoped = { kind: "api_key", scope: { host: "x.example.com", header: "x-key" }, requires_approval: false };
    getMock.mockResolvedValue({ spec: { allow_all_egress: false, withheld_grants: [scoped] } });
    render(<YourWallsCard />);
    await open();
    await user.click(screen.getByRole("button", { name: YOUR_WALLS.SAVE }));
    await waitFor(() => expect(putMock).toHaveBeenCalledWith({ allow_all_egress: false, withheld_grants: [scoped] }));
  });

  it("keeps the capability lists, tool and push rules, recording and idle stop the CLI wrote across a save", async () => {
    const carried = {
      azure_devops_capabilities: ["work_read"],
      github_capabilities: ["code_read"],
      tool_rules: [{ tool: "Bash", effect: "hold" }],
      push_rules: { deny_paths: [".github/**"] },
      recording: "screen_actions",
      auto_stop_after_sec: 900,
    };
    getMock.mockResolvedValue({ spec: carried } as PersonalWalls);
    render(<YourWallsCard />);
    await open();
    await user.click(screen.getByRole("checkbox", { name: YOUR_WALLS.COPY_OUT }));
    await user.click(screen.getByRole("button", { name: YOUR_WALLS.SAVE }));
    await waitFor(() => expect(putMock).toHaveBeenCalledWith({ ...carried, desktop: { copy_out: "blocked" } }));
  });

  it("keeps only the host of a typed site: no scheme, path or capitals", async () => {
    render(<YourWallsCard />);
    await open();
    const never = screen.getByPlaceholderText(YOUR_WALLS.ADD_SITE);
    setField(never, "HTTPS://GitHub.com/org/repo?x=1");
    fireEvent.keyDown(never, { key: "Enter" });
    expect(screen.getByText("github.com")).toBeInTheDocument();
    expect(screen.getByText(YOUR_WALLS.SITE_HINT)).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: YOUR_WALLS.SAVE }));
    await waitFor(() => expect(putMock).toHaveBeenCalledWith({ denied_domains: ["github.com"] }));
  });

  it("can clear the barrier and the unlisted-sites choice, and says what VM desktops do with the clipboard", async () => {
    getMock.mockResolvedValue({ spec: { min_confinement_class: "CC2", first_use_approval: "wait_for_review" } });
    render(<YourWallsCard />);
    await open();
    expect(screen.getByText(YOUR_WALLS.CLIPBOARD_HINT)).toBeInTheDocument();
    await user.click(within(screen.getByRole("radiogroup", { name: YOUR_WALLS.BARRIER })).getByRole("radio", { name: YOUR_WALLS.BARRIER_NONE }));
    await user.click(screen.getByRole("button", { name: YOUR_WALLS.UNLISTED_ORG }));
    await user.click(screen.getByRole("button", { name: YOUR_WALLS.SAVE }));
    await waitFor(() => expect(putMock).toHaveBeenCalledWith({}));
  });

  it("removes a site chip", async () => {
    getMock.mockResolvedValue({ spec: { denied_domains: ["a.example.com", "b.example.com"] } });
    render(<YourWallsCard />);
    await open();
    await user.click(screen.getByRole("button", { name: YOUR_WALLS.REMOVE_SITE("a.example.com") }));
    expect(screen.queryByText("a.example.com")).not.toBeInTheDocument();
    expect(screen.getByText("b.example.com")).toBeInTheDocument();
  });

  it("keeps the host of a typed site without its port, IPv6 brackets included", async () => {
    render(<YourWallsCard />);
    await open();
    const never = screen.getByPlaceholderText(YOUR_WALLS.ADD_SITE);
    for (const typed of ["https://Example.com:8443/x", "internal.test:80", "[::1]:8080"]) {
      setField(never, typed);
      fireEvent.keyDown(never, { key: "Enter" });
    }
    await user.click(screen.getByRole("button", { name: YOUR_WALLS.SAVE }));
    await waitFor(() => expect(putMock).toHaveBeenCalledWith({ denied_domains: ["example.com", "internal.test", "[::1]"] }));
  });

  it("Remove all walls asks first, and Cancel removes nothing", async () => {
    getMock.mockResolvedValue({ spec: { denied_domains: ["a.example.com"] } });
    render(<YourWallsCard />);
    await open();
    await user.click(screen.getByRole("button", { name: YOUR_WALLS.REMOVE_ALL }));
    const dialog = await screen.findByRole("alertdialog");
    expect(within(dialog).getByText(YOUR_WALLS.REMOVE_ALL_TITLE)).toBeInTheDocument();
    expect(within(dialog).getByText(YOUR_WALLS.REMOVE_ALL_BODY)).toBeInTheDocument();
    await user.click(within(dialog).getByRole("button", { name: YOUR_WALLS.CANCEL }));
    await waitFor(() => expect(screen.queryByRole("alertdialog")).not.toBeInTheDocument());
    expect(clearMock).not.toHaveBeenCalled();
    expect(screen.getByText("a.example.com")).toBeInTheDocument();
  });

  it("Remove all walls clears the document and the form once confirmed", async () => {
    getMock.mockResolvedValue({ spec: { denied_domains: ["a.example.com"], desktop: { upload: "blocked" } } });
    render(<YourWallsCard />);
    await open();
    await user.click(screen.getByRole("button", { name: YOUR_WALLS.REMOVE_ALL }));
    const dialog = await screen.findByRole("alertdialog");
    await user.click(within(dialog).getByRole("button", { name: YOUR_WALLS.REMOVE_ALL }));
    await waitFor(() => expect(clearMock).toHaveBeenCalledTimes(1));
    expect(screen.queryByText("a.example.com")).not.toBeInTheDocument();
    expect(screen.getByRole("checkbox", { name: YOUR_WALLS.UPLOAD })).not.toBeChecked();
  });

  it("Remove all walls stays open and shows the error when the clear fails", async () => {
    getMock.mockResolvedValue({ spec: { denied_domains: ["a.example.com"] } });
    clearMock.mockRejectedValue(new HttpError(500, "could not clear", "walls_unavailable"));
    render(<YourWallsCard />);
    await open();
    await user.click(screen.getByRole("button", { name: YOUR_WALLS.REMOVE_ALL }));
    const dialog = await screen.findByRole("alertdialog");
    await user.click(within(dialog).getByRole("button", { name: YOUR_WALLS.REMOVE_ALL }));
    await waitFor(() => expect(clearMock).toHaveBeenCalledTimes(1));
    expect(await within(screen.getByRole("alertdialog")).findByRole("alert")).toHaveTextContent("could not clear");
    expect(screen.getByText("a.example.com")).toBeInTheDocument();
  });

  it("puts a refusal under the section it names, without the prefix or the reason code, and offers no Saved", async () => {
    putMock.mockRejectedValue(new HttpError(400, "invalid walls: denied_domains[0]: \"x y\" must be a DNS name", "walls_invalid"));
    render(<YourWallsCard />);
    await open();
    await user.click(screen.getByRole("button", { name: YOUR_WALLS.SAVE }));
    const alert = await screen.findByRole("alert");
    expect(alert).toHaveTextContent('denied_domains[0]: "x y" must be a DNS name');
    expect(alert).not.toHaveTextContent("invalid walls");
    expect(screen.queryByText("walls_invalid")).not.toBeInTheDocument();
    expect(alert.parentElement).toHaveTextContent(YOUR_WALLS.NEVER);
    expect(screen.queryByRole("status")).not.toBeInTheDocument();
  });

  it("puts a sentence that names no section above the buttons, as the server worded it, with its reason code", async () => {
    putMock.mockRejectedValue(new HttpError(400, "invalid walls: the document is too large", "walls_invalid"));
    render(<YourWallsCard />);
    await open();
    await user.click(screen.getByRole("button", { name: YOUR_WALLS.SAVE }));
    const alert = await screen.findByRole("alert");
    expect(alert).toHaveTextContent("the document is too large");
    expect(within(alert).getByText("walls_invalid")).toBeInTheDocument();
    expect(alert.nextElementSibling).toContainElement(screen.getByRole("button", { name: YOUR_WALLS.SAVE }));
  });

  it("Refuse every push is a switch under Pushes: it sends push_rules.deny_all true beside the paths the CLI wrote, and is not sent when off", async () => {
    getMock.mockResolvedValue({ spec: { push_rules: { deny_paths: [".github/**"] } } });
    render(<YourWallsCard />);
    await open();
    expect(screen.getByText(YOUR_WALLS.PUSHES)).toBeInTheDocument();
    expect(screen.getByText(YOUR_WALLS.REFUSE_PUSH_HELP)).toBeInTheDocument();
    const sw = screen.getByRole("switch", { name: YOUR_WALLS.REFUSE_PUSH });
    expect(sw).toHaveAttribute("aria-checked", "false");
    await user.click(sw);
    expect(sw).toHaveAttribute("aria-checked", "true");
    await user.click(screen.getByRole("button", { name: YOUR_WALLS.SAVE }));
    await waitFor(() => expect(putMock).toHaveBeenLastCalledWith({ push_rules: { deny_paths: [".github/**"], deny_all: true } }));
    await user.click(sw);
    await user.click(screen.getByRole("button", { name: YOUR_WALLS.SAVE }));
    await waitFor(() => expect(putMock).toHaveBeenLastCalledWith({ push_rules: { deny_paths: [".github/**"] } }));
  });

  it("shows the switch on when the walls it loaded refuse every push, and saves only deny_all with nothing else", async () => {
    getMock.mockResolvedValue({ spec: { push_rules: { deny_all: true } } });
    render(<YourWallsCard />);
    await open();
    expect(screen.getByRole("switch", { name: YOUR_WALLS.REFUSE_PUSH })).toHaveAttribute("aria-checked", "true");
    await user.click(screen.getByRole("button", { name: YOUR_WALLS.SAVE }));
    await waitFor(() => expect(putMock).toHaveBeenCalledWith({ push_rules: { deny_all: true } }));
  });

  it("stacks the unlisted-sites choices below 400px", async () => {
    getMock.mockResolvedValue({ spec: { github_capabilities: ["code_read"] } });
    render(<YourWallsCard />);
    await open();
    expect(screen.getByRole("button", { name: YOUR_WALLS.UNLISTED_BLOCK }).parentElement?.className).toContain("max-[399px]:flex-col");
  });

  it.each(["walls_need_person", "walls_unavailable"])("is absent when the server answers %s", async (reason) => {
    getMock.mockRejectedValue(new HttpError(reason === "walls_unavailable" ? 501 : 400, "no", reason));
    const { container } = render(<YourWallsCard />);
    await waitFor(() => expect(getMock).toHaveBeenCalled());
    await waitFor(() => expect(container).toBeEmptyDOMElement());
  });

  it("offers a retry when the read fails for another reason", async () => {
    getMock.mockRejectedValueOnce(new HttpError(500, "boom", ""));
    render(<YourWallsCard />);
    await user.click(await screen.findByRole("button", { name: /^Your walls/ }));
    await user.click(await screen.findByRole("button", { name: YOUR_WALLS.RETRY }));
    await waitFor(() => expect(getMock).toHaveBeenCalledTimes(2));
  });
});

// Every limit a walls document carries has a control: capability lists, tool rules, paths never pushed, a recording
// minimum and an idle stop. What is pinned is what each control shows and what Save sends.
describe("Your walls: every limit has a control", () => {
  const save = () => screen.getByRole("button", { name: YOUR_WALLS.SAVE });
  const sent = () => waitFor(() => expect(putMock).toHaveBeenCalled()).then(() => putMock.mock.lastCall![0]);
  const group = (name: string) => within(screen.getByRole("radiogroup", { name }));
  const checked = (testId: string) => within(screen.getByTestId(testId)).getAllByRole("checkbox").filter((c) => c.getAttribute("aria-checked") === "true");

  it.each([
    ["walls-github", YOUR_WALLS.GITHUB_CAPS, GITHUB_CAPABILITIES.length, "github_capabilities", "Clone, fetch and browse"],
    ["walls-ado", YOUR_WALLS.ADO_CAPS, ADO_CAPABILITIES.length, "azure_devops_capabilities", "Read code"],
  ])("%s: No limit, then Only these starting with nothing checked and Save off until one is", async (testId, title, total, key, first) => {
    render(<YourWallsCard />);
    await open();
    const list = within(screen.getByTestId(testId));
    expect(group(title).getByRole("radio", { name: YOUR_WALLS.LIMIT_NONE })).toBeChecked();
    expect(list.queryAllByRole("checkbox")).toHaveLength(0);
    await user.click(group(title).getByRole("radio", { name: YOUR_WALLS.CAPS_ONLY }));
    expect(list.getAllByRole("checkbox")).toHaveLength(total);
    expect(checked(testId)).toHaveLength(0);
    expect(list.getByText(YOUR_WALLS.CAPS_COUNT(0, total))).toBeInTheDocument();
    expect(list.getByText(YOUR_WALLS.CAPS_EMPTY)).toBeInTheDocument();
    expect(save()).toBeDisabled();
    await user.click(list.getByRole("checkbox", { name: new RegExp(`^${first}`) }));
    expect(list.getByText(YOUR_WALLS.CAPS_COUNT(1, total))).toBeInTheDocument();
    expect(list.queryByText(YOUR_WALLS.CAPS_EMPTY)).not.toBeInTheDocument();
    await user.click(save());
    expect(Object.keys((await sent()) as object)).toEqual([key]);
    expect(putMock.mock.lastCall![0]).toEqual({ [key]: [expect.any(String)] });
  });

  it("Reads only checks the six GitHub and eleven Azure DevOps reads and clears the rest", async () => {
    getMock.mockResolvedValue({ spec: { github_capabilities: ["code_write", "org_admin"], azure_devops_capabilities: ["work_write"] } });
    render(<YourWallsCard />);
    await open();
    await user.click(within(screen.getByTestId("walls-github")).getByRole("button", { name: YOUR_WALLS.CAPS_READS_ONLY }));
    await user.click(within(screen.getByTestId("walls-ado")).getByRole("button", { name: YOUR_WALLS.CAPS_READS_ONLY }));
    expect(checked("walls-github")).toHaveLength(6);
    expect(checked("walls-ado")).toHaveLength(11);
    await user.click(save());
    const spec = (await sent()) as { github_capabilities: string[]; azure_devops_capabilities: string[] };
    expect(spec.github_capabilities).toEqual(["code_read", "issues_read", "actions_read", "packages_read", "org_read", "identity"]);
    expect(spec.azure_devops_capabilities).toHaveLength(11);
  });

  it("says what leaving out the push capability does, for the broker, and drops the line when it is checked", async () => {
    getMock.mockResolvedValue({ spec: { github_capabilities: ["code_read"] } });
    render(<YourWallsCard />);
    await open();
    const list = within(screen.getByTestId("walls-github"));
    expect(list.getByText(YOUR_WALLS.CAPS_NO_PUSH(GITHUB_PUSH_NAME, "GitHub"))).toBeInTheDocument();
    expect(list.getByText(YOUR_WALLS.CAPS_NO_PUSH_BROKER_OFF)).toBeInTheDocument();
    await user.click(list.getByRole("checkbox", { name: /^Push commits with git/ }));
    expect(list.queryByText(YOUR_WALLS.CAPS_NO_PUSH_BROKER_OFF)).not.toBeInTheDocument();
  });

  it("tool rules: no default row, a new rule starts at hold, hold, approve_each and deny only, * is a rule like any other", async () => {
    render(<YourWallsCard />);
    await open();
    const rules = within(screen.getByTestId("walls-tool-rules"));
    expect(rules.getByText(YOUR_WALLS.TOOLS_NONE)).toBeInTheDocument();
    expect(rules.getByText(YOUR_WALLS.TOOLS_STAR_HINT)).toBeInTheDocument();
    await user.click(rules.getByRole("button", { name: YOUR_WALLS.TOOLS_ADD }));
    const effect = rules.getByRole("combobox", { name: YOUR_WALLS.TOOLS_EFFECT_N(1) }) as HTMLSelectElement;
    expect(effect.value).toBe("hold");
    expect([...effect.options].map((o) => o.value)).toEqual(["hold", "approve_each", "deny"]);
    expect(rules.queryByText("*")).not.toBeInTheDocument();
    setField(rules.getByRole("textbox", { name: YOUR_WALLS.TOOLS_TOOL_N(1) }), "*");
    await user.selectOptions(effect, "deny");
    await user.click(rules.getByRole("button", { name: YOUR_WALLS.TOOLS_ADD }));
    setField(rules.getByRole("textbox", { name: YOUR_WALLS.TOOLS_TOOL_N(2) }), "WebFetch");
    await user.click(save());
    expect(await sent()).toEqual({ tool_rules: [{ tool: "*", effect: "deny" }, { tool: "WebFetch", effect: "hold" }] });
  });

  it("tool rules: an approve_each rule loads as approve_each and is saved as approve_each, not turned into hold", async () => {
    getMock.mockResolvedValue({ spec: { tool_rules: [{ tool: "mcp__jira__delete_issue", effect: "approve_each" }] } });
    render(<YourWallsCard />);
    await open();
    const rules = within(screen.getByTestId("walls-tool-rules"));
    expect((rules.getByRole("combobox", { name: YOUR_WALLS.TOOLS_EFFECT_N(1) }) as HTMLSelectElement).value).toBe("approve_each");
    await user.click(rules.getByRole("button", { name: YOUR_WALLS.TOOLS_ADD }));
    setField(rules.getByRole("textbox", { name: YOUR_WALLS.TOOLS_TOOL_N(2) }), "WebFetch");
    await user.click(save());
    expect(await sent()).toEqual({ tool_rules: [{ tool: "mcp__jira__delete_issue", effect: "approve_each" }, { tool: "WebFetch", effect: "hold" }] });
  });

  it.each([
    ["Bash", true],
    ["*", true],
    ["mcp__jira", true],
    ["mcp__jira__delete_issue", false],
  ])("tool rules: for %s approve_each greyed is %s, with the reason as text", async (tool, greyed) => {
    getMock.mockResolvedValue({ spec: { tool_rules: [{ tool, effect: "hold" }] } });
    render(<YourWallsCard />);
    await open();
    const rules = within(screen.getByTestId("walls-tool-rules"));
    const select = rules.getByRole("combobox", { name: YOUR_WALLS.TOOLS_EFFECT_N(1) }) as HTMLSelectElement;
    expect([...select.options].find((o) => o.value === "approve_each")?.disabled).toBe(greyed);
    if (greyed) {
      expect(document.getElementById(select.getAttribute("aria-describedby")!)).toHaveTextContent("Approve each call names one MCP tool. Add it as a rule for that tool.");
    }
  });

  it("tool rules: a loaded approve_each on a name that cannot take it stays selected, and the form says why", async () => {
    getMock.mockResolvedValue({ spec: { tool_rules: [{ tool: "mcp__jira", effect: "approve_each" }] } });
    render(<YourWallsCard />);
    await open();
    const rules = within(screen.getByTestId("walls-tool-rules"));
    expect((rules.getByRole("combobox", { name: YOUR_WALLS.TOOLS_EFFECT_N(1) }) as HTMLSelectElement).value).toBe("approve_each");
    expect(rules.getByText(/cannot be approve_each/)).toBeInTheDocument();
  });

  it("tool rules: two rules for one tool are marked, say so, and keep Save off", async () => {
    getMock.mockResolvedValue({ spec: { tool_rules: [{ tool: "WebFetch", effect: "hold" }, { tool: "WebFetch", effect: "deny" }] } });
    render(<YourWallsCard />);
    await open();
    const rules = within(screen.getByTestId("walls-tool-rules"));
    expect(rules.getByRole("textbox", { name: YOUR_WALLS.TOOLS_TOOL_N(1) })).toHaveAttribute("aria-invalid", "true");
    expect(rules.getByRole("textbox", { name: YOUR_WALLS.TOOLS_TOOL_N(2) })).toHaveAttribute("aria-invalid", "true");
    expect(rules.getByRole("alert")).toHaveTextContent("WebFetch");
    expect(save()).toBeDisabled();
    await user.click(rules.getByRole("button", { name: YOUR_WALLS.TOOLS_REMOVE_N(2) }));
    expect(save()).toBeEnabled();
  });

  it("a rule or path added and left blank is dropped on Save, and Save stays on", async () => {
    render(<YourWallsCard />);
    await open();
    await user.click(screen.getByRole("button", { name: YOUR_WALLS.TOOLS_ADD }));
    await user.click(screen.getByRole("button", { name: YOUR_WALLS.PATHS_ADD }));
    await user.click(screen.getByRole("button", { name: YOUR_WALLS.PATHS_ADD }));
    expect(save()).toBeEnabled();
    await user.click(save());
    expect(await sent()).toEqual({});
  });

  it("an existing tool rule or path whose text was cleared blocks Save until it is typed again or removed", async () => {
    getMock.mockResolvedValue({ spec: { tool_rules: [{ tool: "WebFetch", effect: "hold" }, { tool: "Bash", effect: "deny" }], push_rules: { deny_paths: [".github/workflows/", "infra/prod/**"] } } });
    render(<YourWallsCard />);
    await open();
    setField(screen.getByRole("textbox", { name: YOUR_WALLS.TOOLS_TOOL_N(2) }), "");
    setField(screen.getByRole("textbox", { name: YOUR_WALLS.PATHS_PATH_N(2) }), "");
    expect(screen.getByText(YOUR_WALLS.TOOLS_BLANK)).toBeInTheDocument();
    expect(screen.getByText(YOUR_WALLS.PATHS_BLANK)).toBeInTheDocument();
    expect(save()).toBeDisabled();
    setField(screen.getByRole("textbox", { name: YOUR_WALLS.TOOLS_TOOL_N(2) }), "Bash");
    expect(save()).toBeDisabled();
    await user.click(screen.getByRole("button", { name: YOUR_WALLS.PATHS_REMOVE_N(2) }));
    expect(save()).toBeEnabled();
    await user.click(save());
    expect(await sent()).toEqual({ tool_rules: [{ tool: "WebFetch", effect: "hold" }, { tool: "Bash", effect: "deny" }], push_rules: { deny_paths: [".github/workflows/"] } });
  });

  it("a row that was typed in and then cleared is not dropped either", async () => {
    render(<YourWallsCard />);
    await open();
    await user.click(screen.getByRole("button", { name: YOUR_WALLS.TOOLS_ADD }));
    setField(screen.getByRole("textbox", { name: YOUR_WALLS.TOOLS_TOOL_N(1) }), "Bash");
    setField(screen.getByRole("textbox", { name: YOUR_WALLS.TOOLS_TOOL_N(1) }), "");
    expect(save()).toBeDisabled();
  });

  it("paths never pushed stay editable beside Refuse every push, say why they change nothing, and keep their notes readable", async () => {
    getMock.mockResolvedValue({ spec: { push_rules: { deny_paths: [".github/workflows/"] } } });
    render(<YourWallsCard />);
    await open();
    const paths = within(screen.getByTestId("walls-deny-paths"));
    expect(paths.getByText(YOUR_WALLS.PATHS_ADO_NOTE)).toBeInTheDocument();
    expect(paths.getByText(YOUR_WALLS.PATHS_BROKER_NOTE)).toBeInTheDocument();
    expect(paths.queryByText(YOUR_WALLS.PATHS_OFF_NOTE)).not.toBeInTheDocument();
    await user.click(screen.getByRole("switch", { name: YOUR_WALLS.REFUSE_PUSH }));
    expect(paths.getByText(YOUR_WALLS.PATHS_OFF_NOTE)).toBeInTheDocument();
    const dimmed = screen.getByTestId("walls-deny-paths").querySelector("[data-dimmed]")!;
    expect(dimmed).toContainElement(paths.getByRole("textbox", { name: YOUR_WALLS.PATHS_PATH_N(1) }));
    for (const note of [YOUR_WALLS.PATHS_ADO_NOTE, YOUR_WALLS.PATHS_BROKER_NOTE, YOUR_WALLS.PATHS_OFF_NOTE]) expect(dimmed).not.toContainElement(paths.getByText(note));
    expect(paths.getByRole("textbox", { name: YOUR_WALLS.PATHS_PATH_N(1) })).toBeEnabled();
    await user.click(save());
    expect(await sent()).toEqual({ push_rules: { deny_paths: [".github/workflows/"], deny_all: true } });
  });

  it("Record at least: No minimum, Terminal or Screen and actions, never Off", async () => {
    render(<YourWallsCard />);
    await open();
    expect(screen.getByText(YOUR_WALLS.RECORDING_UNSET)).toBeInTheDocument();
    expect(group(YOUR_WALLS.RECORDING).getAllByRole("radio").map((r) => r.parentElement?.textContent)).toEqual([YOUR_WALLS.RECORDING_NONE, "Terminal", "Screen and actions"]);
    await user.click(group(YOUR_WALLS.RECORDING).getByRole("radio", { name: "Screen and actions" }));
    expect(screen.getByText(/Records the screen and an action log/)).toBeInTheDocument();
    await user.click(save());
    expect(await sent()).toEqual({ recording: "screen_actions" });
  });

  it.each([
    [15, "15", "seconds"],
    [90, "90", "seconds"],
    [900, "15", "minutes"],
    [3600, "60", "minutes"],
  ])("Stop when idle shows a stored %i seconds exactly (%s %s) and an unrelated save sends it unchanged", async (sec, n, unit) => {
    getMock.mockResolvedValue({ spec: { auto_stop_after_sec: sec } });
    render(<YourWallsCard />);
    await open();
    expect(screen.getByRole("textbox", { name: YOUR_WALLS.IDLE_NUMBER })).toHaveValue(n);
    expect(screen.getByRole("combobox", { name: YOUR_WALLS.IDLE_UNIT_LABEL })).toHaveValue(unit);
    await user.click(screen.getByRole("checkbox", { name: YOUR_WALLS.COPY_OUT }));
    await user.click(save());
    expect(await sent()).toEqual({ auto_stop_after_sec: sec, desktop: { copy_out: "blocked" } });
  });

  it("Stop when idle: Stop after N seconds or minutes; 0, a fraction and nothing keep Save off", async () => {
    render(<YourWallsCard />);
    await open();
    expect(screen.getByRole("textbox", { name: YOUR_WALLS.IDLE_NUMBER })).toBeDisabled();
    await user.click(group(YOUR_WALLS.IDLE).getByRole("radio", { name: YOUR_WALLS.IDLE_AFTER }));
    const number = screen.getByRole("textbox", { name: YOUR_WALLS.IDLE_NUMBER });
    for (const bad of ["0", "1.5", "", "-3", "ten"]) {
      setField(number, bad);
      expect(screen.getByText(YOUR_WALLS.IDLE_PROBLEM)).toBeInTheDocument();
      expect(save()).toBeDisabled();
    }
    setField(number, "45");
    await user.selectOptions(screen.getByRole("combobox", { name: YOUR_WALLS.IDLE_UNIT_LABEL }), "seconds");
    expect(screen.queryByText(YOUR_WALLS.IDLE_PROBLEM)).not.toBeInTheDocument();
    await user.click(save());
    expect(await sent()).toEqual({ auto_stop_after_sec: 45 });
    await user.selectOptions(screen.getByRole("combobox", { name: YOUR_WALLS.IDLE_UNIT_LABEL }), "minutes");
    await user.click(save());
    await waitFor(() => expect(putMock).toHaveBeenLastCalledWith({ auto_stop_after_sec: 2700 }));
  });

  it("puts a server sentence for each new limit under its section", async () => {
    getMock.mockResolvedValue({ spec: { recording: "terminal" } });
    putMock.mockRejectedValue(new HttpError(400, "invalid walls: tool_rules[1]: duplicate rule for tool \"WebFetch\" — two rules for one tool means one of them does nothing", "walls_invalid"));
    render(<YourWallsCard />);
    await open();
    await user.click(save());
    const alert = await screen.findByRole("alert");
    expect(alert.parentElement).toBe(screen.getByTestId("walls-tool-rules"));
    expect(alert).toHaveTextContent("tool_rules[1]: duplicate rule");
  });

  it("says which limits a restarted run takes on", async () => {
    render(<YourWallsCard />);
    await open();
    expect(screen.getByText(YOUR_WALLS.RESTART_NOTE)).toBeInTheDocument();
  });

  it("two forms on the page keep their own capability checkboxes: a label click changes only its own form", async () => {
    const drafts: Form[] = [];
    function Draft({ at }: { at: number }) {
      const [form, setForm] = React.useState(() => toForm({ github_capabilities: ["code_read"] }));
      drafts[at] = form;
      return <div data-testid={`form-${at}`}><WallsForm form={form} onEdit={(p) => setForm((f) => ({ ...f, ...p }))} /></div>;
    }
    render(<><Draft at={0} /><Draft at={1} /></>);
    const label = "Open, update and review pull requests";
    const box = (at: number) => within(screen.getByTestId(`form-${at}`)).getByRole("checkbox", { name: label });
    const ids = [...document.querySelectorAll("[id]")].map((e) => e.id);
    expect(new Set(ids).size).toBe(ids.length);
    await user.click(within(screen.getByTestId("form-1")).getByText(label));
    expect(box(1)).toBeChecked();
    expect(box(0)).not.toBeChecked();
    expect(drafts[1].github).toContain("pr");
    expect(drafts[0].github).toEqual(["code_read"]);
  });
  it("an unrelated save keeps a stored capability list's order; a changed selection takes the catalogue's order", () => {
    const stored = { github_capabilities: ["pr", "code_read", "issues_read"], azure_devops_capabilities: ["pr", "code_read"] };
    expect(GITHUB_CAPABILITIES.indexOf("code_read")).toBeLessThan(GITHUB_CAPABILITIES.indexOf("pr"));
    expect(JSON.stringify(toSpec(toForm(stored)))).toBe(JSON.stringify(stored));
    const edited = toSpec({ ...toForm(stored), github: ["pr", "code_read"], ado: ["pr"] });
    expect(edited.github_capabilities).toEqual(["code_read", "pr"]);
    expect(edited.azure_devops_capabilities).toEqual(["pr"]);
    // Picking the same boxes again, in another order, is no change either.
    expect(toSpec({ ...toForm(stored), github: ["issues_read", "pr", "code_read"] }).github_capabilities).toEqual(stored.github_capabilities);
  });

  it("each section error is tied to its input by aria-describedby, and two forms on one page share no id", () => {
    const refused = { sites: "sites bad", never: "never bad", unlisted: "unlisted bad", barrier: "barrier bad", clipboard: "clipboard bad", credentials: "credentials bad", github: "github bad", ado: "ado bad", tools: "tools bad", pushes: "pushes bad", recording: "recording bad", idle: "idle bad" };
    const bad = { ...toForm({ allowed_domains: [], tool_rules: [{ tool: "a", effect: "deny" }, { tool: "a", effect: "deny" }], push_rules: { deny_paths: [""] }, github_capabilities: [], auto_stop_after_sec: 5 }), idleOn: true, idleText: "x" };
    render(
      <>
        <div data-testid="form-0"><WallsForm form={bad} onEdit={() => {}} errors={refused} /></div>
        <div data-testid="form-1"><WallsForm form={bad} onEdit={() => {}} errors={refused} /></div>
      </>,
    );
    const ids = [...document.querySelectorAll("[id]")].map((e) => e.id);
    expect(new Set(ids).size).toBe(ids.length);
    const described = (form: HTMLElement, name: string) => {
      const el = within(form).getByLabelText(name, { selector: "[aria-describedby]" });
      return (el.getAttribute("aria-describedby") ?? "").split(" ").map((id) => document.getElementById(id)?.textContent);
    };
    for (const at of [0, 1]) {
      const form = screen.getByTestId(`form-${at}`);
      const alerts = within(form).getAllByRole("alert");
      // Every error showing is named by exactly one element of its own form.
      for (const a of alerts) {
        const users = [...form.querySelectorAll("[aria-describedby]")].filter((e) => e.getAttribute("aria-describedby")!.split(" ").includes(a.id));
        expect(users.length, a.textContent ?? "").toBeGreaterThan(0);
      }
      expect(described(form, YOUR_WALLS.SITES)).toEqual(["sites bad"]);
      expect(described(form, YOUR_WALLS.NEVER)).toEqual(["never bad"]);
      expect(described(form, YOUR_WALLS.BARRIER)).toEqual(["barrier bad"]);
      expect(described(form, YOUR_WALLS.RECORDING)).toEqual(["recording bad"]);
      expect(described(form, YOUR_WALLS.IDLE_NUMBER)).toEqual([YOUR_WALLS.IDLE_PROBLEM]);
      expect(described(form, YOUR_WALLS.TOOLS)).toEqual(["tools bad", expect.stringContaining("Two rules")]);
      expect(described(form, YOUR_WALLS.TOOLS_TOOL_N(1))).toEqual([expect.stringContaining("Two rules")]);
      expect(described(form, YOUR_WALLS.PATHS_PATH_N(1))).toEqual([YOUR_WALLS.PATHS_BLANK]);
      expect(described(form, YOUR_WALLS.GITHUB_CAPS)).toEqual(["github bad", YOUR_WALLS.CAPS_EMPTY]);
      expect(within(form).getByRole("radiogroup", { name: YOUR_WALLS.SITES })).toHaveAttribute("aria-invalid", "true");
      const only = within(form).getByRole("textbox", { name: YOUR_WALLS.SITES_ONLY });
      expect(only).toHaveAttribute("aria-invalid", "true");
      expect(only.getAttribute("aria-describedby")?.split(" ").map((id) => form.querySelector(`[id="${id}"]`)?.textContent)).toEqual(["sites bad"]);
    }
  });
});
