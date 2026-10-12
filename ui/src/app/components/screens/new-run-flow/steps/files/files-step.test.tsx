/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { NR_FILES } from "../../../../wardyn/copy/new-run/files";
import { NR_SHARED } from "../../../../wardyn/copy/new-run/shared";
import { filesPreflight, limitsFor, MAIN_DRIVE_FACTS, REPO_ATTACHMENT, SCENARIO_FILES_WORKSPACES } from "../../../../../lib/new-run-fixtures/scenarios/files";
import type { RepoBranches } from "../../../../../lib/api/repo-branches";
import type { RunIntent } from "../../draft/run-intent";
import { NewRunDraftProvider, useNewRunDispatch, useNewRunDraft } from "../../new-run-route";
import { FilesStep } from "./files-step";

const wfetchMock = vi.fn();
const go = vi.fn();
vi.mock("../../../../../lib/api/core", async (orig) => ({ ...(await orig<typeof import("../../../../../lib/api/core")>()), wfetch: (...a: unknown[]) => wfetchMock(...a) }));
// The shared checks hook is faked: it owns debounce and identity fencing, which are its own tests'.
const checks = vi.hoisted(() => ({ bodies: [] as (Record<string, unknown> | null)[], result: null as unknown, principal: "alice" }));
vi.mock("../../draft/use-intent-checks", () => ({
  useDraftIdentity: () => ({ principal: checks.principal, resolved: true, revision: 0, authGeneration: 0 }),
  useIntentChecks: (params: { body: Record<string, unknown> | null }) => {
    checks.bodies.push(params.body);
    return { preflightResult: params.body === null ? null : checks.result };
  },
}));

const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status, headers: { "content-type": "application/json" } });
const LISTED: RepoBranches = { listing: "listed", branches: ["main", "dev"], default: "main", truncated: false };
const INTERACTIVE: RunIntent = { shape: "environment", experience: "interactive" };
const BACKGROUND: RunIntent = { shape: "application", experience: "background", workload: { kind: "command", command: "make" } };

let branches: RepoBranches | Response;
beforeEach(() => {
  checks.result = filesPreflight(MAIN_DRIVE_FACTS.filesystem);
  branches = LISTED;
  checks.bodies.length = 0;
  checks.principal = "alice";
  go.mockReset();
  wfetchMock.mockImplementation(async (path: string) => {
    if (path.includes("/branches")) return branches instanceof Response ? branches : json(branches);
    if (path.startsWith("/workspaces")) return json(SCENARIO_FILES_WORKSPACES.member);
    throw new Error(`unexpected ${path}`);
  });
});
afterEach(() => {
  cleanup();
  wfetchMock.mockReset();
});

function Seed({ intent }: { intent: RunIntent }) {
  const dispatch = useNewRunDispatch();
  React.useEffect(() => dispatch({ type: "seed", origin: { kind: "scratch" }, intent }), [dispatch, intent]);
  return null;
}
const Drawn = () => <p data-testid="intent">{JSON.stringify(useNewRunDraft().intent)}</p>;
const drawn = (): RunIntent => JSON.parse(screen.getByTestId("intent").textContent!);

function show(intent: RunIntent = INTERACTIVE) {
  render(
    <MemoryRouter>
      <NewRunDraftProvider owner="alice">
        <Seed intent={intent} />
        <FilesStep location={{ screen: "step", step: "files" }} go={go} startFromScratch={() => {}} />
        <Drawn />
      </NewRunDraftProvider>
    </MemoryRouter>,
  );
}
const size = () => screen.getByLabelText(NR_FILES.SIZE_LABEL) as HTMLInputElement;
const includeBox = (name: string) => screen.findByRole("checkbox", { name: NR_FILES.INCLUDE_ARIA(name) });

describe("main drive", () => {
  it("shows the section, a Required chip and the pool's default, with the cap and its enforcement word", async () => {
    show();
    expect(screen.getByRole("heading", { level: 2, name: NR_FILES.MAIN_TITLE })).toBeInTheDocument();
    expect(screen.getByText(NR_FILES.MAIN_BODY)).toBeInTheDocument();
    expect(screen.getByText(NR_SHARED.REQUIRED)).toBeInTheDocument();
    expect(await screen.findByText(NR_FILES.UP_TO(8))).toBeInTheDocument();
    expect(size()).toHaveValue(4);
    expect(size()).toHaveAttribute("step", "0.1");
    expect(screen.getByText(NR_FILES.ENFORCED)).toBeInTheDocument();
    expect(screen.getByText(NR_FILES.REUSE_LABEL)).toBeInTheDocument();
    expect(screen.getByLabelText(NR_FILES.REUSE_LABEL)).toBeDisabled();
    expect(screen.getAllByText(NR_SHARED.COMING_SOON).length).toBeGreaterThanOrEqual(2);
  });

  it("clamps a size over the cap to the cap on blur and names the pool", async () => {
    show();
    await screen.findByText(NR_FILES.UP_TO(8));
    await userEvent.clear(size());
    await userEvent.type(size(), "16");
    await userEvent.tab();
    expect(size()).toHaveValue(8);
    expect(screen.getByText(NR_FILES.CLAMP_POOL(8, "Shared pool"))).toBeInTheDocument();
    expect(drawn().resources).toEqual({ disk_mib: 8192 });
  });

  it("names the server or the organisation when they, not the pool, set the cap", async () => {
    checks.result = { ...filesPreflight(MAIN_DRIVE_FACTS.filesystem), runner_pools: [{ ...filesPreflight(MAIN_DRIVE_FACTS.filesystem).runner_pools![0], limits: [limitsFor("interactive", 4096, "deployment")] }] };
    show();
    await screen.findByText(NR_FILES.UP_TO(4));
    await userEvent.clear(size());
    await userEvent.type(size(), "9");
    await userEvent.tab();
    expect(screen.getByText(NR_FILES.CLAMP_SERVER(4))).toBeInTheDocument();
  });

  it("keeps a size under the cap in whole MiB and sends it as resources.disk_mib", async () => {
    show();
    await screen.findByText(NR_FILES.UP_TO(8));
    await userEvent.clear(size());
    await userEvent.type(size(), "2.5");
    await userEvent.tab();
    expect(drawn().resources).toEqual({ disk_mib: 2560 });
    expect(screen.queryByText(/^Clamped/)).toBeNull();
  });

  it("reads a cap under 1 GiB with one decimal (A8)", async () => {
    checks.result = filesPreflight(MAIN_DRIVE_FACTS.filesystem, 512);
    show();
    expect(await screen.findByText(NR_FILES.UP_TO("0.5"))).toBeInTheDocument();
    expect(size()).toHaveValue(0.5);
  });

  it("omits the cap line when nothing caps the size, and asks for a size when nothing defaults it", async () => {
    checks.result = { ...filesPreflight(MAIN_DRIVE_FACTS.filesystem), runner_pools: [{ ...filesPreflight(MAIN_DRIVE_FACTS.filesystem).runner_pools![0], limits: [limitsFor("interactive")] }] };
    show();
    await screen.findByText(NR_FILES.ENFORCED);
    expect(screen.queryByText(/^Up to/)).toBeNull();
    expect(size()).toHaveValue(null);
    await userEvent.click(screen.getByRole("button", { name: NR_SHARED.CONTINUE_TO(NR_SHARED.STEP_TOOLS) }));
    expect(size()).toHaveFocus();
    expect(go).not.toHaveBeenCalled();
    await userEvent.tab();
    expect(await screen.findByText(NR_FILES.SIZE_ERROR)).toBeInTheDocument();
  });
});

describe("enforcement chip (W1)", () => {
  it.each([
    ["eviction", NR_FILES.ENFORCED_BY_STOP],
    ["none", NR_FILES.NOT_ENFORCED],
    ["runner", `${NR_FILES.ENFORCED} ${NR_FILES.REPORTED_BY_RUNNER}`],
  ] as const)("%s says its own words", async (word, text) => {
    checks.result = filesPreflight(MAIN_DRIVE_FACTS[word]);
    show();
    expect(await screen.findByText(text)).toBeInTheDocument();
  });

  it("an unknown word shows no chip", async () => {
    checks.result = filesPreflight(MAIN_DRIVE_FACTS.unknown);
    show();
    await screen.findByText(NR_FILES.UP_TO(8));
    expect(screen.queryByText(NR_FILES.ENFORCED)).toBeNull();
    expect(screen.queryByText(NR_FILES.NOT_ENFORCED)).toBeNull();
  });

  it("its tooltip opens on press and carries the server's sentence", async () => {
    checks.result = filesPreflight(MAIN_DRIVE_FACTS.none);
    show();
    await userEvent.click(await screen.findByRole("button", { name: NR_FILES.NOT_ENFORCED_ARIA }));
    expect(await screen.findByText(NR_FILES.NOT_ENFORCED_TIP)).toBeInTheDocument();
  });
});

describe("what happens to it (W2)", () => {
  it("an Interactive run on a pool that keeps stopped sandboxes says how long, with its maximum", async () => {
    show();
    expect(await screen.findByText(NR_FILES.LIFE_KEPT("8 hours", "24 hours"))).toBeInTheDocument();
  });

  it("deletes it when the pool keeps nothing, and for a Background run", async () => {
    checks.result = filesPreflight(MAIN_DRIVE_FACTS.eviction);
    show();
    expect(await screen.findByText(NR_FILES.LIFE_DELETED)).toBeInTheDocument();
    cleanup();
    checks.result = filesPreflight(MAIN_DRIVE_FACTS.filesystem, 8192, "background");
    show(BACKGROUND);
    expect(await screen.findByText(NR_FILES.LIFE_DELETED)).toBeInTheDocument();
  });

  it("a run with no end is kept until you end it; before the facts arrive nothing is claimed", async () => {
    show({ ...INTERACTIVE, no_end: true });
    expect(await screen.findByText(NR_FILES.LIFE_NO_END)).toBeInTheDocument();
    cleanup();
    checks.result = null;
    show();
    expect(screen.queryByText(NR_FILES.LIFE_LABEL)).toBeNull();
  });
});

describe("repositories", () => {
  it("lists each repository with its fixed server path, none included, and a link to add one", async () => {
    show();
    expect(await includeBox("payments")).not.toBeChecked();
    expect(await includeBox("docs")).not.toBeChecked();
    expect(screen.getByText("/home/agent/work/payments")).toBeInTheDocument();
    expect(screen.getAllByText(NR_FILES.REPOSITORY_BADGE)).toHaveLength(2);
    expect(screen.getByRole("link", { name: NR_FILES.ADD_REPOSITORY })).toHaveAttribute("href", "/workspaces");
    expect(screen.getByLabelText(NR_FILES.BRANCH_ARIA("payments"))).toBeDisabled();
  });

  it("includes several, reads branches only for the included, and writes a picked branch", async () => {
    show();
    await userEvent.click(await includeBox("payments"));
    await userEvent.click(await includeBox("docs"));
    const picker = await screen.findByRole("combobox", { name: NR_FILES.BRANCH_ARIA("payments") });
    await waitFor(() => expect(picker).toBeEnabled());
    await userEvent.selectOptions(picker, "dev");
    expect(drawn().workspaces).toEqual([
      { workspace_id: "ws-payments", repos: [{ repo: "github.com/acme/payments", ref: "dev" }] },
      { workspace_id: "ws-docs", repos: [{ repo: "github.com/acme/docs" }] },
    ]);
    expect(wfetchMock.mock.calls.filter(([p]) => String(p).includes("/branches"))).toHaveLength(2);
  });

  it("falls back to a text field with its hint when the listing is unavailable, and says a typed ref is not checked", async () => {
    branches = { listing: "unavailable", branches: [], truncated: false, reason: "forge_not_supported" };
    show();
    await userEvent.click(await includeBox("payments"));
    const field = await screen.findByRole("textbox", { name: NR_FILES.BRANCH_ARIA("payments") });
    expect(field).toHaveAttribute("placeholder", NR_FILES.BRANCH_PLACEHOLDER);
    expect(screen.getByText(NR_FILES.BRANCH_FALLBACK, { exact: false })).toBeInTheDocument();
    expect(screen.queryByText(NR_FILES.REF_NOT_CHECKED)).toBeNull();
    await userEvent.type(field, "release/1.2");
    expect(screen.getByText(NR_FILES.REF_NOT_CHECKED)).toBeInTheDocument();
    expect(drawn().workspaces?.[0].repos).toEqual([{ repo: "github.com/acme/payments", ref: "release/1.2" }]);
  });

  it("a failed read is the same fallback", async () => {
    branches = json({ reason: "branch_list_rate_limited", error: "too many branch lists; slow down" }, 429);
    show();
    await userEvent.click(await includeBox("payments"));
    expect(await screen.findByRole("textbox", { name: NR_FILES.BRANCH_ARIA("payments") })).toBeInTheDocument();
  });

  it("a truncated list ends with Other…, which opens the same field", async () => {
    branches = { ...LISTED, truncated: true };
    show();
    await userEvent.click(await includeBox("payments"));
    const picker = await screen.findByRole("combobox", { name: NR_FILES.BRANCH_ARIA("payments") });
    await waitFor(() => expect(within(picker).getByRole("option", { name: NR_FILES.BRANCH_OTHER })).toBeInTheDocument());
    await userEvent.selectOptions(picker, NR_FILES.BRANCH_OTHER);
    const field = await screen.findByRole("textbox", { name: NR_FILES.BRANCH_ARIA("payments") });
    expect(screen.queryByText(NR_FILES.BRANCH_FALLBACK, { exact: false })).toBeNull();
    await userEvent.type(field, "v2");
    expect(screen.getByText(NR_FILES.REF_NOT_CHECKED)).toBeInTheDocument();
  });
});

describe("mount map and summary", () => {
  it("lists the main drive and the server's attachment paths, marking the start, from the run's own preflight", async () => {
    checks.result = { ...filesPreflight(MAIN_DRIVE_FACTS.filesystem), attachments: [{ ...REPO_ATTACHMENT, target: "/srv/checkout/payments", starts_here: true }] };
    show({ ...INTERACTIVE, workspaces: [{ workspace_id: "ws-payments", repos: [{ repo: "github.com/acme/payments" }] }], start_folder: { kind: "attachment", attachment: "ws-payments", repo: "github.com/acme/payments" } });
    const map = screen.getByRole("region", { name: NR_FILES.MOUNT_TITLE });
    expect(await within(map).findByText("└ /srv/checkout/payments")).toBeInTheDocument();
    expect(within(map).getByText(`${NR_FILES.MOUNT_ITEM("payments")} ${NR_FILES.MOUNT_STARTS}`)).toBeInTheDocument();
    expect(within(map).getByText(NR_FILES.MOUNT_MAIN(4))).toBeInTheDocument();
    expect(screen.getByText("/srv/checkout/payments", { selector: "code" })).toBeInTheDocument();
  });

  it("keeps the last server answer through an edit instead of guessing a map from the draft", async () => {
    checks.result = { ...filesPreflight(MAIN_DRIVE_FACTS.filesystem), attachments: [REPO_ATTACHMENT, { kind: "drive", drive_id: "d1", name: "Notes", target: "/home/agent/drives/notes", read_only: false, starts_here: false }] };
    show({ ...INTERACTIVE, workspaces: [{ workspace_id: "ws-payments" }], start_folder: { kind: "attachment", attachment: "ws-payments" } });
    const map = screen.getByRole("region", { name: NR_FILES.MOUNT_TITLE });
    expect(await within(map).findByText("└ /home/agent/drives/notes")).toBeInTheDocument();
    // the next answer is not in yet: the hook has nothing current for this body
    checks.result = null;
    await userEvent.click(await includeBox("docs"));
    expect(within(map).getByText("└ /home/agent/drives/notes")).toBeInTheDocument();
    expect(within(map).getByText("└ /home/agent/work/payments")).toBeInTheDocument();
  });

  it("drops a kept answer when the person it was for is gone", async () => {
    checks.result = { ...filesPreflight(MAIN_DRIVE_FACTS.filesystem), attachments: [REPO_ATTACHMENT] };
    show({ ...INTERACTIVE, workspaces: [{ workspace_id: "ws-payments" }], start_folder: { kind: "attachment", attachment: "ws-payments" } });
    const map = screen.getByRole("region", { name: NR_FILES.MOUNT_TITLE });
    expect(await within(map).findByText("└ /home/agent/work/payments")).toBeInTheDocument();
    checks.principal = "bob";
    checks.result = null;
    await userEvent.click(await includeBox("docs"));
    expect(within(map).queryByText("└ /home/agent/work/payments")).toBeNull();
  });

  it("the summary is a status that follows the choices", async () => {
    show();
    await userEvent.click(await includeBox("payments"));
    expect(screen.getByRole("status")).toBeInTheDocument();
    await userEvent.click(screen.getByRole("radio", { name: `payments ${NR_FILES.STARTS_HERE}` }));
    await waitFor(() => expect(screen.getByText(/^Main drive 4 GiB · starts in payments/)).toBeInTheDocument(), { timeout: 3000 });
  });
});

describe("choices and Continue", () => {
  it("shows Your computer as Coming soon, and 'No extra files' writes the choice and says what is lost", async () => {
    show(BACKGROUND);
    expect(screen.getByRole("heading", { level: 2, name: NR_FILES.COMPUTER_TITLE })).toBeInTheDocument();
    expect(screen.getByText(NR_FILES.COMPUTER_SOON)).toBeInTheDocument();
    await userEvent.click(await includeBox("payments"));
    await userEvent.click(screen.getByRole("radio", { name: NR_FILES.NO_EXTRA }));
    expect(drawn().no_repositories_or_drives).toBe(true);
    expect(drawn().workspaces).toBeUndefined();
    expect(await screen.findByText(NR_FILES.NO_EXTRA_DELETED)).toBeInTheDocument();
    await userEvent.click(await includeBox("payments"));
    expect(drawn().no_repositories_or_drives).toBeUndefined();
  });

  it("holds Continue on the 'No extra files' choice until one is made, then opens Tools and access; an untouched size is not sent", async () => {
    show();
    await screen.findByText(NR_FILES.UP_TO(8));
    await userEvent.click(screen.getByRole("button", { name: NR_SHARED.CONTINUE_TO(NR_SHARED.STEP_TOOLS) }));
    expect(document.getElementById("nr-files-choice")).toHaveFocus();
    expect(go).not.toHaveBeenCalled();
    await userEvent.click(screen.getByRole("radio", { name: NR_FILES.NO_EXTRA }));
    await userEvent.click(screen.getByRole("button", { name: NR_SHARED.CONTINUE_TO(NR_SHARED.STEP_TOOLS) }));
    expect(go).toHaveBeenCalledWith({ screen: "step", step: "tools" });
    expect(drawn().resources).toBeUndefined();
  });

  it("holds Continue on the first Starts here radio when something is included and nothing starts", async () => {
    show();
    await userEvent.click(await includeBox("docs"));
    await userEvent.click(screen.getByRole("button", { name: NR_SHARED.CONTINUE_TO(NR_SHARED.STEP_TOOLS) }));
    expect(document.getElementById("nr-starts-here")).toHaveFocus();
    expect(go).not.toHaveBeenCalled();
  });

  it("sends nothing while a step's local issues are open, and sends once they close", async () => {
    show({});
    await screen.findByRole("checkbox", { name: NR_FILES.INCLUDE_ARIA("payments") });
    expect(checks.bodies.every((b) => b === null)).toBe(true);
    cleanup();
    checks.bodies.length = 0;
    show();
    await userEvent.click(await includeBox("docs"));
    expect(checks.bodies.at(-1)).toBeNull();
    await userEvent.click(screen.getByRole("radio", { name: `docs ${NR_FILES.STARTS_HERE}` }));
    expect(checks.bodies.at(-1)).toMatchObject({ mode_contract: "0.9", start_folder: { attachment: "ws-docs" } });
  });

  it("asks for the pool's facts before anything is chosen, but never sends the prefilled size as a request", async () => {
    show();
    await screen.findByText(NR_FILES.UP_TO(8));
    expect(size()).toHaveValue(4);
    await userEvent.click(screen.getByRole("radio", { name: NR_FILES.NO_EXTRA }));
    await userEvent.click(screen.getByRole("button", { name: NR_SHARED.CONTINUE_TO(NR_SHARED.STEP_TOOLS) }));
    expect(go).toHaveBeenCalled();
    expect(drawn().resources).toBeUndefined();
    expect(checks.bodies.at(-1)).not.toHaveProperty("resources");
  });

  it("with no repository included, a held Continue focuses the Repositories section", async () => {
    show({ ...INTERACTIVE, drive: { enabled: true } });
    await screen.findByRole("checkbox", { name: NR_FILES.INCLUDE_ARIA("payments") });
    await userEvent.type(size(), "2");
    await userEvent.click(screen.getByRole("button", { name: NR_SHARED.CONTINUE_TO(NR_SHARED.STEP_TOOLS) }));
    expect(document.getElementById("nr-starts-here")).toHaveFocus();
  });
});
