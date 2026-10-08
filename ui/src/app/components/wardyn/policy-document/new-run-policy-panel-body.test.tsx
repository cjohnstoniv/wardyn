/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

// The structured sections render the SafetyMeter-free editor here, but the
// templates import the panel module, which reaches the grade call.
vi.mock("../../../lib/api/runs", () => ({ runs: { gradePolicy: vi.fn().mockResolvedValue(null) } }));

import { setField } from "../../../../test/set-field";
import type { RunPolicySpec } from "../../../lib/types";
import type { PolicyPreviewResult } from "../../../lib/types/policy-preview";
import { toYaml } from "../code-block";
import type { PolicyMode } from "../policy-panel";
import {
  NewRunPolicyPanelBody,
  sameSpec,
  type NewRunPolicyPanelBodyProps,
  type NewRunPolicyPreview,
} from "./new-run-policy-panel-body";
import type { PolicyView } from "./policy-document";
import { parseSpec, type PolicySourceFormat } from "./policy-source";

const STARTER = "# Safe starting policy\nmin_confinement_class: CC2\nallowed_domains: []\nfirst_use_approval: deny_with_review\n";
const CUSTOM = "# mine\nmin_confinement_class: CC2\nallowed_domains:\n  - api.example.com # model host\n";
const SAVED = {
  min_confinement_class: "CC2",
  allowed_domains: ["api.example.com", "registry.example.org"],
  first_use_approval: "deny_with_review",
} as unknown as RunPolicySpec;
const REDACTED_DEFAULT = {
  min_confinement_class: "CC2",
  allowed_domains: ["api.example.com"],
  workspace_mounts: [{ source: "<redacted>", target: "/mnt/shared", read_only: true }],
  eligible_grants: [{ kind: "env_secret", scope: { secret_name: "<redacted>" }, requires_approval: false }],
} as unknown as RunPolicySpec;

const result = (spec: RunPolicySpec, over: Partial<PolicyPreviewResult> = {}): PolicyPreviewResult => ({
  spec,
  source: { kind: "inline" },
  provisional: true,
  redacted: false,
  warnings: [],
  pending: ["task", "credential_liveness"],
  repository_access: [],
  ...over,
});
const preview = (over: Partial<NewRunPolicyPreview> = {}): NewRunPolicyPreview => ({ result: null, busy: false, fresh: false, ...over });

// What the screen would send, built the way its one builder would: from what
// the source parses to now, and nothing when it does not parse.
const requestValue = (source: string, format: PolicySourceFormat) => {
  const parsed = parseSpec(source, format);
  return parsed.ok ? JSON.stringify(parsed.spec) : null;
};

interface Draft {
  mode: PolicyMode;
  source: string;
  format: PolicySourceFormat;
  editing: boolean;
  view: PolicyView;
  safe: boolean;
}
let draft: Draft;
const customized = vi.fn();

// The screen: it owns the draft and gives the body nothing else to write to.
function Screen({
  initial,
  ...rest
}: { initial?: Partial<Draft> } & Partial<NewRunPolicyPanelBodyProps>) {
  const [d, setD] = React.useState<Draft>({
    mode: "custom",
    source: STARTER,
    format: "yaml",
    editing: false,
    view: "summary",
    safe: false,
    ...initial,
  });
  draft = d;
  const set = (patch: Partial<Draft>) => setD((cur) => ({ ...cur, ...patch }));
  return (
    <NewRunPolicyPanelBody
      barrier={<div>barrier picker</div>}
      mode={d.mode}
      onModeChange={(mode) => set({ mode })}
      defaultStatus="ready"
      onRetryDefault={() => {}}
      savedPicker={<div>saved picker</div>}
      source={d.source}
      format={d.format}
      onSourceChange={(source) => set({ source })}
      onFormatChange={(format, source) => set({ format, source })}
      editing={d.editing}
      onEditingChange={(editing) => set({ editing })}
      view={d.view}
      onViewChange={(view) => set({ view })}
      preview={preview()}
      sourceSpec={null}
      sourceRedacted={false}
      safeStarter={STARTER}
      customUntouched={d.source === STARTER}
      safeCustom={d.safe}
      onCustomize={(seed) => {
        customized(seed);
        set({ mode: "custom", source: seed.source, format: seed.format, editing: true, view: "yaml", safe: seed.safe });
      }}
      {...rest}
    />
  );
}

const button = (name: string | RegExp) => screen.getByRole("button", { name });
const box = () => screen.getByRole("textbox") as HTMLTextAreaElement;

beforeEach(() => {
  customized.mockReset();
});

describe("NewRunPolicyPanelBody — reading is the default", () => {
  it("opens on the read view: barrier, three modes, This run's policy, and no editor", () => {
    render(<Screen preview={preview({ result: result(SAVED), fresh: true })} />);
    expect(screen.getByText("barrier picker")).toBeInTheDocument();
    expect(button(/^Use the default policy/)).toHaveAttribute("aria-pressed", "false");
    expect(button(/^Reuse a saved policy/)).toHaveAttribute("aria-pressed", "false");
    expect(button(/^Custom policy/)).toHaveAttribute("aria-pressed", "true");
    expect(screen.getByRole("heading", { name: "This run's policy" })).toBeInTheDocument();
    expect(screen.getByText("Read-only. Choose Edit policy to change it.")).toBeInTheDocument();
    expect(button("Summary")).toHaveAttribute("aria-pressed", "true");
    expect(screen.getByRole("heading", { name: "Network" })).toBeInTheDocument();
    expect(screen.queryByRole("textbox")).toBeNull();
    expect(screen.queryByText("Editing")).toBeNull();
  });

  it("Edit policy opens the YAML editor with focus in the source; Done editing goes back to the button", async () => {
    const user = userEvent.setup();
    render(<Screen />);
    await user.click(button("Edit policy"));
    expect(screen.getByText("Editing")).toBeInTheDocument();
    expect(screen.getByLabelText(/^Spec \(YAML\)/)).toBe(box());
    await waitFor(() => expect(box()).toHaveFocus());
    expect(screen.getByText("Read-only. It updates as you edit the policy below.")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Edit policy" })).toBeNull();

    await user.click(button("Done editing"));
    expect(screen.queryByRole("textbox")).toBeNull();
    await waitFor(() => expect(button("Edit policy")).toHaveFocus());
    // Leaving the editor is presentation only.
    expect(draft.source).toBe(STARTER);
  });

  it("offers the templates and the tool and push rule sections inside the editor", async () => {
    const user = userEvent.setup();
    render(<Screen initial={{ editing: true }} />);
    expect(screen.getByText("Start from a template")).toBeInTheDocument();
    await user.click(button("CI baseline"));
    expect(parseSpec(draft.source).ok && parseSpec(draft.source)).toMatchObject({ spec: { min_confinement_class: "CC1" } });
    expect(draft.format).toBe("yaml");
    expect(button("Add rule")).toBeInTheDocument();
  });

  it("a structured edit keeps the comments the person wrote", async () => {
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    render(<Screen initial={{ editing: true, source: CUSTOM }} />);
    await user.click(button("Add rule"));
    setField(screen.getByLabelText("Tool 1"), "Bash");
    await waitFor(() => expect(draft.source).toContain("tool_rules"));
    expect(draft.source).toContain("# mine");
    expect(draft.source).toContain("- api.example.com # model host");
  });
});

describe("NewRunPolicyPanelBody — view switches change nothing", () => {
  it("Summary → YAML → JSON → Summary leaves source, format and the request value identical", async () => {
    const user = userEvent.setup();
    render(<Screen initial={{ source: CUSTOM }} preview={preview({ result: result(SAVED), fresh: true })} />);
    const before = { source: draft.source, format: draft.format, request: requestValue(draft.source, draft.format) };
    for (const name of ["YAML", "JSON", "Summary"]) await user.click(button(name));
    expect({ source: draft.source, format: draft.format, request: requestValue(draft.source, draft.format) }).toEqual(before);
    expect(draft.source).toBe(CUSTOM);
    expect(customized).not.toHaveBeenCalled();
  });

  it("the read view is the server's preview, never the typed text", async () => {
    const user = userEvent.setup();
    render(<Screen initial={{ source: CUSTOM }} preview={preview({ result: result(SAVED), fresh: true })} />);
    await user.click(button("YAML"));
    const text = Array.from(document.querySelectorAll("pre code > div"), (d) => d.textContent).join("\n");
    expect(text).toBe(toYaml(SAVED));
    expect(text).not.toContain("# mine");
  });
});

describe("NewRunPolicyPanelBody — saved and default are read-only", () => {
  it("shows Customize for this run and no editor, whatever the editing flag was", () => {
    render(<Screen initial={{ mode: "saved", editing: true }} sourceSpec={SAVED} preview={preview({ result: result(SAVED), fresh: true })} />);
    expect(screen.getByText("saved picker")).toBeInTheDocument();
    expect(screen.getByText("Read-only. Choose Customize for this run to change it.")).toBeInTheDocument();
    expect(button("Customize for this run")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Edit policy" })).toBeNull();
    expect(screen.queryByRole("textbox")).toBeNull();
  });

  it("the default mode keeps its loading, unavailable and loaded states", () => {
    const retry = vi.fn();
    const { rerender } = render(<Screen initial={{ mode: "default" }} defaultStatus="error" onRetryDefault={retry} />);
    expect(screen.getByText("Default policy, read-only")).toBeInTheDocument();
    expect(screen.getByText("Couldn't load the default policy to show here. The run still launches under it.")).toBeInTheDocument();
    fireEvent.click(button("Retry"));
    expect(retry).toHaveBeenCalledTimes(1);
    rerender(<Screen initial={{ mode: "default" }} defaultStatus="ready" defaultProfileName="Platform team" />);
    expect(
      screen.getByText("This run launches under this policy as it stands. Your attached workspace mounts into it; nothing else on this page is merged."),
    ).toBeInTheDocument();
    expect(button(/^Use the default policy/)).toHaveTextContent("Launch under the policy set by your profile, Platform team.");
  });

  it("customizing a readable source seeds it as YAML and opens the editor on it", async () => {
    const user = userEvent.setup();
    render(<Screen initial={{ mode: "saved" }} sourceSpec={SAVED} />);
    await user.click(button("Customize for this run"));
    expect(customized).toHaveBeenCalledWith({ source: `${toYaml(SAVED)}\n`, format: "yaml", safe: false });
    expect(draft.mode).toBe("custom");
    await waitFor(() => expect(box()).toHaveFocus());
    expect(box().value).toBe(`${toYaml(SAVED)}\n`);
    expect(screen.queryByText("Some source settings are hidden. Customization starts from a safe policy.")).toBeNull();
  });

  it("customizing a redacted source starts from the safe policy and says why; no hidden marker becomes editable", async () => {
    const user = userEvent.setup();
    render(<Screen initial={{ mode: "default" }} sourceSpec={REDACTED_DEFAULT} sourceRedacted />);
    await user.click(button("Customize for this run"));
    expect(customized).toHaveBeenCalledWith({ source: STARTER, format: "yaml", safe: true });
    expect(screen.getByText("Some source settings are hidden. Customization starts from a safe policy.")).toBeInTheDocument();
    expect(box().value).toBe(STARTER);
    expect(draft.source).not.toContain("<redacted>");
    expect(requestValue(draft.source, draft.format)).not.toContain("redacted");
    expect(requestValue(draft.source, draft.format)).not.toContain("/mnt/shared");
  });

  it("a source that cannot be read at all also starts from the safe policy", async () => {
    render(<Screen initial={{ mode: "default" }} sourceSpec={null} />);
    await userEvent.click(button("Customize for this run"));
    expect(customized).toHaveBeenCalledWith({ source: STARTER, format: "yaml", safe: true });
  });

  it("switching modes keeps an existing custom draft, invalid text and comments included", async () => {
    const user = userEvent.setup();
    const typed = "# mine\na: [";
    render(<Screen initial={{ source: typed, editing: true }} />);
    await user.click(button(/^Reuse a saved policy/));
    await user.click(button(/^Use the default policy/));
    await user.click(button(/^Custom policy/));
    expect(draft.source).toBe(typed);
    expect(box().value).toBe(typed);
    expect(customized).not.toHaveBeenCalled();
  });

  it("replacing an existing custom draft asks first; Keep custom policy keeps it byte for byte", async () => {
    const user = userEvent.setup();
    render(<Screen initial={{ mode: "saved", source: CUSTOM }} sourceSpec={SAVED} />);
    await user.click(button("Customize for this run"));
    const dialog = await screen.findByRole("alertdialog", { name: "Replace your custom policy?" });
    expect(dialog).toHaveTextContent("Your existing custom policy will be replaced by this source.");
    await user.click(within(dialog).getByRole("button", { name: "Keep custom policy" }));
    await waitFor(() => expect(screen.queryByRole("alertdialog")).toBeNull());
    expect(customized).not.toHaveBeenCalled();
    expect(draft).toMatchObject({ mode: "saved", source: CUSTOM });

    await user.click(button("Customize for this run"));
    await user.click(within(await screen.findByRole("alertdialog")).getByRole("button", { name: "Replace custom policy" }));
    await waitFor(() => expect(draft.mode).toBe("custom"));
    expect(draft.source).toBe(`${toYaml(SAVED)}\n`);
    await waitFor(() => expect(box()).toHaveFocus());
  });
});

describe("NewRunPolicyPanelBody — invalid source", () => {
  it("`a: [` survives leaving and returning to the panel, with its location and a way back to the text", async () => {
    function Panels() {
      const [panel, setPanel] = React.useState<"policy" | "run">("policy");
      // The draft lives above the panel, as it does on the screen.
      const [source, setSource] = React.useState("a: [");
      return (
        <>
          <button type="button" onClick={() => setPanel(panel === "policy" ? "run" : "policy")}>
            switch panel
          </button>
          {panel === "policy" && (
            <Screen initial={{ editing: true }} source={source} onSourceChange={setSource} />
          )}
        </>
      );
    }
    const user = userEvent.setup();
    render(<Panels />);
    const position = screen.getByText(/^Line \d+, column \d+$/).textContent;
    await user.click(button("switch panel"));
    expect(screen.queryByRole("textbox")).toBeNull();
    await user.click(button("switch panel"));
    expect(box().value).toBe("a: [");
    expect(screen.getByText(/^Invalid YAML — /)).toBeInTheDocument();
    expect(screen.getByText(/^Line \d+, column \d+$/)).toHaveTextContent(position!);
    expect(box()).toHaveAttribute("aria-invalid", "true");
    await user.click(button("Edit policy"));
    expect(box()).toHaveFocus();
    expect(requestValue("a: [", "yaml")).toBeNull();
  });

  it("holds the structured controls and Check again, and shows an earlier preview as out of date", () => {
    const onCheck = vi.fn();
    render(
      <Screen
        initial={{ source: "a: [", editing: true }}
        preview={preview({ result: result(SAVED), fresh: true })}
        onCheck={onCheck}
      />,
    );
    expect(screen.getByText("This preview is out of date. Fix the policy source to refresh it.")).toBeInTheDocument();
    expect(screen.queryByText("This preview includes your selections. Launch checks may change it.")).toBeNull();
    expect(button("Copy YAML")).toBeDisabled();
    expect(button("Check again")).toBeDisabled();
    expect(button("CI baseline")).toBeDisabled();
    expect(button("Copy source")).toBeEnabled();
  });

  it("with no earlier preview, an invalid source shows nothing invented", () => {
    render(<Screen initial={{ source: "a: [", editing: true }} />);
    expect(screen.queryByRole("heading", { name: "Network" })).toBeNull();
    expect(screen.queryByText(/This preview/)).toBeNull();
  });
});

describe("NewRunPolicyPanelBody — preview states", () => {
  it("a fresh preview is provisional, lists its launch-only checks, and shows what this draft requests", () => {
    render(<Screen preview={preview({ result: result(SAVED), fresh: true })} requestedClass="CC3" />);
    expect(screen.getByText("This preview includes your selections. Launch checks may change it.")).toBeInTheDocument();
    const checks = screen.getByRole("heading", { name: "Checked at launch" }).parentElement!;
    expect(within(checks).getAllByRole("listitem").map((li) => li.textContent)).toEqual(["Task", "Credentials still valid"]);
    expect(screen.getByText("This run requests").nextElementSibling).toHaveTextContent("Vault");
    expect(screen.queryByText("This run used")).toBeNull();
    expect(button("Copy YAML")).toBeEnabled();
  });

  it("an aged preview is marked out of date and its copy is held", () => {
    render(<Screen preview={preview({ result: result(SAVED), fresh: false })} />);
    expect(screen.getByText("This preview is out of date. Check again to refresh it.")).toBeInTheDocument();
    expect(button("Copy YAML")).toBeDisabled();
    expect(screen.getByRole("heading", { name: "Network" })).toBeInTheDocument();
  });

  it("a rate-limited read says when to try again and keeps the last preview, stale", () => {
    render(<Screen preview={preview({ result: result(SAVED), fresh: false, rateLimitSeconds: 5 })} />);
    expect(screen.getByText("Preview limit reached. Try again in 5s.")).toBeInTheDocument();
    expect(screen.getByText("This preview is out of date. Check again to refresh it.")).toBeInTheDocument();
    expect(button("Copy YAML")).toBeDisabled();
  });

  it("a refused read shows the server's sentence and no policy", () => {
    render(<Screen preview={preview({ result: result(SAVED), error: "policy preview refused: not readable by you" })} />);
    expect(screen.getByText("policy preview refused: not readable by you")).toBeInTheDocument();
    expect(screen.queryByRole("heading", { name: "Network" })).toBeNull();
  });

  it("the first read in flight says so", () => {
    render(<Screen preview={preview({ busy: true })} />);
    expect(screen.getByText("Checking this run…")).toBeInTheDocument();
  });

  it("a refused draft is not previewed, and its sentence sits beside the modes", () => {
    const problem = "The default policy launches with one workspace. Remove the extra workspace, or choose Custom policy to keep them all.";
    render(<Screen initial={{ mode: "default" }} modeProblem={problem} preview={preview({ result: result(SAVED), fresh: true })} />);
    expect(screen.getByText(problem)).toBeInTheDocument();
    expect(screen.queryByRole("heading", { name: "Network" })).toBeNull();
  });

  it("a member's preview with hidden values says so above the raw views", async () => {
    render(<Screen initial={{ mode: "default" }} preview={preview({ result: result(REDACTED_DEFAULT, { redacted: true }), fresh: true })} />);
    expect(screen.getAllByRole("note", { name: "Hidden. Only admins can see this." }).length).toBeGreaterThan(0);
    await userEvent.click(button("YAML"));
    expect(
      screen.getByText("Values shown as <redacted> are hidden from you. Fill them in before using this as a policy."),
    ).toBeInTheDocument();
  });

  it("says so, once, when the read view holds more than the person typed", () => {
    const sentence = "It also includes what this run's selections add and what your limits change.";
    const { unmount } = render(<Screen initial={{ source: CUSTOM }} preview={preview({ result: result(SAVED), fresh: true })} />);
    expect(screen.getAllByText(sentence)).toHaveLength(1);
    unmount();
    // The server spelling out unset fields is not a difference.
    const same = { ...parsedCustom(), denied_domains: [], allow_all_egress: false, eligible_grants: null } as unknown as RunPolicySpec;
    render(<Screen initial={{ source: CUSTOM }} preview={preview({ result: result(same), fresh: true })} />);
    expect(screen.queryByText(sentence)).toBeNull();
  });

  it("Check again is the screen's call, with its hint", async () => {
    const onCheck = vi.fn();
    render(<Screen onCheck={onCheck} />);
    expect(screen.getByText("Checked as you edit. A refusal holds Launch for up to a minute.")).toBeInTheDocument();
    await userEvent.click(button("Check again"));
    expect(onCheck).toHaveBeenCalledTimes(1);
  });
});

function parsedCustom(): RunPolicySpec {
  const parsed = parseSpec(CUSTOM);
  if (!parsed.ok) throw new Error(parsed.message);
  return parsed.spec;
}

describe("sameSpec", () => {
  it("ignores key order and values that mean not set", () => {
    expect(sameSpec({ a: 1, b: [] }, { b: null, a: 1, c: false, d: {} })).toBe(true);
    expect(sameSpec({ a: ["x"] }, { a: ["x", "y"] })).toBe(false);
  });
});
