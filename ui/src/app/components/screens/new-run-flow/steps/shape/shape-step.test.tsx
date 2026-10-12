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
const radio = (name: string) => screen.getByRole("radio", { name });

beforeEach(() => serve({}));
afterEach(() => {
  cleanup();
  wfetchMock.mockReset();
});

describe("Shape step: workload", () => {
  it("shows the three workload rows with their sub-lines, none chosen, Continue to Run", async () => {
    show();
    expect(await screen.findByRole("heading", { level: 1, name: NR_SHAPE.TITLE })).toHaveFocus();
    expect(screen.getByText(NR_SHARED.EYEBROW(1))).toBeInTheDocument();
    for (const [title, sub] of [[NR_SHAPE.AGENT_TITLE, NR_SHAPE.AGENT_SUB], [NR_SHAPE.ENV_TITLE, NR_SHAPE.ENV_SUB], [NR_SHAPE.APP_TITLE, NR_SHAPE.APP_SUB]]) {
      expect(radio(title)).not.toBeChecked();
      expect(screen.getByText(sub)).toBeInTheDocument();
    }
    expect(screen.getByRole("button", { name: NR_SHARED.CONTINUE_TO(NR_SHARED.STEP_RUN) })).toBeInTheDocument();
  });

  it("writes the shape and the carriers that say it, then Continue opens Run", async () => {
    show();
    await userEvent.click(await screen.findByRole("radio", { name: NR_SHAPE.APP_TITLE }));
    await userEvent.click(screen.getByRole("button", { name: NR_SHARED.CONTINUE_TO(NR_SHARED.STEP_RUN) }));
    const drawn = await screen.findByText(/^run /);
    expect(JSON.parse(drawn.textContent!.slice("run scratch ".length))).toEqual({
      shape: "application", experience: "background", workload: { kind: "command", command: "" },
    });
    expect(path()).toBe("/runs/new?step=run");
  });

  it("switching the workload replaces the earlier workload's carriers", async () => {
    show();
    await userEvent.click(await screen.findByRole("radio", { name: NR_SHAPE.APP_TITLE }));
    await userEvent.click(radio(NR_SHAPE.ENV_TITLE));
    expect(JSON.parse(screen.getByTestId("screen").textContent!.slice("shape scratch ".length))).toEqual({ shape: "environment", experience: "interactive" });
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
  it("lists both kinds of template with scope labels, a policy marked, and opens Review on the pinned revision", async () => {
    serve({ templates: { templates: [NIGHTLY, POLICY], publish: SCENARIO_TEMPLATES.governed.publish } });
    show();
    await userEvent.click(await screen.findByRole("radio", { name: NR_SHAPE.SOURCE_TEMPLATE }));
    await userEvent.click(await screen.findByRole("combobox", { name: NR_SHAPE.TEMPLATE_LABEL }));
    expect(await screen.findByRole("option", { name: NR_SHAPE.TEMPLATE_OPTION(NIGHTLY.name, NR_TEMPLATES.SCOPE_ME) })).toBeInTheDocument();
    const policy = screen.getByRole("option", { name: new RegExp(`^${POLICY.name} · ${NR_TEMPLATES.SCOPE_ORG}`) });
    expect(within(policy).getByText(NR_TEMPLATES.POLICY_BADGE)).toBeInTheDocument();
    await userEvent.click(policy);
    await waitFor(() => expect(path()).toBe(`/runs/new?template=${POLICY.id}&rev=${POLICY.revision}&step=review`));
  });

  it("labels a group's template with its display label, then the raw identifier", async () => {
    serve({ templates: { templates: [{ ...NIGHTLY, scope: "group", group_id: "g-1", group_label: "Platform" }, { ...POLICY, scope: "group", group_id: "g-2" }], publish: SCENARIO_TEMPLATES.governed.publish } });
    show();
    await userEvent.click(await screen.findByRole("radio", { name: NR_SHAPE.SOURCE_TEMPLATE }));
    await userEvent.click(await screen.findByRole("combobox", { name: NR_SHAPE.TEMPLATE_LABEL }));
    expect(await screen.findByRole("option", { name: new RegExp(NR_TEMPLATES.GROUP_LABEL("Platform")) })).toBeInTheDocument();
    expect(screen.getByRole("option", { name: new RegExp(NR_TEMPLATES.GROUP_LABEL("g-2")) })).toBeInTheDocument();
  });

  it("lists the creator's earlier runs with when, notes the recheck, and opens Review on the one chosen", async () => {
    serve({ intents: SCENARIO_LAUNCH_INTENTS.member });
    show();
    await userEvent.click(await screen.findByRole("radio", { name: NR_SHAPE.SOURCE_AGAIN }));
    await userEvent.click(await screen.findByRole("combobox", { name: NR_SHAPE.PREVIOUS_LABEL }));
    expect(screen.getByText(NR_SHAPE.AGAIN_NOTE)).toBeInTheDocument();
    await userEvent.click(await screen.findByRole("option", { name: new RegExp(`^${RUN.title} · `) }));
    await waitFor(() => expect(path()).toBe(`/runs/new?again=${RUN.run_id}&step=review`));
  });

  it("says so when there is nothing to start from", async () => {
    show();
    await userEvent.click(await screen.findByRole("radio", { name: NR_SHAPE.SOURCE_TEMPLATE }));
    expect(await screen.findByText(NR_SHAPE.EMPTY_TEMPLATES)).toBeInTheDocument();
    await userEvent.click(radio(NR_SHAPE.SOURCE_AGAIN));
    expect(await screen.findByText(NR_SHAPE.EMPTY_AGAIN)).toBeInTheDocument();
    expect(screen.queryByRole("combobox")).toBeNull();
  });

  it("shows the server's sentence when the catalogue cannot be read", async () => {
    serve({ templates: json({ error: "This server keeps no templates.", reason: "templates_unavailable" }, 501) });
    show();
    await userEvent.click(await screen.findByRole("radio", { name: NR_SHAPE.SOURCE_TEMPLATE }));
    expect(await screen.findByRole("alert")).toHaveTextContent("This server keeps no templates.");
  });

  it("drops a seeded template for a blank draft and stays on Shape without the template in the URL", async () => {
    const loaders: SeedLoaders = {
      ...noLoaders,
      template: () => Promise.resolve({ origin: { kind: "template", id: NIGHTLY.id, revision: 2, name: "Nightly", templateKind: "template" }, intent: { shape: "application" } }),
    };
    serve({ templates: SCENARIO_TEMPLATES.governed });
    show(`/runs/new?template=${NIGHTLY.id}&rev=2&step=shape`, loaders);
    expect(await screen.findByRole("radio", { name: NR_SHAPE.SOURCE_TEMPLATE })).toBeChecked();
    await userEvent.click(radio(NR_SHAPE.SOURCE_SCRATCH));
    await waitFor(() => expect(path()).toBe("/runs/new?step=shape"));
    expect(await screen.findByRole("radio", { name: NR_SHAPE.SOURCE_SCRATCH })).toBeChecked();
    expect(radio(NR_SHAPE.APP_TITLE)).not.toBeChecked();
  });
});
