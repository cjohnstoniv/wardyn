/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// New Run's Policy panel on the shared policy document (#1922, slice 2): read
// first, an explicit way into the editor and out of it, the source's format
// carried by the draft, an invalid source held with its remedy, and the
// server's preview as the thing being read. The panel body's own rules are in
// wardyn/policy-document/new-run-policy-panel-body.test.tsx; these are the
// screen's: what the draft, the gates and the request do with it.
import { beforeEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";

vi.mock("../../../lib/api/setup", () => ({
  setup: { getSetupStatus: () => Promise.resolve({}) },
}));
const getDefaultPolicyMock = vi.fn();
vi.mock("../../../lib/api/policies", () => ({
  policies: {
    listPolicies: () => Promise.resolve([]),
    createPolicy: vi.fn(),
    getDefaultPolicy: (...a: unknown[]) => getDefaultPolicyMock(...a),
  },
}));
const navigateMock = vi.fn();
vi.mock("react-router-dom", async () => {
  const actual = await vi.importActual<typeof import("react-router-dom")>("react-router-dom");
  return { ...actual, useNavigate: () => navigateMock };
});
const previewMock = vi.fn();
vi.mock("../../../lib/api/policy-preview", () => ({ previewRunPolicy: (...a: unknown[]) => previewMock(...a) }));
const createRunMock = vi.fn();
const preflightRunMock = vi.fn();
vi.mock("../../../lib/api/runs", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../../../lib/api/runs")>();
  return {
    ...actual,
    runs: {
      createRun: (...a: unknown[]) => createRunMock(...a),
      listRuns: () => Promise.resolve([]),
      preflightRun: (...a: unknown[]) => preflightRunMock(...a),
      gradePolicy: () => Promise.resolve({ risk_assessment: [], overall_risk: "low" }),
    },
  };
});
vi.mock("../../../lib/api/workspaces", () => ({
  workspaces: { listWorkspaces: () => Promise.resolve([]) },
}));

import { ISSUE_TARGET } from "./new-run-launch-gates";
import { NewRunScreen } from "./new-run-screen";
import { defaultSpecText } from "./policy-lane";
import { OperatorProvider } from "../../wardyn/operator-context";
import { POLICY_DOCUMENT as D } from "../../wardyn/copy/policy-document";
import { NEW_RUN_POLICY_IDS } from "../../wardyn/policy-document/new-run-policy-panel-body";
import { toYaml } from "../../wardyn/yaml-block";
import { UNSAVED } from "../../../lib/unsaved-copy";
import { UnsavedGuardProvider } from "../../../lib/use-unsaved-guard";
import { setField } from "../../../../test/set-field";
import { editPolicy, goToPanel } from "../../../../test/new-run-panel";

const user = userEvent.setup({ pointerEventsCheck: 0 });

const DEFAULT_SPEC = {
  allowed_domains: ["api.default.example"],
  first_use_approval: "deny_with_review" as const,
  min_confinement_class: "CC1" as const,
};
const SPEC = /^Spec \((YAML|JSON)\)/;
// The preview is read 800 ms after the draft settles.
const WAIT = { timeout: 4000 };

const answer = (spec: unknown) => ({
  spec,
  source: { kind: "inline" },
  provisional: true,
  redacted: false,
  pending: [],
  warnings: [],
  repository_access: [],
});

// Under the shell's own unsaved guard, as the app mounts it.
function renderScreen(securityOperator = true) {
  return render(
    <MemoryRouter>
      <UnsavedGuardProvider>
        <OperatorProvider principal="test-owner" operator securityOperator={securityOperator}>
          <NewRunScreen />
        </OperatorProvider>
      </UnsavedGuardProvider>
    </MemoryRouter>,
  );
}

const panel = () => within(screen.getByTestId("nr-policy-panel-body"));
const rail = () => within(screen.getByRole("complementary"));
const launch = () => screen.getByRole("button", { name: "Launch run" });
const views = () =>
  within(screen.getByTestId("policy-view-switch"))
    .getAllByRole("button")
    .map((b) => [b.textContent, b.getAttribute("aria-pressed")]);

beforeEach(() => {
  navigateMock.mockReset();
  createRunMock.mockReset().mockResolvedValue({ id: "run_1" });
  preflightRunMock.mockReset().mockResolvedValue({ setup_items: [], enforced_confinement_class: "CC1", overall_risk: "low", warnings: [] });
  getDefaultPolicyMock.mockReset().mockResolvedValue(DEFAULT_SPEC);
  // The server's preview of the request: the inline policy it was sent, else the default.
  previewMock.mockReset().mockImplementation((input: { inline_policy?: unknown }) => Promise.resolve(answer(input.inline_policy ?? DEFAULT_SPEC)));
});

describe("New Run — the Policy panel reads first", () => {
  it("opens on the read view: display names, the three views, and no source to type in", async () => {
    renderScreen();
    goToPanel("Policy");
    expect(await panel().findByRole("heading", { level: 3, name: D.THIS_RUN })).toBeInTheDocument();
    expect(panel().getByText(D.READ_EDIT)).toBeInTheDocument();
    expect(panel().getByRole("button", { name: D.EDIT })).toBeInTheDocument();
    expect(screen.queryByLabelText(SPEC)).toBeNull();
    expect(panel().queryByText(D.EDITING)).toBeNull();
    expect(views()).toEqual([["Summary", "true"], ["YAML", "false"], ["JSON", "false"]]);

    // What is read is the server's preview of this draft, in plain names.
    expect(await panel().findByText("Allowed hosts", {}, WAIT)).toBeInTheDocument();
    expect(panel().getByText(D.PROVISIONAL)).toBeInTheDocument();
    expect(panel().queryByText(/allowed_domains|min_confinement_class/)).toBeNull();
    expect(previewMock.mock.lastCall![0]).toHaveProperty("inline_policy");
  });

  it("Edit policy opens the YAML source with focus in it; Done editing closes it and returns focus", async () => {
    renderScreen();
    goToPanel("Policy");
    await user.click(await panel().findByRole("button", { name: D.EDIT }));

    const source = screen.getByLabelText(/^Spec \(YAML\)/) as HTMLTextAreaElement;
    expect(source).toHaveFocus();
    // The draft opens as YAML, not as JSON under a YAML label.
    expect(source.value).toBe(defaultSpecText());
    expect(source.value).toMatch(/^min_confinement_class: CC1$/m);
    expect(panel().getByText(D.EDITING)).toBeInTheDocument();
    expect(panel().getByText(D.READ_UPDATES)).toBeInTheDocument();
    expect(panel().queryByRole("button", { name: D.EDIT })).toBeNull();

    await user.click(panel().getByRole("button", { name: D.DONE }));
    expect(screen.queryByLabelText(SPEC)).toBeNull();
    expect(panel().getByRole("button", { name: D.EDIT })).toHaveFocus();
  });

  it("JSON is an explicit choice, and the draft's one parse follows it", async () => {
    renderScreen();
    setField(await screen.findByLabelText("Title"), "json run");
    await editPolicy();
    await user.click(within(screen.getByTestId("policy-format-switch")).getByRole("button", { name: "JSON" }));
    await user.click(await screen.findByRole("button", { name: D.SWITCH_JSON }));

    const source = screen.getByLabelText(/^Spec \(JSON\)/) as HTMLTextAreaElement;
    expect(JSON.parse(source.value)).toMatchObject({ min_confinement_class: "CC1" });
    // YAML's flow syntax is not JSON: the gate and the editor agree it does not parse.
    setField(source, "{min_confinement_class: CC1}");
    expect(await panel().findByText(/^Invalid JSON — /)).toBeInTheDocument();
    expect(rail().getByRole("button", { name: D.INVALID_GATE })).toBeInTheDocument();
    expect(launch()).toBeDisabled();

    setField(source, '{"min_confinement_class": "CC1", "allowed_domains": ["json.example"]}');
    await user.click(launch());
    await waitFor(() => expect(createRunMock).toHaveBeenCalled());
    const body = createRunMock.mock.calls[0][0] as { inline_policy: { allowed_domains: string[] } };
    expect(body.inline_policy.allowed_domains).toContain("json.example");
  });

  it("neither the open editor nor the view chosen makes the draft dirty; a conversion does", async () => {
    renderScreen();
    await editPolicy();
    await user.click(within(screen.getByTestId("policy-view-switch")).getByRole("button", { name: "YAML" }));
    fireEvent.keyDown(window, { key: "Escape" });
    expect(navigateMock).toHaveBeenCalledWith("/runs");

    navigateMock.mockReset();
    await user.click(within(screen.getByTestId("policy-format-switch")).getByRole("button", { name: "JSON" }));
    await user.click(await screen.findByRole("button", { name: D.SWITCH_JSON }));
    await waitFor(() => expect(screen.getByLabelText(/^Spec \(JSON\)/)).toBeInTheDocument());
    fireEvent.keyDown(window, { key: "Escape" });
    expect(await screen.findByRole("alertdialog", { name: UNSAVED.TITLE })).toBeInTheDocument();
    expect(navigateMock).not.toHaveBeenCalled();
  });
});

// The screen's links name controls by id; the panel body is what renders them.
it("the issue targets on the Policy panel are the ids the panel body renders", () => {
  expect([ISSUE_TARGET.POLICY_SOURCE, ISSUE_TARGET.POLICY_MODE, ISSUE_TARGET.TOOL_RULES, ISSUE_TARGET.POLICY_READ]).toEqual([
    NEW_RUN_POLICY_IDS.SOURCE,
    NEW_RUN_POLICY_IDS.MODES,
    NEW_RUN_POLICY_IDS.TOOL_RULES,
    NEW_RUN_POLICY_IDS.READ,
  ]);
});

describe("New Run — an invalid policy source", () => {
  const BROKEN = "allowed_domains: [";

  // A titled form with an authorized preview on screen and its automatic check answered.
  async function settled() {
    renderScreen();
    setField(await screen.findByLabelText("Title"), "invalid source");
    const source = await editPolicy();
    await panel().findByText("Allowed hosts", {}, WAIT);
    await waitFor(() => expect(preflightRunMock).toHaveBeenCalled());
    return source;
  }

  it("holds the structured controls with the remedy, holds Launch on the approved sentence, and sends nothing", async () => {
    const source = await settled();
    const previews = previewMock.mock.calls.length;
    const checks = preflightRunMock.mock.calls.length;
    setField(source, BROKEN);

    expect(await panel().findByText(/^Invalid YAML — /)).toBeInTheDocument();
    expect(source).toHaveAttribute("aria-invalid", "true");
    const structured = screen.getByTestId("policy-structured");
    expect(structured).toBeDisabled();
    // The way back to the text sits outside the held group.
    const remedy = panel().getByRole("button", { name: D.EDIT });
    expect(structured).not.toContainElement(remedy);
    source.blur();
    await user.click(remedy);
    expect(source).toHaveFocus();

    expect(launch()).toBeDisabled();
    expect(panel().getByRole("button", { name: "Check again" })).toBeDisabled();
    expect(rail().getByRole("button", { name: D.INVALID_GATE })).toBeInTheDocument();
    expect(D.INVALID_GATE).toBe("The policy spec isn't valid YAML or JSON.");
    expect(within(screen.getByRole("navigation", { name: "New run" })).getByRole("button", { name: "Policy 1 issue" })).toBeInTheDocument();

    // Past the scheduler's 800 ms: no read and no check of text that does not parse.
    await new Promise((done) => setTimeout(done, 1000));
    expect(previewMock.mock.calls.length).toBe(previews);
    expect(preflightRunMock.mock.calls.length).toBe(checks);
    expect(createRunMock).not.toHaveBeenCalled();
  });

  it("keeps the last preview on screen, marked out of date, with its copy held", async () => {
    const source = await settled();
    setField(source, BROKEN);

    expect(await panel().findByText(D.INVALID_PREVIEW)).toBeInTheDocument();
    expect(panel().getByText("Allowed hosts")).toBeInTheDocument();
    expect(panel().queryByText(D.PROVISIONAL)).toBeNull();
    expect(panel().getByRole("button", { name: "Copy YAML" })).toBeDisabled();
    // The source itself, mistakes included, can always be copied.
    expect(panel().getByRole("button", { name: D.COPY_SOURCE })).toBeEnabled();
  });

  it("the line above Launch opens the editor again and focuses the source, text intact", async () => {
    const source = await settled();
    setField(source, BROKEN);
    await user.click(panel().getByRole("button", { name: D.DONE }));
    expect(screen.queryByLabelText(SPEC)).toBeNull();

    goToPanel("Run");
    await user.click(rail().getByRole("button", { name: D.INVALID_GATE }));
    expect(screen.getByRole("heading", { level: 2, name: "Policy" })).toBeVisible();
    const reopened = screen.getByLabelText(/^Spec \(YAML\)/) as HTMLTextAreaElement;
    expect(reopened).toHaveFocus();
    expect(reopened.value).toBe(BROKEN);
  });
});

describe("New Run — the policy preview being read", () => {
  it("an edit keeps the last preview, out of date, until the next answer; another policy choice never borrows it", async () => {
    renderScreen();
    const source = await editPolicy();
    await panel().findByText("Allowed hosts", {}, WAIT);

    let release: ((value: unknown) => void) | undefined;
    previewMock.mockImplementationOnce(() => new Promise((resolve) => { release = resolve; }));
    setField(source, "min_confinement_class: CC1\nallowed_domains:\n  - second.example\n");
    expect(await panel().findByText(D.STALE_PREVIEW)).toBeInTheDocument();
    expect(panel().getByText("Allowed hosts")).toBeInTheDocument();
    expect(panel().queryByText("second.example")).toBeNull();
    expect(panel().getByRole("button", { name: "Copy YAML" })).toBeDisabled();

    await waitFor(() => expect(release).toBeDefined(), WAIT);
    release!(answer({ min_confinement_class: "CC1", allowed_domains: ["second.example"] }));
    expect(await panel().findByText("second.example")).toBeInTheDocument();
    expect(panel().queryByText(D.STALE_PREVIEW)).toBeNull();
    expect(panel().getByText(D.PROVISIONAL)).toBeInTheDocument();

    previewMock.mockImplementation(() => new Promise(() => {}));
    await user.click(panel().getByRole("button", { name: /^Use the default policy/ }));
    expect(panel().queryByText("second.example")).toBeNull();
    expect(panel().queryByText(D.STALE_PREVIEW)).toBeNull();
  });

  it("Check again reads the preview again along with the check", async () => {
    renderScreen();
    goToPanel("Policy");
    await panel().findByText("Allowed hosts", {}, WAIT);
    const previews = previewMock.mock.calls.length;
    const checks = preflightRunMock.mock.calls.length;

    await user.click(panel().getByRole("button", { name: "Check again" }));
    await waitFor(() => expect(previewMock.mock.calls.length).toBe(previews + 1));
    expect(preflightRunMock.mock.calls.length).toBe(checks + 1);
    expect(previewMock.mock.lastCall![0]).toEqual(preflightRunMock.mock.lastCall![0]);
  });
});

describe("New Run — a saved or default policy is read-only", () => {
  const chooseDefault = async () => user.click(await panel().findByRole("button", { name: /^Use the default policy/ }));

  it("the Tool rules link lands on the read-only policy document, on its Summary", async () => {
    renderScreen();
    goToPanel("Policy");
    await user.click(within(await screen.findByTestId("policy-view-switch")).getByRole("button", { name: "YAML" }));
    await chooseDefault();
    goToPanel("Run");
    await user.click(screen.getByRole("radio", { name: /^Autonomous/ }));
    await user.click(screen.getByRole("radio", { name: /^Hold in Wardyn/ }));
    await user.click(screen.getByRole("button", { name: "Tool rules" }));

    expect(screen.getByRole("heading", { level: 2, name: "Policy" })).toBeVisible();
    expect(screen.getByRole("heading", { level: 3, name: D.THIS_RUN })).toHaveFocus();
    expect(views()).toEqual([["Summary", "true"], ["YAML", "false"], ["JSON", "false"]]);
    expect(panel().getByText(D.READ_CUSTOMIZE)).toBeInTheDocument();
    expect(screen.queryByLabelText(SPEC)).toBeNull();
    expect(panel().queryByRole("button", { name: D.EDIT })).toBeNull();
  });

  it("Customize for this run copies a readable default as YAML and opens the editor on it", async () => {
    renderScreen();
    goToPanel("Policy");
    await chooseDefault();
    await panel().findByText("Allowed hosts", {}, WAIT);
    await user.click(panel().getByRole("button", { name: D.CUSTOMIZE }));

    const source = screen.getByLabelText(/^Spec \(YAML\)/) as HTMLTextAreaElement;
    expect(source).toHaveFocus();
    expect(source.value).toBe(`${toYaml(DEFAULT_SPEC)}\n`);
    expect(panel().getByRole("button", { name: /^Custom policy/ })).toHaveAttribute("aria-pressed", "true");
    expect(panel().queryByText(D.SAFE_CUSTOM)).toBeNull();
  });

  it("a read that may have values removed is never copied: customizing starts from the safe policy and says why", async () => {
    renderScreen(false);
    goToPanel("Policy");
    await chooseDefault();
    await panel().findByText("Allowed hosts", {}, WAIT);
    await user.click(panel().getByRole("button", { name: D.CUSTOMIZE }));

    const source = screen.getByLabelText(/^Spec \(YAML\)/) as HTMLTextAreaElement;
    expect(source.value).toBe(defaultSpecText());
    expect(source.value).not.toContain("api.default.example");
    expect(panel().getByText(D.SAFE_CUSTOM)).toBeInTheDocument();
  });
});
