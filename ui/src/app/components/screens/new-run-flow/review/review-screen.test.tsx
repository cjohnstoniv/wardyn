/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, Route, Routes, useLocation } from "react-router-dom";
import { setField } from "../../../../../test/set-field";
import { OperatorProvider } from "../../../wardyn/operator-context";
import { NR_FILES } from "../../../wardyn/copy/new-run/files";
import { NR_REVIEW } from "../../../wardyn/copy/new-run/review";
import { NR_SAVE } from "../../../wardyn/copy/new-run/save";
import { NR_SHAPE } from "../../../wardyn/copy/new-run/shape";
import { NR_TEMPLATES } from "../../../wardyn/copy/new-run/templates";
import { NR_SHARED } from "../../../wardyn/copy/new-run/shared";
import { RUN_WAIT } from "../../../../lib/runner-pool-refusals";
import { SCENARIO_PREFLIGHT } from "../../../../lib/new-run-fixtures/scenarios/preflight";
import { SCENARIO_ACCESS } from "../../../../lib/new-run-fixtures/scenarios/tools";
import type { PreflightResult } from "../../../../lib/types/preflight";
import { intentToWire } from "../draft/intent-to-wire";
import { PREFLIGHT_FRESH_MS } from "../draft/launch-gate";
import type { RunIntent } from "../draft/run-intent";
import { NEW_RUN_SCREENS, NewRunRoute, useNewRunDraft, type Seed, type SeedLoaders } from "../new-run-route";
import { runDocumentText } from "../document/run-document";
import { ReviewScreen } from "./review-screen";

const { checks, wfetchMock } = vi.hoisted(() => ({
  checks: { current: {} as Record<string, unknown>, params: undefined as { body: unknown } | undefined },
  wfetchMock: vi.fn(),
}));
vi.mock("../draft/use-intent-checks", () => ({
  useDraftIdentity: () => ({ principal: "alice", resolved: true, revision: 1, authGeneration: 0 }),
  useIntentChecks: (params: { body: unknown }) => {
    checks.params = params;
    return checks.current;
  },
}));
vi.mock("../../../../lib/api/core", async (orig) => ({ ...(await orig<typeof import("../../../../lib/api/core")>()), wfetch: (...a: unknown[]) => wfetchMock(...a) }));

const GENERATION = 5;
const RUN_ID = "11111111-1111-4111-8111-111111111111";
const TEMPLATE_ID = "22222222-2222-4222-8222-222222222222";
const preflight = vi.fn();
const retry = vi.fn();
const answered = (pre: PreflightResult | null, age = 1000, over: Record<string, unknown> = {}) => ({
  generation: GENERATION,
  preflightRead: pre ? { generation: GENERATION, at: Date.now() - age } : null,
  preflight,
  preflightResult: pre,
  preflightError: null,
  preflightReason: "",
  preview: { result: null, retry },
  ...over,
});

const AGENT: RunIntent = {
  shape: "agent_task", experience: "interactive", runner_pool_id: "pool-general", no_repositories_or_drives: true,
  workload: { kind: "agent_task", agent: "claude-code" },
  tools: [{ id: "claude-code", kind: "harness" }],
  startup: { kind: "harness", tool: "claude-code", prompt: "Fix the build" },
};
const APP: RunIntent = { shape: "application", experience: "background", workload: { kind: "command", command: "make test" }, no_repositories_or_drives: true };

function Probe() {
  const draft = useNewRunDraft();
  return <p data-testid="intent">{JSON.stringify(draft.intent)}</p>;
}
function Where() {
  const l = useLocation();
  return <p data-testid="path">{l.pathname + l.search}</p>;
}
const json = (body: unknown) => new Response(JSON.stringify(body), { status: 200, headers: { "content-type": "application/json" } });

interface Open {
  /** Which door seeded the draft: a template may open Review with setup left to do. */
  via?: "again" | "template";
  query?: string;
  needs?: { field: string }[];
  onLaunch?: () => void;
  /** The read of the template's document; a template origin only. */
  templateRead?: () => Promise<Response>;
}
function open(intent: RunIntent, o: Open = {}) {
  const loaders: SeedLoaders = {
    template: async (): Promise<Seed> => ({ origin: { kind: "template", id: TEMPLATE_ID, revision: 1, name: "Nightly", templateKind: "template" }, intent }),
    again: async (): Promise<Seed> => ({ origin: { kind: "again", runId: RUN_ID, title: "Nightly" }, intent }),
  };
  wfetchMock.mockImplementation(async (path: string, init?: { method?: string }) => {
    if (path === "/templates" && init?.method === "POST") return json({ id: "t9", kind: "template" });
    if (path === "/templates") return json({ templates: [], publish: { org: false, groups: [], groups_unverified: false } });
    if (path.startsWith("/templates/")) return o.templateRead ? o.templateRead() : json({ document: { needs_setup: o.needs ?? [] } });
    throw new Error(`unexpected ${path}`);
  });
  const door = o.via === "template" ? `template=${TEMPLATE_ID}&rev=1` : `again=${RUN_ID}`;
  render(
    <MemoryRouter initialEntries={[`/runs/new?${door}${o.query ?? ""}`]}>
      <OperatorProvider operator={false} principal="alice">
        <Routes>
          <Route
            path="/runs/new"
            element={<><NewRunRoute loaders={loaders} screens={{ ...NEW_RUN_SCREENS, review: (p) => <><ReviewScreen {...p} onLaunch={o.onLaunch} /><Probe /></> }} /><Where /></>}
          />
        </Routes>
      </OperatorProvider>
    </MemoryRouter>,
  );
}
const launch = () => screen.getByRole("button", { name: NR_REVIEW.LAUNCH_RUN });
const ctrlEnter = () => fireEvent.keyDown(document, { key: "Enter", ctrlKey: true });
const path = () => screen.getByTestId("path").textContent;
const blockerList = () => document.getElementById("nr-blockers") as HTMLElement;
const group = (id: string) => within(document.querySelector(`[data-review-group=${id}]`) as HTMLElement);
const intentNow = () => JSON.parse(screen.getByTestId("intent").textContent ?? "{}") as RunIntent;
const ready = async () => screen.findByRole("button", { name: NR_REVIEW.LAUNCH_RUN });

beforeEach(() => {
  checks.current = answered(SCENARIO_PREFLIGHT.member);
  preflight.mockReset();
  retry.mockReset();
});
afterEach(() => {
  cleanup();
  wfetchMock.mockReset();
});

describe("Review Launch gate (U11)", () => {
  it.each([
    ["no answer yet", answered(null)],
    ["an answer older than a minute", answered(SCENARIO_PREFLIGHT.member, PREFLIGHT_FRESH_MS + 1)],
    ["an answer for another body", { ...answered(SCENARIO_PREFLIGHT.member), generation: GENERATION + 1 }],
    ["a fresh answer beside a missing setup item", answered(SCENARIO_PREFLIGHT.app)],
  ])("%s keeps Launch shut, described by the blocker list, and Ctrl+Enter does nothing", async (_case, state) => {
    checks.current = state;
    const onLaunch = vi.fn();
    open(AGENT, { onLaunch });
    await ready();
    expect(launch()).toBeDisabled();
    expect(launch()).toHaveAttribute("aria-describedby", "nr-blockers");
    expect(blockerList()).toHaveTextContent(/\S/);
    ctrlEnter();
    fireEvent.click(launch());
    expect(onLaunch).not.toHaveBeenCalled();
  });

  it("opens on a clean, current, fresh check, by click and by Ctrl+Enter", async () => {
    const onLaunch = vi.fn();
    open(AGENT, { onLaunch });
    await ready();
    expect(launch()).toBeEnabled();
    expect(blockerList()).toBeEmptyDOMElement();
    ctrlEnter();
    expect(onLaunch).toHaveBeenCalledTimes(1);
    fireEvent.click(launch());
    expect(onLaunch).toHaveBeenCalledTimes(2);
  });

  describe("the Azure DevOps sign-in item", () => {
    const ADO_ITEM = { id: "repo_credential:azure_devops:acme", kind: "repo_credential", label: "Sign in to Azure DevOps", required_by: "acme", status: "missing", fix: { action: "sign_in", target: "azure_devops" } };
    const accessWith = (setup: unknown[]) => ({ ...SCENARIO_ACCESS.member, setup });
    const previewWith = (setup: unknown[]) => ({ result: { spec: {}, source: { name: "Default" }, pending: [], warnings: [], repository_access: [], access: accessWith(setup) }, retry });
    const withAdo = { ...SCENARIO_PREFLIGHT.member, access: accessWith([ADO_ITEM]) } as unknown as PreflightResult;

    it("leaves Launch open when the policy preview shows no such item, so the launch refusal opens the door", async () => {
      checks.current = answered(withAdo, 1000, { preview: previewWith([]) });
      open(AGENT);
      await ready();
      expect(launch()).toBeEnabled();
    });

    it("holds Launch when the preview shows the same item, which the Tools step then offers", async () => {
      checks.current = answered(withAdo, 1000, { preview: previewWith([ADO_ITEM]) });
      open(AGENT);
      await ready();
      expect(launch()).toBeDisabled();
    });
  });

  it("offers Check again while shut, reads both again, and announces once Launch opens", async () => {
    const user = userEvent.setup();
    checks.current = answered(null);
    open(AGENT);
    await user.click(await screen.findByRole("button", { name: NR_REVIEW.CHECK_AGAIN }));
    expect(preflight).toHaveBeenCalledTimes(1);
    expect(retry).toHaveBeenCalledTimes(1);
    checks.current = answered(SCENARIO_PREFLIGHT.member);
    await user.click(screen.getByRole("button", { name: NR_REVIEW.VIEW_YAML }));
    expect(await screen.findByText(NR_REVIEW.CHECKED_ANNOUNCE)).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: NR_REVIEW.CHECK_AGAIN })).toBeNull();
  });
});

describe("Review blockers", () => {
  it("lists them in the design's order, links each to its fix, and keeps Launch shut", async () => {
    const user = userEvent.setup();
    checks.current = answered(SCENARIO_PREFLIGHT.app, 1000, { preflightError: "None of your runners is online.", preflightReason: "runner_offline" });
    const { shape: _shape, ...noShape } = AGENT;
    open({ ...noShape, legacy_extra: 1 } as RunIntent, { via: "template", needs: [{ field: "inline_policy.model_provider" }] });
    await ready();
    await waitFor(() => expect(within(blockerList()).getAllByRole("listitem")).toHaveLength(5));
    const items = within(blockerList()).getAllByRole("listitem").map((li) => li.textContent);
    expect(items).toEqual([
      NR_SHARED.STEP_SHAPE,
      NR_REVIEW.BLOCKER_UNSUPPORTED("legacy_extra"),
      NR_TEMPLATES.NEEDS_SETUP("Model provider"),
      "None of your runners is online.",
      "deploy-key",
    ]);
    expect(blockerList()).toHaveTextContent(NR_REVIEW.BLOCKERS_MANY(5));
    expect(launch()).toBeDisabled();
    await user.click(within(blockerList()).getByRole("button", { name: NR_SHARED.STEP_SHAPE }));
    await waitFor(() => expect(path()).toContain("step=shape"));
  });

  it("says the setup changed, with Check again, when the only thing wrong is a stale result", async () => {
    checks.current = answered(SCENARIO_PREFLIGHT.member, PREFLIGHT_FRESH_MS + 1);
    open(AGENT);
    await ready();
    expect(blockerList()).toHaveTextContent(NR_REVIEW.BLOCKER_STALE);
    expect(blockerList()).toHaveTextContent(NR_REVIEW.BLOCKERS_ONE);
  });
});

describe("Review groups", () => {
  it("shows the six groups with who put each value there", async () => {
    open(AGENT);
    await ready();
    const labels = Array.from(document.querySelectorAll("[data-review-group] h3")).map((h) => h.textContent);
    expect(labels).toEqual([NR_REVIEW.GROUP_RUN, NR_REVIEW.GROUP_WHERE, NR_REVIEW.GROUP_WORK, NR_REVIEW.GROUP_TOOLS, NR_REVIEW.GROUP_ACCESS, NR_REVIEW.GROUP_LIMITS]);
    expect(group("run").getByText(NR_REVIEW.VALUE_AGENT)).toBeInTheDocument();
    expect(group("run").getByText(NR_REVIEW.TIMING_INTERACTIVE)).toBeInTheDocument();
    expect(group("where").getByText(NR_REVIEW.WHERE_VALUE("General"))).toBeInTheDocument();
    expect(group("where").getByText(NR_REVIEW.WHERE_DETAIL("Organization pool", 6, 10))).toBeInTheDocument();
    expect(group("where").getByText(NR_REVIEW.PROV_ORG_DEFAULT)).toBeInTheDocument();
    expect(group("work").getByText(NR_REVIEW.WORK_NONE)).toBeInTheDocument();
    expect(group("tools").getByText(NR_REVIEW.HARNESS_STARTS)).toBeInTheDocument();
    expect(group("tools").getByText("Fix the build")).toBeInTheDocument();
    expect(group("tools").getByText(NR_SHAPE.SOURCE_AGAIN)).toBeInTheDocument();
    expect(group("limits").getByText(NR_SHARED.JOIN(NR_SHARED.CPU("2"), NR_SHARED.MIB("4,096")))).toBeInTheDocument();
  });

  it("shows a document-only field as a read-only row with its Run document chip, no Change on the row", async () => {
    open({ ...AGENT, image: "example/base:1", ends_after_sec: 3600 });
    await ready();
    const where = group("where");
    expect(where.getByText("image")).toBeInTheDocument();
    expect(where.getByText("example/base:1")).toBeInTheDocument();
    expect(where.getByText(NR_REVIEW.DOC_ONLY_CHIP)).toBeInTheDocument();
    const limits = group("limits");
    expect(limits.getByText("ends_after_sec")).toBeInTheDocument();
    expect(limits.getByText(NR_REVIEW.LIMITS_VALUE("2", "4,096", "1 hour"))).toBeInTheDocument();
    expect(within(document.querySelector("[data-review-group=where]") as HTMLElement).getAllByRole("button", { name: new RegExp(NR_REVIEW.CHANGE) })).toHaveLength(1);
  });

  it("opens a group's Change as a disclosure that closes again", async () => {
    const user = userEvent.setup();
    open(AGENT);
    await ready();
    const change = screen.getByRole("button", { name: `${NR_REVIEW.CHANGE} ${NR_REVIEW.GROUP_WORK}` });
    expect(change).toHaveAttribute("aria-expanded", "false");
    await user.click(change);
    const close = screen.getByRole("button", { name: `${NR_REVIEW.CLOSE} ${NR_REVIEW.GROUP_WORK}` });
    expect(close).toHaveAttribute("aria-expanded", "true");
    await user.click(screen.getByRole("button", { name: NR_REVIEW.EDIT_FILES }));
    expect(path()).toContain("step=files");
  });

  it("edits the CPU and memory in place and the draft follows", async () => {
    const user = userEvent.setup();
    open(AGENT);
    await ready();
    await user.click(screen.getByRole("button", { name: `${NR_REVIEW.CHANGE} ${NR_REVIEW.GROUP_LIMITS}` }));
    setField(screen.getByLabelText(NR_REVIEW.CPU_LABEL), "0.5");
    setField(screen.getByLabelText(NR_REVIEW.MEMORY_LABEL), "1536");
    expect(intentNow().resources).toEqual({ cpu_millis: 500, memory_mib: 1536 });
    setField(screen.getByLabelText(NR_REVIEW.CPU_LABEL), "abc");
    expect(screen.getByLabelText(NR_REVIEW.CPU_LABEL)).toHaveAttribute("aria-invalid", "true");
    expect(intentNow().resources?.cpu_millis).toBe(500);
  });

  it("edits an application's command in place", async () => {
    const user = userEvent.setup();
    open(APP);
    await ready();
    expect(group("tools").getByText("make test")).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: `${NR_REVIEW.CHANGE} ${NR_REVIEW.GROUP_TOOLS}` }));
    setField(screen.getByLabelText(NR_REVIEW.COMMAND_LABEL), "make lint");
    expect(intentNow().workload).toEqual({ kind: "command", command: "make lint" });
  });
});

describe("Review notices", () => {
  it("states the pool at its limit in the server's sentence", async () => {
    checks.current = answered(SCENARIO_PREFLIGHT.full);
    open(AGENT);
    await ready();
    expect(screen.getByText(RUN_WAIT.AT_CAPACITY("CI jobs", 40))).toBeInTheDocument();
  });

  it("states what narrowed the request, with its source", async () => {
    checks.current = answered(SCENARIO_PREFLIGHT.governed);
    open({ ...AGENT, resources: { cpu_millis: 16000, memory_mib: 16384 } });
    await ready();
    expect(screen.getByText(NR_REVIEW.NARROWED_BY("Platform team"))).toBeInTheDocument();
    expect(screen.getByText(`${NR_REVIEW.NARROWED_APPLIES("8 CPU · 8,192 MiB")} ${NR_REVIEW.NARROWED_TAIL_CEILING}`)).toBeInTheDocument();
  });

  it("lists the preflight's warnings before launch", async () => {
    checks.current = answered({ ...SCENARIO_PREFLIGHT.member, warnings: ["inline_policy: a grant was dropped by your ceiling"] });
    open(AGENT);
    await ready();
    expect(screen.getByText(NR_REVIEW.WARNINGS_TITLE)).toBeInTheDocument();
    expect(screen.getByText("inline_policy: a grant was dropped by your ceiling")).toBeInTheDocument();
  });
});

describe("Review raw views", () => {
  it("shows the draft's real run document and toggles by replacing the URL", async () => {
    const user = userEvent.setup();
    open(AGENT, { query: "&step=review&view=yaml" });
    await screen.findByRole("heading", { name: NR_REVIEW.RAW_TITLE("YAML") });
    expect(document.querySelector("pre code")?.textContent).toBe(runDocumentText(AGENT, "yaml"));
    await user.click(screen.getByRole("button", { name: NR_REVIEW.VIEW_JSON }));
    await screen.findByRole("heading", { name: NR_REVIEW.RAW_TITLE("JSON") });
    expect(JSON.parse(document.querySelector("pre code")?.textContent ?? "")).toEqual({ api_version: "wardyn/v1", kind: "Run", intent: AGENT });
    expect(path()).toContain("view=json");
    await user.click(screen.getByRole("button", { name: NR_REVIEW.EDIT_FORMAT("JSON") }));
    expect(path()).toContain("step=document");
    expect(path()).toContain("format=json");
  });
});

describe("Review Save as", () => {
  it("reads who it can be shared with, saves without touching the draft, and announces it", async () => {
    const user = userEvent.setup();
    open(AGENT);
    await ready();
    await user.click(screen.getByRole("button", { name: NR_REVIEW.SAVE_TEMPLATE }));
    const dialog = await screen.findByRole("dialog");
    expect(wfetchMock.mock.calls.some(([p, i]) => p === "/templates" && (i as { method?: string } | undefined)?.method === "GET")).toBe(true);
    setField(within(dialog).getByLabelText(NR_SAVE.NAME_LABEL), "Nightly");
    await user.click(within(dialog).getByRole("button", { name: NR_SAVE.CONFIRM_TEMPLATE }));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(screen.getByText(NR_SAVE.SAVED(NR_SHAPE.TEMPLATE_LABEL))).toBeInTheDocument();
    expect(intentNow()).toEqual(AGENT);
  });

  it("saves a policy as a policy", async () => {
    const user = userEvent.setup();
    open(AGENT);
    await ready();
    await user.click(screen.getByRole("button", { name: NR_REVIEW.SAVE_POLICY }));
    expect(await screen.findByRole("dialog")).toHaveTextContent(NR_SAVE.TITLE_POLICY);
  });
});

describe("Review requests", () => {
  it("sends no body while the draft has a local issue, and the serialised draft once it is clean", async () => {
    open({ ...AGENT, tools: [] });
    await ready();
    expect(checks.params?.body).toBeNull();
    cleanup();
    open(AGENT);
    await ready();
    expect(checks.params?.body).toEqual(intentToWire(AGENT).body);
  });
});

describe("Review template setup read", () => {
  const needs = [{ field: "inline_policy.model_provider" }];
  const shut = () => {
    expect(launch()).toBeDisabled();
    expect(blockerList()).toHaveTextContent(NR_REVIEW.BLOCKER_STALE);
  };

  it("keeps Launch shut while the template's setup is still being read", async () => {
    open(AGENT, { via: "template", templateRead: () => new Promise<Response>(() => {}) });
    await ready();
    shut();
  });

  it("keeps Launch shut when the read fails, and Check again reads it again", async () => {
    const user = userEvent.setup();
    let calls = 0;
    open(AGENT, { via: "template", templateRead: () => (++calls === 1 ? Promise.reject(new Error("offline")) : Promise.resolve(json({ document: { needs_setup: [] } }))) });
    await ready();
    await waitFor(() => expect(calls).toBe(1));
    shut();
    await user.click(screen.getByRole("button", { name: NR_REVIEW.CHECK_AGAIN }));
    await waitFor(() => expect(launch()).toBeEnabled());
    expect(calls).toBe(2);
    expect(blockerList()).toBeEmptyDOMElement();
  });

  it("still lists what the template left to set once it is read", async () => {
    open(AGENT, { via: "template", needs });
    await waitFor(() => expect(blockerList()).toHaveTextContent(NR_TEMPLATES.NEEDS_SETUP("Model provider")));
    expect(launch()).toBeDisabled();
  });
});

describe("Review provenance", () => {
  const FILES = ["workspace_mounts"];
  const withProvenance = (source: object) =>
    answered({ ...SCENARIO_PREFLIGHT.member, provenance: [{ field: FILES[0], value: "/work", source, effect: "added" }] } as PreflightResult);

  it("names a Run again value's origin, not the person", async () => {
    open(AGENT);
    await ready();
    expect(group("work").getByText(NR_SHAPE.SOURCE_AGAIN)).toBeInTheDocument();
    expect(group("work").queryByText(NR_REVIEW.PROV_ADDED)).toBeNull();
  });

  it("names the template a value came from", async () => {
    open(AGENT, { via: "template" });
    await ready();
    expect(group("work").getByText(NR_SHARED.JOIN(NR_SHAPE.SOURCE_TEMPLATE, "Nightly"))).toBeInTheDocument();
    expect(group("tools").getByText(NR_SHARED.JOIN(NR_SHAPE.SOURCE_TEMPLATE, "Nightly"))).toBeInTheDocument();
  });

  it("takes the server's provenance row over the draft's account", async () => {
    checks.current = withProvenance({ kind: "policy", name: "Platform team" });
    open(AGENT);
    await ready();
    expect(group("work").getByText(NR_SHARED.JOIN(NR_REVIEW.PROV_SOURCE_POLICY, "Platform team"))).toBeInTheDocument();
    cleanup();
    checks.current = withProvenance({ kind: "person" });
    open(AGENT);
    await ready();
    expect(group("work").getByText(NR_REVIEW.PROV_ADDED)).toBeInTheDocument();
    cleanup();
    checks.current = withProvenance({ kind: "model_provider" });
    open(AGENT);
    await ready();
    expect(group("tools").getByText(NR_REVIEW.PROV_MODEL_PROVIDER)).toBeInTheDocument();
  });

  it("shows the person's own pick as theirs once they change it, and not the template's ask as theirs", async () => {
    const user = userEvent.setup();
    open({ ...AGENT, resources: { cpu_millis: 2000, memory_mib: 4096 } });
    await ready();
    expect(group("limits").getByText(NR_SHAPE.SOURCE_AGAIN)).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: `${NR_REVIEW.CHANGE} ${NR_REVIEW.GROUP_LIMITS}` }));
    setField(screen.getByLabelText(NR_REVIEW.CPU_LABEL), "1");
    expect(group("limits").getByText(NR_REVIEW.PROV_SELECTED)).toBeInTheDocument();
  });

  it("does not call a template's narrowed ask the person's", async () => {
    checks.current = answered(SCENARIO_PREFLIGHT.governed);
    open({ ...AGENT, resources: { cpu_millis: 16000, memory_mib: 16384 } });
    await ready();
    expect(screen.getByText(NR_REVIEW.NARROWED_BY("Platform team"))).toBeInTheDocument();
    expect(screen.queryByText(new RegExp(NR_REVIEW.PROV_ADDED))).toBeNull();
  });
});

describe("Review Files and Limits detail", () => {
  it("reads the main drive's enforcement from facts and its size from the applied amount", async () => {
    checks.current = answered({ ...SCENARIO_PREFLIGHT.member, main_drive: { applied_mib: 20480, facts: { enforcement: "filesystem" } } } as unknown as PreflightResult);
    open(AGENT);
    await ready();
    expect(group("work").getByText(NR_REVIEW.MAIN_DRIVE(20))).toBeInTheDocument();
    expect(group("work").getByText(NR_FILES.ENFORCED)).toBeInTheDocument();
  });

  it("opens the limits editor on what was asked for, not the narrowed amount", async () => {
    const user = userEvent.setup();
    checks.current = answered(SCENARIO_PREFLIGHT.governed);
    open({ ...AGENT, resources: { cpu_millis: 16000, memory_mib: 16384 } });
    await ready();
    await user.click(screen.getByRole("button", { name: `${NR_REVIEW.CHANGE} ${NR_REVIEW.GROUP_LIMITS}` }));
    expect(screen.getByLabelText(NR_REVIEW.CPU_LABEL)).toHaveValue("16");
    expect(screen.getByLabelText(NR_REVIEW.MEMORY_LABEL)).toHaveValue("16384");
  });
});
