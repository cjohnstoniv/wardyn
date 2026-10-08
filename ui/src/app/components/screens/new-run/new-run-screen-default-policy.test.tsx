/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// New Run's "Use the default policy" mode (mock packet 088-mock C2): the third
// card of the policy mode row. Its own copy of the screen's mock harness, as
// new-run-screen-saved-policy.test.tsx keeps.
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { act, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";

vi.mock("../../../lib/api/setup", () => ({
  setup: { getSetupStatus: () => Promise.resolve({}) },
}));
vi.mock("../../../lib/api/health", () => ({
  health: { health: () => Promise.resolve({ confinement_classes: ["CC1"] }), whoami: () => Promise.resolve(null) },
}));
const getDefaultPolicyMock = vi.fn();
const listPoliciesMock = vi.fn();
vi.mock("../../../lib/api/policies", () => ({
  policies: {
    listPolicies: (...a: unknown[]) => listPoliciesMock(...a),
    createPolicy: vi.fn(),
    getDefaultPolicy: (...a: unknown[]) => getDefaultPolicyMock(...a),
  },
}));
const createRunMock = vi.fn();
const preflightRunMock = vi.fn();
vi.mock("../../../lib/api/policy-preview", () => ({ previewRunPolicy: vi.fn().mockResolvedValue({ spec: {}, pending: [], warnings: [], repository_access: [] }) }));
vi.mock("../../../lib/api/runs", async () => ({
  ...await vi.importActual<typeof import("../../../lib/api/runs")>("../../../lib/api/runs"),
  runs: {
    createRun: (...a: unknown[]) => createRunMock(...a),
    listRuns: () => Promise.resolve([]),
    preflightRun: (...a: unknown[]) => preflightRunMock(...a),
    gradePolicy: () => Promise.resolve({ risk_assessment: [], overall_risk: "low" }),
  },
}));
const listWorkspacesMock = vi.fn();
vi.mock("../../../lib/api/workspaces", () => ({
  workspaces: { listWorkspaces: (...a: unknown[]) => listWorkspacesMock(...a) },
}));
const myCapabilitiesMock = vi.fn();
vi.mock("../../../lib/capabilities", async () => {
  const actual = await vi.importActual<typeof import("../../../lib/capabilities")>(
    "../../../lib/capabilities",
  );
  return { ...actual, useMyCapabilities: (...a: unknown[]) => myCapabilitiesMock(...a) };
});

import { NewRunScreen } from "./new-run-screen";
import { OperatorProvider } from "../../wardyn/operator-context";
import { POLICY_TEMPLATE_COPY as C } from "../../wardyn/copy/policy-templates";
import { setField } from "../../../../test/set-field";

const user = userEvent.setup({ pointerEventsCheck: 0 });

const DEFAULT_SPEC = {
  allowed_domains: ["api.default.example"],
  first_use_approval: "deny_with_review" as const,
  min_confinement_class: "CC1" as const,
};

function renderScreen() {
  return render(
    <MemoryRouter>
      <OperatorProvider principal="test-owner" operator>
        <NewRunScreen />
      </OperatorProvider>
    </MemoryRouter>,
  );
}

const chooseDefault = async () =>
  user.click(await screen.findByRole("button", { name: new RegExp(C.DEFAULT_TITLE) }));

beforeEach(() => {
  createRunMock.mockReset().mockResolvedValue({ id: "run_1" });
  preflightRunMock.mockReset().mockResolvedValue({
    setup_items: [],
    enforced_confinement_class: "CC1",
    overall_risk: "low",
    warnings: [],
  });
  getDefaultPolicyMock.mockReset().mockResolvedValue(DEFAULT_SPEC);
  listPoliciesMock.mockReset().mockResolvedValue([]);
  listWorkspacesMock.mockReset().mockResolvedValue([]);
  myCapabilitiesMock.mockReset().mockReturnValue(null);
});

describe("NewRunScreen — Use the default policy", () => {
  it("the mode row reads Default, Saved, Custom, with Custom preselected", async () => {
    renderScreen();
    const def = await screen.findByRole("button", { name: /^Use the default policy/ });
    const saved = screen.getByRole("button", { name: /^Reuse a saved policy/ });
    const custom = screen.getByRole("button", { name: /^Custom policy/ });
    expect(Array.from(def.parentElement!.children)).toEqual([def, saved, custom]);
    expect(custom).toHaveAttribute("aria-pressed", "true");
    expect(def).toHaveAttribute("aria-pressed", "false");
  });

  it("sends neither policy_id nor inline_policy, and Check again sends the same body", async () => {
    renderScreen();
    await chooseDefault();
    setField(screen.getByLabelText("Title"), "default run");

    await user.click(screen.getByRole("button", { name: /^Check again$/ }));
    await waitFor(() => expect(preflightRunMock).toHaveBeenCalled());
    await user.click(screen.getByRole("button", { name: "Launch run" }));
    await waitFor(() => expect(createRunMock).toHaveBeenCalled());

    const body = createRunMock.mock.calls[0][0] as Record<string, unknown>;
    expect(body).not.toHaveProperty("policy_id");
    expect(body).not.toHaveProperty("inline_policy");
    const checked = preflightRunMock.mock.lastCall![0] as Record<string, unknown>;
    expect(checked).not.toHaveProperty("policy_id");
    expect(checked).not.toHaveProperty("inline_policy");
    expect(checked).toEqual(body);
  });

  it("a picked local_dir workspace still travels by reference in default mode", async () => {
    listWorkspacesMock.mockResolvedValue([
      { id: "ws1", name: "repo-a", kind: "local_dir", source: "/home/agent/repo-a", status: "scanned" },
    ]);
    render(
      <MemoryRouter
        initialEntries={[
          { pathname: "/runs/new", state: { prefill: { inlinePolicy: false, state: { workspaces: [{ workspaceId: "ws1" }] } } } },
        ]}
      >
        <OperatorProvider principal="test-owner" operator>
          <NewRunScreen />
        </OperatorProvider>
      </MemoryRouter>,
    );
    await chooseDefault();
    setField(screen.getByLabelText("Title"), "default run");
    await user.click(screen.getByRole("button", { name: /^Check again$/ }));
    await waitFor(() => expect(preflightRunMock).toHaveBeenCalled());
    await user.click(screen.getByRole("button", { name: "Launch run" }));
    await waitFor(() => expect(createRunMock).toHaveBeenCalled());

    const body = createRunMock.mock.calls[0][0] as Record<string, unknown>;
    expect(body.workspace_id).toBe("ws1");
    expect(body).not.toHaveProperty("policy_id");
    expect(body).not.toHaveProperty("inline_policy");
    expect(preflightRunMock.mock.lastCall![0]).toEqual(body);
  });

  it("shows the read-only preview and hides the editor and the additions box", async () => {
    renderScreen();
    expect(screen.getByLabelText("Spec (JSON)")).toBeInTheDocument();
    await chooseDefault();
    expect(await screen.findByText(C.DEFAULT_PREVIEW)).toBeInTheDocument();
    expect(await screen.findByText(/api\.default\.example/)).toBeInTheDocument();
    // The words, not the constant: the workspace still mounts, and the note must not say otherwise.
    expect(
      screen.getByText(
        "This run launches under this policy as it stands. Your attached workspace mounts into it; nothing else on this page is merged.",
      ),
    ).toBeInTheDocument();
    expect(screen.queryByLabelText("Spec (JSON)")).toBeNull();
    expect(screen.queryByTestId("run-spec-additions")).toBeNull();
    expect(screen.queryByTestId("safety-meter")).toBeNull();
    expect(screen.getByRole("button", { name: /^Check again$/ })).toBeEnabled();
  });

  it("neither the spec-parse refusal nor the pick-a-policy refusal fires in default mode", async () => {
    renderScreen();
    // Break the Custom document, then choose Default: the broken document is not on the wire.
    setField(await screen.findByLabelText("Spec (JSON)"), "{ not json");
    expect(await screen.findByText("The policy spec isn't valid JSON.")).toBeInTheDocument();
    await chooseDefault();
    expect(screen.queryByText("The policy spec isn't valid JSON.")).toBeNull();
    expect(screen.queryByText("Pick a saved policy, or write a custom one.")).toBeNull();
    setField(screen.getByLabelText("Title"), "default run");
    await user.click(screen.getByRole("button", { name: "Launch run" }));
    await waitFor(() => expect(createRunMock).toHaveBeenCalled());
  });

  it("names the profile on the card when GET /policies/default carries one", async () => {
    getDefaultPolicyMock.mockResolvedValue({ ...DEFAULT_SPEC, governance_profile_name: "Platform team" });
    renderScreen();
    expect(await screen.findByText(C.DEFAULT_HINT_PROFILE("Platform team"))).toBeInTheDocument();
    expect(screen.queryByText(C.DEFAULT_HINT)).toBeNull();
  });

  it("reads the deployment hint without a profile", async () => {
    renderScreen();
    expect(await screen.findByText(C.DEFAULT_HINT)).toBeInTheDocument();
  });

  it("a failed preview read says so, offers Retry, and leaves Launch enabled", async () => {
    getDefaultPolicyMock.mockRejectedValueOnce(new Error("down"));
    renderScreen();
    await chooseDefault();
    expect(await screen.findByText(C.DEFAULT_UNAVAILABLE)).toBeInTheDocument();
    setField(screen.getByLabelText("Title"), "default run");
    expect(screen.getByRole("button", { name: "Launch run" })).toBeEnabled();

    await user.click(screen.getByRole("button", { name: "Retry" }));
    expect(await screen.findByText(/api\.default\.example/)).toBeInTheDocument();
    expect(screen.queryByText(C.DEFAULT_UNAVAILABLE)).toBeNull();
  });

  it("Custom -> Default -> Custom keeps the Custom document", async () => {
    renderScreen();
    const edited = JSON.stringify({ ...DEFAULT_SPEC, allowed_domains: ["mine.example"] });
    setField(await screen.findByLabelText("Spec (JSON)"), edited);
    await chooseDefault();
    await user.click(screen.getByRole("button", { name: /^Custom policy/ }));
    expect((screen.getByLabelText("Spec (JSON)") as HTMLTextAreaElement).value).toBe(edited);
  });

  it("the existing options still behave: Reuse a saved policy is the title and launches by reference", async () => {
    listPoliciesMock.mockResolvedValue([{ id: "pol_1", name: "Team policy", spec: DEFAULT_SPEC }]);
    renderScreen();
    await user.click(await screen.findByRole("button", { name: /^Reuse a saved policy/ }));
    await user.click(screen.getByRole("combobox", { name: "Saved policy" }));
    await user.click(await screen.findByRole("option", { name: "Team policy" }));
    setField(screen.getByLabelText("Title"), "saved run");
    await user.click(screen.getByRole("button", { name: "Launch run" }));
    await waitFor(() => expect(createRunMock).toHaveBeenCalled());
    const body = createRunMock.mock.calls[0][0] as Record<string, unknown>;
    expect(body.policy_id).toBe("pol_1");
    expect(body).not.toHaveProperty("inline_policy");
  });

  it("Custom still sends the document inline", async () => {
    renderScreen();
    setField(await screen.findByLabelText("Title"), "custom run");
    await user.click(screen.getByRole("button", { name: "Launch run" }));
    await waitFor(() => expect(createRunMock).toHaveBeenCalled());
    expect(createRunMock.mock.calls[0][0]).toHaveProperty("inline_policy");
  });
});

// The default lane carries one workspace by reference and the API has no
// second attachment on that path, so a second attached workspace holds Launch
// and Check again rather than being left out of the request.
describe("NewRunScreen — the default policy with two workspaces attached", () => {
  const SENTENCE =
    "The default policy launches with one workspace. Remove the extra workspace, or choose Custom policy to keep them all.";

  async function renderTwoAttachedOnDefault() {
    listWorkspacesMock.mockResolvedValue([
      { id: "ws1", name: "repo-a", kind: "local_dir", source: "/data/a", sources: [{ type: "local_dir", path: "/data/a", target: "/work/a" }], status: "scanned" },
      { id: "ws2", name: "repo-b", kind: "local_dir", source: "/data/b", sources: [{ type: "local_dir", path: "/data/b", target: "/work/b" }], status: "scanned" },
    ]);
    render(
      <MemoryRouter
        initialEntries={[
          {
            pathname: "/runs/new",
            state: { prefill: { inlinePolicy: true, state: { workspaces: [{ workspaceId: "ws1" }, { workspaceId: "ws2" }] } } },
          },
        ]}
      >
        <OperatorProvider principal="test-owner" operator>
          <NewRunScreen />
        </OperatorProvider>
      </MemoryRouter>,
    );
    await chooseDefault();
    await waitFor(() => expect(screen.getByTestId("nr-workspace-extras")).toHaveTextContent("repo-b"));
    setField(screen.getByLabelText("Title"), "default two workspaces");
  }

  it("holds Launch and Check again, says why once, and sends no request", async () => {
    await renderTwoAttachedOnDefault();

    // Once, in the default-policy panel beside Check again; the rail does not
    // repeat it. Both held buttons name it as their description.
    await waitFor(() => expect(screen.getAllByText(SENTENCE)).toHaveLength(1));
    const hold = screen.getByText(SENTENCE);
    expect(hold).toHaveAttribute("role", "status");
    expect(hold.closest("aside")).toBeNull();
    expect(screen.getByRole("button", { name: "Launch run" })).toBeDisabled();
    expect(screen.getByRole("button", { name: /^Check again$/ })).toBeDisabled();
    expect(screen.getByRole("button", { name: "Launch run" })).toHaveAccessibleDescription(SENTENCE);
    expect(screen.getByRole("button", { name: /^Check again$/ })).toHaveAccessibleDescription(SENTENCE);
    // Nothing is hidden or cleared, and the mode never switches by itself.
    expect(screen.getByRole("button", { name: "Remove repo-b" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: new RegExp(C.DEFAULT_TITLE) })).toHaveAttribute("aria-pressed", "true");

    // No launch, and no automatic check either, once the debounce has passed.
    preflightRunMock.mockClear();
    await user.click(screen.getByRole("button", { name: "Launch run" }));
    await act(async () => {
      await new Promise((r) => setTimeout(r, 1000));
    });
    expect(createRunMock).not.toHaveBeenCalled();
    expect(preflightRunMock).not.toHaveBeenCalled();
  });

  it("coexists with the default-policy read failing: Retry stays and the hold stays", async () => {
    getDefaultPolicyMock.mockRejectedValue(new Error("boom"));
    await renderTwoAttachedOnDefault();
    expect(await screen.findByText(C.DEFAULT_UNAVAILABLE)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Retry" })).toBeInTheDocument();
    expect(screen.getAllByText(SENTENCE)).toHaveLength(1);
    expect(screen.getByRole("button", { name: "Launch run" })).toBeDisabled();
  });

  it("launches with the remaining workspace once the extra one is removed", async () => {
    await renderTwoAttachedOnDefault();
    await user.click(screen.getByRole("button", { name: "Remove repo-b" }));

    await waitFor(() => expect(screen.queryByText(SENTENCE)).toBeNull());
    await user.click(screen.getByRole("button", { name: "Launch run" }));
    await waitFor(() => expect(createRunMock).toHaveBeenCalled());
    const body = createRunMock.mock.calls[0][0] as Record<string, unknown>;
    expect(body.workspace_id).toBe("ws1");
    expect(body).not.toHaveProperty("policy_id");
    expect(body).not.toHaveProperty("inline_policy");
    expect(JSON.stringify(body)).not.toContain("ws2");
  });

  it("keeps both workspaces when Custom policy is chosen instead", async () => {
    await renderTwoAttachedOnDefault();
    await user.click(screen.getByRole("button", { name: /^Custom policy/ }));

    await waitFor(() => expect(screen.queryByText(SENTENCE)).toBeNull());
    expect(screen.getByTestId("nr-workspace-extras")).toHaveTextContent("repo-b");
    await user.click(screen.getByRole("button", { name: "Launch run" }));
    await waitFor(() => expect(createRunMock).toHaveBeenCalled());
    const body = createRunMock.mock.calls[0][0] as { inline_policy: { workspace_mounts: { source: string }[] } };
    expect(body.inline_policy.workspace_mounts.map((m) => m.source)).toEqual(["/data/a", "/data/b"]);
  });
});

describe("NewRunScreen — the default preview's loading line", () => {
  beforeEach(() => vi.useFakeTimers({ shouldAdvanceTime: true }));
  afterEach(() => vi.useRealTimers());

  it("appears only after a second of waiting", async () => {
    getDefaultPolicyMock.mockReturnValue(new Promise(() => {}));
    renderScreen();
    await chooseDefault();
    expect(screen.queryByText(C.DEFAULT_LOADING)).toBeNull();
    await act(async () => {
      vi.advanceTimersByTime(1100);
    });
    expect(screen.getByText(C.DEFAULT_LOADING).closest("[role=status]")).not.toBeNull();
    // The cards keep working while it loads.
    await user.click(screen.getByRole("button", { name: /^Custom policy/ }));
    expect(screen.getByLabelText("Spec (JSON)")).toBeInTheDocument();
  });
});
