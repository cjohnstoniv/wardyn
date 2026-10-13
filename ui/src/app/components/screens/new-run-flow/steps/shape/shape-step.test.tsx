/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import type * as React from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, Route, Routes, useLocation } from "react-router-dom";
import { OperatorProvider } from "../../../../wardyn/operator-context";
import { NR_SHAPE } from "../../../../wardyn/copy/new-run/shape";
import { NR_SHARED } from "../../../../wardyn/copy/new-run/shared";
import { NR_TEMPLATES } from "../../../../wardyn/copy/new-run/templates";
import { SCENARIO_LAUNCH_INTENTS, SCENARIO_TEMPLATES } from "../../../../../lib/new-run-fixtures/scenarios/templates";
import type { TemplateList } from "../../../../../lib/types/templates";
import { NEW_RUN_SCREENS, NewRunRoute, useNewRunDraft, type NewRunScreenName, type NewRunScreenProps, type SeedLoaders } from "../../new-run-route";
import { ShapeStep } from "./shape-step";

const wfetchMock = vi.fn();
vi.mock("../../../../../lib/api/core", async (orig) => ({ ...(await orig<typeof import("../../../../../lib/api/core")>()), wfetch: (...a: unknown[]) => wfetchMock(...a) }));

const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status, headers: { "content-type": "application/json" } });
const NIGHTLY = SCENARIO_TEMPLATES.governed.templates[0];
const POLICY = { ...SCENARIO_TEMPLATES.governed.templates[1], group_label: undefined };
const RUN = SCENARIO_LAUNCH_INTENTS.member.intents[0];

function serve(routes: { templates?: TemplateList | Response; intents?: unknown }) {
  wfetchMock.mockImplementation(async (path: string) => {
    if (path === "/templates") return routes.templates instanceof Response ? routes.templates : json(routes.templates ?? { templates: [], publish: {} });
    if (path === "/me/launch-intents") return json(routes.intents ?? { intents: [] });
    throw new Error(`unexpected ${path}`);
  });
}

function Probe({ name }: { name: NewRunScreenName }) {
  const draft = useNewRunDraft();
  return <p data-testid="screen">{`${name} ${draft.origin.kind} ${JSON.stringify(draft.intent)}`}</p>;
}
const probes = Object.fromEntries(
  (Object.keys(NEW_RUN_SCREENS) as NewRunScreenName[]).map((name) => [name, (_: NewRunScreenProps) => <Probe name={name} />]),
) as Record<NewRunScreenName, React.ComponentType<NewRunScreenProps>>;

// The draft is read beside the real step; once Continue leaves Shape, the Run probe takes over.
const ShapeWithProbe = (props: NewRunScreenProps) => <><ShapeStep {...props} /><Probe name="shape" /></>;

function Where() {
  const location = useLocation();
  return <p data-testid="path">{location.pathname + location.search}</p>;
}
const noLoaders: SeedLoaders = { template: () => Promise.reject(new Error("no")), again: () => Promise.reject(new Error("no")) };
function show(entry = "/runs/new?step=shape", loaders: SeedLoaders = noLoaders) {
  render(
    <MemoryRouter initialEntries={[entry]}>
      <OperatorProvider operator={false} principal="alice">
        <Routes>
          <Route path="/runs/new" element={<><NewRunRoute loaders={loaders} screens={{ ...probes, shape: ShapeWithProbe }} /><Where /></>} />
        </Routes>
      </OperatorProvider>
    </MemoryRouter>,
  );
}
const path = () => screen.getByTestId("path").textContent;
const source = (name: string) => screen.getByRole("button", { name });
const workload = (name: string) => screen.findByRole("button", { name });

beforeEach(() => serve({}));
afterEach(() => {
  cleanup();
  wfetchMock.mockReset();
});

describe("Shape step: workload", () => {
  it("shows Start from as an optional three-way choice on From scratch, and the three workload buttons with their lines", async () => {
    show();
    expect(await screen.findByRole("heading", { level: 1, name: NR_SHAPE.TITLE })).toHaveFocus();
    expect(screen.getByText(NR_SHARED.EYEBROW(1))).toBeInTheDocument();
    const group = screen.getByRole("group", { name: NR_SHAPE.START_FROM });
    expect(within(group).getAllByRole("button").map((b) => [b.textContent, b.getAttribute("aria-pressed")])).toEqual([
      [NR_SHAPE.SOURCE_SCRATCH, "true"], [NR_SHAPE.SOURCE_TEMPLATE, "false"], [NR_SHAPE.SOURCE_AGAIN, "false"],
    ]);
    expect(screen.getByText(NR_SHARED.OPTIONAL)).toBeInTheDocument();
    for (const [title, body, sub] of [
      [NR_SHAPE.AGENT_TITLE, NR_SHAPE.AGENT_BODY, NR_SHAPE.AGENT_SUB], [NR_SHAPE.ENV_TITLE, NR_SHAPE.ENV_BODY, NR_SHAPE.ENV_SUB], [NR_SHAPE.APP_TITLE, NR_SHAPE.APP_BODY, NR_SHAPE.APP_SUB],
    ]) {
      expect(await workload(title)).toHaveAccessibleDescription(`${body} ${sub}`);
    }
  });

  it("a workload button writes the shape and the carriers that say it, and opens Run", async () => {
    show();
    await userEvent.click(await workload(NR_SHAPE.APP_TITLE));
    const drawn = await screen.findByText(/^run /);
    expect(JSON.parse(drawn.textContent!.slice("run scratch ".length))).toEqual({
      shape: "application", experience: "background", workload: { kind: "command", command: "" },
    });
    expect(path()).toBe("/runs/new?step=run");
  });

  it("an environment writes its own carriers", async () => {
    show();
    await userEvent.click(await workload(NR_SHAPE.ENV_TITLE));
    const drawn = await screen.findByText(/^run /);
    expect(JSON.parse(drawn.textContent!.slice("run scratch ".length))).toEqual({ shape: "environment", experience: "interactive" });
  });

  it("holds Continue on the workload group, marked invalid, until one is chosen", async () => {
    show();
    await userEvent.click(await screen.findByRole("button", { name: NR_SHARED.CONTINUE_TO(NR_SHARED.STEP_RUN) }));
    const group = document.getElementById("nr-workload");
    expect(group).toHaveFocus();
    expect(group).toHaveAttribute("aria-invalid", "true");
    expect(path()).toBe("/runs/new?step=shape");
  });

  it("Back returns to Start", async () => {
    show();
    await userEvent.click(await screen.findByRole("button", { name: NR_SHARED.BACK }));
    expect(path()).toBe("/runs/new");
  });
});

describe("Shape step: Start from", () => {
  it("hides the workloads, lists both kinds of template as plain options, and Continue opens Review on the one chosen", async () => {
    serve({ templates: { templates: [NIGHTLY, POLICY], publish: SCENARIO_TEMPLATES.governed.publish } });
    show();
    await userEvent.click(await screen.findByRole("button", { name: NR_SHAPE.SOURCE_TEMPLATE }));
    expect(screen.queryByRole("button", { name: NR_SHAPE.AGENT_TITLE })).toBeNull();
    expect(screen.queryByRole("button", { name: NR_SHARED.CONTINUE_TO(NR_SHARED.STEP_RUN) })).toBeNull();
    const combo = await screen.findByRole("combobox", { name: NR_SHAPE.TEMPLATE_LABEL });
    await waitFor(() => expect(combo).toHaveTextContent(NR_SHAPE.TEMPLATE_OPTION(NIGHTLY.name, NR_TEMPLATES.SCOPE_ME)));
    await userEvent.click(combo);
    expect(screen.getByRole("option", { name: NR_SHAPE.TEMPLATE_OPTION(NIGHTLY.name, NR_TEMPLATES.SCOPE_ME) })).toBeInTheDocument();
    const policy = screen.getByRole("option", { name: NR_SHAPE.TEMPLATE_OPTION(POLICY.name, NR_TEMPLATES.SCOPE_ORG) });
    expect(within(policy).queryByText(NR_TEMPLATES.POLICY_BADGE)).toBeNull();
    await userEvent.click(policy);
    expect(path()).toBe("/runs/new?step=shape");
    await userEvent.click(screen.getByRole("button", { name: NR_SHARED.CONTINUE }));
    await waitFor(() => expect(path()).toBe(`/runs/new?template=${POLICY.id}&rev=${POLICY.revision}&step=review`));
  });

  it("Continue opens Review on the first template when none was chosen", async () => {
    serve({ templates: { templates: [NIGHTLY, POLICY], publish: SCENARIO_TEMPLATES.governed.publish } });
    show();
    await userEvent.click(await screen.findByRole("button", { name: NR_SHAPE.SOURCE_TEMPLATE }));
    const go = screen.getByRole("button", { name: NR_SHARED.CONTINUE });
    await waitFor(() => expect(go).toBeEnabled());
    await userEvent.click(go);
    await waitFor(() => expect(path()).toBe(`/runs/new?template=${NIGHTLY.id}&rev=${NIGHTLY.revision}&step=review`));
  });

  it("labels a group's template with its display label, then the raw identifier", async () => {
    serve({ templates: { templates: [{ ...NIGHTLY, scope: "group", group_id: "g-1", group_label: "Platform" }, { ...POLICY, scope: "group", group_id: "g-2" }], publish: SCENARIO_TEMPLATES.governed.publish } });
    show();
    await userEvent.click(await screen.findByRole("button", { name: NR_SHAPE.SOURCE_TEMPLATE }));
    const combo = await screen.findByRole("combobox", { name: NR_SHAPE.TEMPLATE_LABEL });
    await waitFor(() => expect(combo).not.toBeDisabled());
    await userEvent.click(combo);
    expect(await screen.findByRole("option", { name: new RegExp(NR_TEMPLATES.GROUP_LABEL("Platform")) })).toBeInTheDocument();
    expect(screen.getByRole("option", { name: new RegExp(NR_TEMPLATES.GROUP_LABEL("g-2")) })).toBeInTheDocument();
  });

  it("lists the creator's earlier runs with when, notes the recheck, and Continue opens Review on the one chosen", async () => {
    serve({ intents: SCENARIO_LAUNCH_INTENTS.member });
    show();
    await userEvent.click(await screen.findByRole("button", { name: NR_SHAPE.SOURCE_AGAIN }));
    expect(screen.queryByRole("button", { name: NR_SHAPE.AGENT_TITLE })).toBeNull();
    const combo = await screen.findByRole("combobox", { name: NR_SHAPE.PREVIOUS_LABEL });
    await waitFor(() => expect(combo).not.toBeDisabled());
    await userEvent.click(combo);
    expect(screen.getByText(NR_SHAPE.AGAIN_NOTE)).toBeInTheDocument();
    await userEvent.click(await screen.findByRole("option", { name: new RegExp(`^${RUN.title} · `) }));
    await userEvent.click(screen.getByRole("button", { name: NR_SHARED.CONTINUE }));
    await waitFor(() => expect(path()).toBe(`/runs/new?again=${RUN.run_id}&step=review`));
  });

  it("says so when there is nothing to start from", async () => {
    show();
    await userEvent.click(await screen.findByRole("button", { name: NR_SHAPE.SOURCE_TEMPLATE }));
    expect(await screen.findByText(NR_SHAPE.EMPTY_TEMPLATES)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: NR_SHARED.CONTINUE })).toBeDisabled();
    await userEvent.click(source(NR_SHAPE.SOURCE_AGAIN));
    expect(await screen.findByText(NR_SHAPE.EMPTY_AGAIN)).toBeInTheDocument();
    expect(screen.queryByRole("combobox")).toBeNull();
    expect(screen.getByRole("button", { name: NR_SHARED.CONTINUE })).toBeDisabled();
  });

  it("shows the server's sentence when the catalogue cannot be read", async () => {
    serve({ templates: json({ error: "This server keeps no templates.", reason: "templates_unavailable" }, 501) });
    show();
    await userEvent.click(await screen.findByRole("button", { name: NR_SHAPE.SOURCE_TEMPLATE }));
    expect(await screen.findByRole("alert")).toHaveTextContent("This server keeps no templates.");
  });

  it("shows the draft's own template and keeps the revision its link pinned", async () => {
    const loaders: SeedLoaders = {
      ...noLoaders,
      template: () => Promise.resolve({ origin: { kind: "template", id: POLICY.id, revision: 1, name: POLICY.name, templateKind: "policy" }, intent: { shape: "application" } }),
    };
    serve({ templates: { templates: [NIGHTLY, POLICY], publish: SCENARIO_TEMPLATES.governed.publish } });
    show(`/runs/new?template=${POLICY.id}&rev=1&step=shape`, loaders);
    const combo = await screen.findByRole("combobox", { name: NR_SHAPE.TEMPLATE_LABEL });
    await waitFor(() => expect(combo).toHaveTextContent(NR_SHAPE.TEMPLATE_OPTION(POLICY.name, NR_TEMPLATES.SCOPE_ORG)));
    await userEvent.click(screen.getByRole("button", { name: NR_SHARED.CONTINUE }));
    await waitFor(() => expect(path()).toBe(`/runs/new?template=${POLICY.id}&rev=1&step=review`));
  });

  it("drops a seeded template for a blank draft and stays on Shape without the template in the URL", async () => {
    const loaders: SeedLoaders = {
      ...noLoaders,
      template: () => Promise.resolve({ origin: { kind: "template", id: NIGHTLY.id, revision: 2, name: "Nightly", templateKind: "template" }, intent: { shape: "application" } }),
    };
    serve({ templates: SCENARIO_TEMPLATES.governed });
    show(`/runs/new?template=${NIGHTLY.id}&rev=2&step=shape`, loaders);
    expect(await screen.findByRole("button", { name: NR_SHAPE.SOURCE_TEMPLATE })).toHaveAttribute("aria-pressed", "true");
    await userEvent.click(source(NR_SHAPE.SOURCE_SCRATCH));
    await waitFor(() => expect(path()).toBe("/runs/new?step=shape"));
    expect(await screen.findByRole("button", { name: NR_SHAPE.SOURCE_SCRATCH })).toHaveAttribute("aria-pressed", "true");
    expect(JSON.parse(screen.getByTestId("screen").textContent!.slice("shape scratch ".length))).toEqual({});
  });
});
