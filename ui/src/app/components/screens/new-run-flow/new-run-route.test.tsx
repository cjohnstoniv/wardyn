/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import type * as React from "react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { MemoryRouter, Route, Routes, useLocation, useNavigationType, type InitialEntry } from "react-router-dom";
import { OperatorProvider } from "../../wardyn/operator-context";
import { NEW_RUN_SCREENS, NewRunRoute, useLocalIssues, useNewRunDraft, type NewRunScreenName, type NewRunScreenProps, type SeedLoaders } from "./new-run-route";

const T = "d1111111-1111-4111-8111-111111111111";
const R = "e1111111-1111-4111-8111-111111111111";

function Probe({ name }: { name: NewRunScreenName }) {
  const draft = useNewRunDraft();
  const issues = useLocalIssues();
  return (
    <>
      <p data-testid="screen">{name}</p>
      <p data-testid="origin">{draft.origin.kind}</p>
      <p data-testid="intent">{JSON.stringify(draft.intent)}</p>
      <p data-testid="issues">{issues.map((i) => i.code).join(" ")}</p>
    </>
  );
}
const probes = Object.fromEntries(
  (Object.keys(NEW_RUN_SCREENS) as NewRunScreenName[]).map((name) => [name, (_: NewRunScreenProps) => <Probe name={name} />]),
) as Record<NewRunScreenName, React.ComponentType<NewRunScreenProps>>;

function Where() {
  const location = useLocation();
  return (
    <>
      <p data-testid="path">{location.pathname + location.search}</p>
      <p data-testid="nav">{useNavigationType()}</p>
      <p data-testid="state">{JSON.stringify(location.state ?? null)}</p>
    </>
  );
}

const unavailable: SeedLoaders = { template: () => Promise.reject(new Error("no")), again: () => Promise.reject(new Error("no")) };
function show(entry: InitialEntry, loaders: SeedLoaders = unavailable) {
  render(
    <MemoryRouter initialEntries={[entry]}>
      <OperatorProvider operator={false} principal="alice">
        <Routes>
          <Route path="/runs/new" element={<><NewRunRoute loaders={loaders} screens={probes} /><Where /></>} />
          <Route path="/runs" element={<Where />} />
        </Routes>
      </OperatorProvider>
    </MemoryRouter>,
  );
}
const text = (id: string) => screen.getByTestId(id).textContent;

afterEach(cleanup);

describe("New Run route (U9)", () => {
  it("sends a scratch deep link past the first incomplete step back to it, replacing the URL", async () => {
    show("/runs/new?step=files");
    expect(await screen.findByText("shape")).toBeInTheDocument();
    expect(text("path")).toBe("/runs/new?step=shape");
    expect(text("nav")).toBe("REPLACE");
  });

  it("guards Review the same way for a scratch draft", async () => {
    show("/runs/new?step=review&view=json");
    expect(await screen.findByText("shape")).toBeInTheDocument();
    expect(text("path")).toBe("/runs/new?step=shape");
  });

  it("opens a template straight at Review, its missing choices left as blockers", async () => {
    const template = vi.fn<SeedLoaders["template"]>(() => Promise.resolve({
      origin: { kind: "template", id: T, revision: 4, name: "Nightly", templateKind: "template" },
      intent: { shape: "agent_task", experience: "background", image: "registry.example/app:1" },
    }));
    show(`/runs/new?template=${T}&rev=4`, { ...unavailable, template });
    expect(await screen.findByText("review")).toBeInTheDocument();
    expect(template).toHaveBeenCalledWith({ id: T, revision: 4 }, expect.any(AbortSignal));
    expect(text("origin")).toBe("template");
    expect(text("path")).toBe(`/runs/new?template=${T}&rev=4`);
    expect(text("issues")).toBe("harness_required task_required files_choice_required");
  });

  it("shows the failed load, not an empty draft, when the seed cannot be read", async () => {
    show(`/runs/new?again=${R}`);
    expect(await screen.findByText("seedFailed")).toBeInTheDocument();
  });

  it("reads the composer's prefill once, keeps its workspace, lands on Run and drops the router state", async () => {
    show({ pathname: "/runs/new", state: { prefill: { source: "composer", state: { task: "fix it", workspaces: [{ workspaceId: "w1" }] } } } });
    expect(await screen.findByText("run")).toBeInTheDocument();
    expect(text("origin")).toBe("composer");
    expect(JSON.parse(text("intent") ?? "")).toEqual({
      shape: "agent_task", workload: { kind: "agent_task", task: "fix it" }, workspaces: [{ workspace_id: "w1" }],
    });
    expect(text("path")).toBe("/runs/new?step=run");
    expect(text("nav")).toBe("REPLACE");
    expect(text("state")).toBe("null");
  });

  it("seeds a clone from the run page's prefill as origin clone, opens Review and drops the router state", async () => {
    const prefill = { inlinePolicy: false, state: { title: "Old", runType: "agent", mode: "batch", agent: "claude-code", task: "fix it", workspaces: [] } };
    show({ pathname: "/runs/new", state: { prefill } });
    expect(await screen.findByText("review")).toBeInTheDocument();
    expect(text("origin")).toBe("clone");
    expect(JSON.parse(text("intent") ?? "")).toEqual({
      shape: "agent_task", experience: "background", tools: [{ id: "claude-code", kind: "harness" }],
      workload: { kind: "agent_task", agent: "claude-code", task: "fix it" },
    });
    expect(text("path")).toBe("/runs/new?step=review");
    expect(text("state")).toBe("null");
  });

  it("never shows another person's launched run from router state: Back into their entry drops it", async () => {
    show({ pathname: "/runs/new", search: `?launched=${R}`, state: { created: { id: R }, owner: "mallory" } });
    expect(await screen.findByText("launched")).toBeInTheDocument();
    expect(text("state")).toBe("null");
    expect(text("nav")).toBe("REPLACE");
  });

  it("drops launched router state that names no owner", async () => {
    show({ pathname: "/runs/new", search: `?launched=${R}`, state: { created: { id: R } } });
    expect(await screen.findByText("launched")).toBeInTheDocument();
    expect(text("state")).toBe("null");
  });

  it("keeps the launched router state of the person signed in", async () => {
    const state = { created: { id: R }, owner: "alice" };
    show({ pathname: "/runs/new", search: `?launched=${R}`, state });
    expect(await screen.findByText("launched")).toBeInTheDocument();
    expect(JSON.parse(text("state") ?? "")).toEqual(state);
  });

  it("replaces a query outside the grammar with Start", async () => {
    show("/runs/new?step=nope");
    expect(await screen.findByText("start")).toBeInTheDocument();
    expect(text("path")).toBe("/runs/new");
    expect(text("nav")).toBe("REPLACE");
  });

  it("leaves on Escape when nothing is unsaved", async () => {
    show("/runs/new");
    await screen.findByText("start");
    fireEvent.keyDown(window, { key: "Escape" });
    expect(await screen.findByText("/runs")).toBeInTheDocument();
  });
});
