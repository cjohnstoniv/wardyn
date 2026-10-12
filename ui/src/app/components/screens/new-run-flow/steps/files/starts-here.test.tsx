/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import type { RunIntent } from "../../draft/run-intent";
import { NewRunDraftProvider, useNewRunDispatch, useNewRunDraft } from "../../new-run-route";
import type { RepoRow } from "./files-seams";
import { RepositoriesSection } from "./repositories-section";

const wfetchMock = vi.fn();
vi.mock("../../../../../lib/api/core", async (orig) => ({ ...(await orig<typeof import("../../../../../lib/api/core")>()), wfetch: (...a: unknown[]) => wfetchMock(...a) }));

const row = (id: string, repo: string): RepoRow => ({ key: `${id}/${repo}`, workspaceId: id, repo, name: repo, siblings: [repo], path: `/home/agent/work/${repo}` });
const ROWS = [row("w1", "api"), row("w2", "web"), row("w3", "docs")];
const AT_START: RunIntent = {
  shape: "environment", experience: "interactive",
  workspaces: [{ workspace_id: "w1", repos: [{ repo: "api" }] }, { workspace_id: "w2", repos: [{ repo: "web" }] }],
  start_folder: { kind: "attachment", attachment: "w1", repo: "api" },
};

function Seed({ intent }: { intent: RunIntent }) {
  const dispatch = useNewRunDispatch();
  React.useEffect(() => dispatch({ type: "seed", origin: { kind: "scratch" }, intent }), [dispatch, intent]);
  return null;
}
const Drawn = () => <p data-testid="intent">{JSON.stringify(useNewRunDraft().intent)}</p>;
const drawn = (): RunIntent => JSON.parse(screen.getByTestId("intent").textContent!);

function show(intent: RunIntent) {
  render(
    <MemoryRouter>
      <NewRunDraftProvider owner="alice">
        <Seed intent={intent} />
        <RepositoriesSection rows={ROWS} pathOf={(r) => r.path} />
        <Drawn />
      </NewRunDraftProvider>
    </MemoryRouter>,
  );
}
const startsAt = (name: string) => screen.getByRole("radio", { name: `${name} Starts here` });

beforeEach(() => wfetchMock.mockResolvedValue(new Response(JSON.stringify({ listing: "listed", branches: ["main"], default: "main", truncated: false }), { headers: { "content-type": "application/json" } })));
afterEach(() => {
  cleanup();
  wfetchMock.mockReset();
});

describe("Starts here (U6)", () => {
  it("is one radio group across every row, named for its item, checked where the draft starts", async () => {
    show(AT_START);
    expect(await screen.findByRole("checkbox", { name: "Include api" })).toBeChecked();
    expect(startsAt("api")).toBeChecked();
    expect(startsAt("web")).not.toBeChecked();
    expect(new Set(screen.getAllByRole("radio").map((r) => r.getAttribute("name")))).toEqual(new Set(["nr-starts-here"]));
  });

  it("arrowing through the group moves the start between included items and never attaches one", async () => {
    show(AT_START);
    await screen.findByRole("checkbox", { name: "Include api" });
    const attached = drawn().workspaces;
    startsAt("api").focus();
    await userEvent.keyboard("{ArrowDown}");
    expect(drawn().start_folder).toEqual({ kind: "attachment", attachment: "w2", repo: "web" });
    await userEvent.keyboard("{ArrowDown}");
    await userEvent.keyboard("{ArrowDown}");
    expect(drawn().workspaces).toEqual(attached);
    expect(attached?.map((w) => w.workspace_id)).toContain(drawn().start_folder?.attachment);
    expect(drawn().drive).toBeUndefined();
  });

  it("an item that is not included cannot be chosen, and choosing one included does not touch the others", async () => {
    show(AT_START);
    await screen.findByRole("checkbox", { name: "Include docs" });
    expect(startsAt("docs")).toBeDisabled();
    await userEvent.click(startsAt("docs"));
    expect(drawn().start_folder).toEqual(AT_START.start_folder);
    await userEvent.click(startsAt("web"));
    expect(drawn().workspaces).toEqual(AT_START.workspaces);
  });

  it("including an item does not make it the start; leaving the start's item out clears the start", async () => {
    show(AT_START);
    await userEvent.click(await screen.findByRole("checkbox", { name: "Include docs" }));
    expect(drawn().workspaces?.map((w) => w.workspace_id)).toEqual(["w1", "w2", "w3"]);
    expect(drawn().start_folder).toEqual(AT_START.start_folder);
    await userEvent.click(screen.getByRole("checkbox", { name: "Include api" }));
    expect(drawn().start_folder).toBeUndefined();
  });
});
