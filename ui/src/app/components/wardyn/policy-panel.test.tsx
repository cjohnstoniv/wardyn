/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

// The panel now renders the SafetyMeter, which debounces a POST /policies/grade.
// Stub it so no test touches the network — and so a stray post-unmount call from
// any panel-render test resolves a promise instead of `undefined.then`-crashing.
const gradePolicyMock = vi.fn();
vi.mock("../../lib/api/runs", () => ({
  runs: { gradePolicy: (...a: unknown[]) => gradePolicyMock(...a) },
}));

import {
  FIELD_HELP,
  POLICY_TEMPLATES,
  PolicyPanel,
  templateText,
  toolRulesSummary,
  type PolicyPanelInstance,
  type PolicyPanelProps,
} from "./policy-panel";
import type { PolicyGrade, RunPolicySpec } from "../../lib/types";

// A real-shaped medium grade (composer.Grade of a CC2 spec) — the default the
// meter resolves unless a test overrides it.
const GUARDED_GRADE: PolicyGrade = {
  overall_risk: "medium",
  risk_assessment: [
    {
      field: "min_confinement_class",
      value: "CC2",
      risk_level: "medium",
      rationale: "Wall (the default tier — a gVisor sandbox).",
      invariant_ref: "5",
    },
  ],
};

beforeEach(() => {
  gradePolicyMock.mockReset();
  gradePolicyMock.mockResolvedValue(GUARDED_GRADE);
});

// The panel is fully controlled, so drive it through a tiny stateful harness —
// otherwise a chip click only ever proves the callback fired, never that the
// textarea (and every derivation reading it) actually changed.
function Harness({
  initial = "",
  instance = "policies",
  ...rest
}: { initial?: string; instance?: PolicyPanelInstance } & Omit<
  PolicyPanelProps,
  "instance" | "value" | "onChange"
>) {
  const [text, setText] = React.useState(initial);
  return <PolicyPanel instance={instance} value={text} onChange={setText} {...rest} />;
}

function specBox(): HTMLTextAreaElement {
  return screen.getByLabelText("Spec (JSON)") as HTMLTextAreaElement;
}

const VALID = JSON.stringify({ allowed_domains: ["api.anthropic.com"], min_confinement_class: "CC2" }, null, 2);

describe("PolicyPanel — templates", () => {
  it("every template is parseable JSON that round-trips its const", () => {
    for (const t of POLICY_TEMPLATES) {
      expect(JSON.parse(templateText(t))).toEqual(t.spec);
    }
  });

  it("every template carries auto_stop_after_sec: 3600 EXCEPT allow-all (a load-bearing omission)", () => {
    for (const t of POLICY_TEMPLATES) {
      const parsed = JSON.parse(templateText(t)) as RunPolicySpec;
      if (t.id === "allow-all") {
        // DELIBERATE and LOAD-BEARING: allow-all's two highs (allow-all egress +
        // the omitted idle cap = never-reap) are what make the safety meter's
        // "Weakest" reachable from a single template click. A 3600 here would drop
        // it to ONE high and collapse Weakest — so absent is asserted, not 3600.
        expect(Object.keys(parsed)).not.toContain("auto_stop_after_sec");
      } else {
        // ci/model-provider inherit 3600 from their source examples; minimal and
        // registries gain it so the conservative (non-interactive) meter frame
        // does not grade the shipped starter "Elevated" purely for an omitted cap.
        // NOTE: minimal is BOTH policies.tsx's STARTER_SPEC and the run wizard's
        // fresh Custom-policy prefill (new-run-screen spreads MINIMAL.spec) — both
        // now idle-stop after an hour. Intended; TouchDebounce keeps live sessions
        // alive, so an attended interactive run is never reaped out from under you.
        expect(parsed.auto_stop_after_sec).toBe(3600);
      }
    }
  });

  it("the allow-all template is the honest allow-all shape", () => {
    const t = POLICY_TEMPLATES.find((x) => x.id === "allow-all")!;
    expect(t.spec.allow_all_egress).toBe(true);
    // first_use_approval is INERT under allow-all: say always_deny rather than
    // imply a review that never fires.
    expect(t.spec.first_use_approval).toBe("always_deny");
    expect(t.spec.allowed_domains).toEqual([]);
  });

  it("clicking a chip replaces the textarea body with that template", async () => {
    const user = userEvent.setup();
    render(<Harness initial="{}" />);
    const t = POLICY_TEMPLATES.find((x) => x.id === "registries")!;

    await user.click(screen.getByRole("button", { name: t.label }));

    expect(specBox().value).toBe(templateText(t));
    expect(JSON.parse(specBox().value)).toEqual(t.spec);
  });
});

describe("PolicyPanel — helper rail", () => {
  it("/policies documents every RunPolicySpec key", () => {
    render(<Harness initial={VALID} />);
    for (const key of Object.keys(FIELD_HELP)) {
      expect(screen.getByRole("button", { name: `Insert ${key}` })).toBeInTheDocument();
    }
  });

  it("the run instance drops workspace_mounts (the Workspace card owns mounts there)", () => {
    render(<Harness instance="run" initial={VALID} />);
    const keys = Object.keys(FIELD_HELP);
    expect(screen.queryByRole("button", { name: "Insert workspace_mounts" })).toBeNull();
    expect(screen.queryByText("workspace_mounts")).toBeNull();
    for (const key of keys.filter((k) => k !== "workspace_mounts")) {
      expect(screen.getByRole("button", { name: `Insert ${key}` })).toBeInTheDocument();
    }
    // The help entry still EXISTS — the parity guard keeps the full key set;
    // only the render is gated.
    expect(FIELD_HELP.workspace_mounts).toBeTruthy();
  });

  it("no snippet anywhere authors workspace_secret_values (refused on write)", () => {
    const json = JSON.stringify(Object.values(FIELD_HELP).map((h) => h.snippet));
    expect(json).not.toContain("workspace_secret_values");
    expect(FIELD_HELP.llm_inspection.values).toMatch(/workspace_secret_values is REFUSED/);
  });

  // Was network-dialog.test.tsx's "each card states what actually happens, not
  // the mode name". The dialog and the run wizard's Confined card both died with
  // the custom form; this rail is the one surviving surface that describes the
  // three modes, so the canon strings (ui-batch2-mock.md D33 + D8) pin here.
  it("first_use_approval states what each mode does, and bounds the hold (D33 + D8)", () => {
    const v = FIELD_HELP.first_use_approval.values;
    expect(v).toContain(
      "Default-deny. A new host is refused and raised for your review — approve it once and a retry gets through.",
    );
    expect(v).toContain(
      "The connection waits, live, for the standard 30-second window. Decide in time and it goes through; miss it and it's refused — the approval itself stays open for you to decide.",
    );
    expect(v).toContain("no prompt, no wait");
    // The bound is the point: an open-ended promise is what D8 removed.
    expect(v).not.toMatch(/until you approve or deny it/);
  });

  it("auto_stop_after_sec documents -1 as explicit never-reap intent, same as absent", () => {
    expect(FIELD_HELP.auto_stop_after_sec.values).toMatch(/ABSENT = never reaped/);
    expect(FIELD_HELP.auto_stop_after_sec.values).toMatch(/-1 = never reaped, stated explicitly/);
  });

  it("insert merges the snippet into the parsed spec, keeping what was there", async () => {
    const user = userEvent.setup();
    render(<Harness initial={VALID} />);

    await user.click(screen.getByRole("button", { name: "Insert resources" }));

    const merged = JSON.parse(specBox().value) as RunPolicySpec;
    expect(merged.allowed_domains).toEqual(["api.anthropic.com"]);
    expect(merged.min_confinement_class).toBe("CC2");
    expect(merged.resources).toEqual(FIELD_HELP.resources.snippet);
  });

  it("insert is disabled while the JSON does not parse", async () => {
    render(<Harness initial="{ not json" />);
    for (const key of Object.keys(FIELD_HELP)) {
      expect(screen.getByRole("button", { name: `Insert ${key}` })).toBeDisabled();
    }
  });
});

describe("PolicyPanel — live derivations", () => {
  it("reports a parse failure instead of deriving from nothing", () => {
    render(<Harness initial="{ not json" />);
    const status = screen.getByRole("status");
    expect(within(status).getByText(/invalid json/i)).toBeInTheDocument();
    expect(within(status).queryByText("Valid JSON")).toBeNull();
  });

  it("a top-level array is a parse failure too (a spec is an object)", () => {
    render(<Harness initial="[]" />);
    expect(screen.getByText(/must be a JSON object/i)).toBeInTheDocument();
  });

  it("derives egress, lifecycle and the barrier floor from a valid spec", async () => {
    const user = userEvent.setup();
    render(<Harness initial="{}" />);
    await user.click(screen.getByRole("button", { name: "CI baseline" }));

    const status = screen.getByRole("status");
    expect(within(status).getByText("Valid JSON")).toBeInTheDocument();
    expect(within(status).getByText("No egress")).toBeInTheDocument();
    expect(within(status).getByText("Auto-stop: 60 min idle")).toBeInTheDocument();
    // min_confinement_class CC1 renders as the friendly barrier label.
    expect(within(status).getByText("Fence")).toBeInTheDocument();
  });

  it("allow-all egress always reads as block-list only, never 'unrestricted'", async () => {
    const user = userEvent.setup();
    render(<Harness initial="{}" />);
    await user.click(screen.getByRole("button", { name: "Allow-all — observe first" }));

    expect(screen.getByText("Allow-all egress (block-list only)")).toBeInTheDocument();
    expect(screen.getByText("Runs until stopped")).toBeInTheDocument();
  });
});

describe("PolicyPanel — run instance extras", () => {
  it("renders Preflight only when the screen passes a handler", async () => {
    const user = userEvent.setup();
    const onPreflight = vi.fn();
    const { unmount } = render(<Harness instance="run" initial={VALID} />);
    expect(screen.queryByRole("button", { name: /preflight/i })).toBeNull();
    unmount();

    render(<Harness instance="run" initial={VALID} onPreflight={onPreflight} />);
    await user.click(screen.getByRole("button", { name: /preflight/i }));
    expect(onPreflight).toHaveBeenCalledTimes(1);
  });

  it("the saved-policy mode row swaps the editor for the screen's own picker", async () => {
    const user = userEvent.setup();
    const onActiveChange = vi.fn();
    render(
      <Harness
        instance="run"
        initial={VALID}
        savedPolicy={{
          active: true,
          onActiveChange,
          picker: <div>policy picker</div>,
        }}
      />,
    );
    expect(screen.getByText("policy picker")).toBeInTheDocument();
    expect(screen.queryByLabelText("Spec (JSON)")).toBeNull();

    await user.click(screen.getByRole("button", { name: /custom policy/i }));
    expect(onActiveChange).toHaveBeenCalledWith(false);
  });
});

describe("PolicyPanel — safety meter", () => {
  it("renders the meter and grades the DOCUMENT on the /policies instance (no interactive hint)", async () => {
    render(<Harness initial={VALID} />);
    expect(screen.getByTestId("safety-meter")).toBeInTheDocument();
    await waitFor(() => expect(gradePolicyMock).toHaveBeenCalled(), { timeout: 2000 });
    // /policies sends nothing for the hint — the conservative default-false frame.
    expect(gradePolicyMock).toHaveBeenLastCalledWith(expect.any(Object), undefined);
    await waitFor(() => expect(screen.getByTestId("safety-meter")).toHaveTextContent("Guarded"));
    // Its title does NOT carry the run instance's Preflight contrast.
    expect(screen.getByTestId("safety-meter").getAttribute("title")).not.toMatch(
      /Preflight grades the resolved run/,
    );
  });

  it("the run instance grades the document too, and its title names the doc-vs-run difference", async () => {
    render(<Harness instance="run" initial={VALID} />);
    const meter = screen.getByTestId("safety-meter");
    expect(meter.getAttribute("title")).toMatch(/policy document as written/);
    expect(meter.getAttribute("title")).toMatch(/Preflight grades the resolved run/);
    await waitFor(() => expect(gradePolicyMock).toHaveBeenCalled(), { timeout: 2000 });
  });

  it("a parse failure dims the meter and skips the grade call entirely", () => {
    render(<Harness initial="{ not json" />);
    const meter = screen.getByTestId("safety-meter");
    expect(meter).toHaveAttribute("data-safety", "");
    expect(meter).toHaveTextContent(/fix the json first/i);
    expect(gradePolicyMock).not.toHaveBeenCalled();
  });
});

// tool_rules shipped in 0.7 with ZERO console surface: enforced by the proxy,
// invisible in the UI. The section is the first rung of posture-gated autonomy,
// so what it writes has to be exactly what the wire means — an implicit `hold`
// default, an absent key for "no rules", and a duplicate refused before Save.
describe("PolicyPanel — the tool_rules section", () => {
  const user = userEvent.setup({ pointerEventsCheck: 0 });

  function currentSpec(): RunPolicySpec {
    return JSON.parse(specBox().value) as RunPolicySpec;
  }

  it("adds a rule and persists it into the spec document", async () => {
    render(<Harness initial={VALID} />);
    await user.click(screen.getByRole("button", { name: "Add rule" }));
    await user.type(screen.getByLabelText("Tool 1"), "Bash");
    await user.selectOptions(screen.getByLabelText("Effect 1"), "deny");

    expect(currentSpec().tool_rules).toEqual([{ tool: "Bash", effect: "deny" }]);
  });

  it("removes a rule and drops the key entirely when the last one goes", async () => {
    render(
      <Harness
        initial={JSON.stringify({ ...JSON.parse(VALID), tool_rules: [{ tool: "Bash", effect: "hold" }] })}
      />,
    );
    await user.click(screen.getByRole("button", { name: "Remove rule" }));

    // Absent, not `[]`: a policy written before the field existed has no key,
    // and "no rules" has to serialise back to exactly that.
    expect(Object.keys(currentSpec())).not.toContain("tool_rules");
  });

  it("keeps the * default out of the wire while it says hold, and writes it when it does not", async () => {
    render(<Harness initial={VALID} />);
    // The row renders at `hold` with nothing in the document — an absent "*"
    // IS hold (ToolEffectFor falls through to the human).
    const def = screen.getByLabelText("Default effect") as HTMLSelectElement;
    expect(def.value).toBe("hold");
    expect(Object.keys(currentSpec())).not.toContain("tool_rules");

    await user.selectOptions(def, "deny");
    expect(currentSpec().tool_rules).toEqual([{ tool: "*", effect: "deny" }]);
  });

  it("the default row cannot be removed", async () => {
    render(<Harness initial={VALID} />);
    expect(screen.queryByRole("button", { name: "Remove rule" })).not.toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Add rule" }));
    // One remove control, for the one named rule — never for "*".
    expect(screen.getAllByRole("button", { name: "Remove rule" })).toHaveLength(1);
  });

  it("names the server's own refusal for a duplicate tool, before Save", async () => {
    render(
      <Harness
        initial={JSON.stringify({
          ...JSON.parse(VALID),
          tool_rules: [
            { tool: "Bash", effect: "allow" },
            { tool: "Bash", effect: "deny" },
          ],
        })}
      />,
    );
    expect(await screen.findByRole("alert")).toHaveTextContent(/Two rules name “Bash”/);
  });

  it("states the narrows-never-widens rule where the rules are authored", () => {
    render(<Harness initial={VALID} />);
    expect(
      screen.getByText(/it never widens what the agent may do/),
    ).toBeInTheDocument();
  });

  // parseSpec is a bare cast: whatever object the textarea parses to arrives
  // here as a "RunPolicySpec". Typing any of these used to THROW out of the
  // section into the route's ErrorBoundary, blanking the screen and losing the
  // draft mid-edit. The section has to keep rendering and say what is wrong.
  it.each([
    ["a rule with no fields", [{}]],
    ["a rule with a numeric tool", [{ tool: 1, effect: "allow" }]],
    ["a string instead of a list", "x"],
    ["an object instead of a list", {}],
  ])("renders a refusal, not a crash, for %s", (_label, tool_rules) => {
    render(<Harness initial={JSON.stringify({ ...JSON.parse(VALID), tool_rules })} />);
    // The section itself is still on screen…
    expect(screen.getByText(/it never widens what the agent may do/)).toBeInTheDocument();
    // …and it names the problem rather than swallowing it.
    expect(screen.getByRole("alert")).toHaveTextContent(/\S/);
  });
});

// The rail's one line. It names the tools on purpose: a bare count says nothing
// about which calls still stop for a human.
describe("toolRulesSummary", () => {
  it("is null when the run has no rules, so the rail grows no empty section", () => {
    expect(toolRulesSummary({ allowed_domains: [], first_use_approval: "always_deny", min_confinement_class: "CC2" })).toBeNull();
  });

  it("names every tool, its effect, and what happens to everything else", () => {
    expect(
      toolRulesSummary({
        allowed_domains: [],
        first_use_approval: "always_deny",
        min_confinement_class: "CC2",
        tool_rules: [
          { tool: "Read", effect: "allow" },
          { tool: "Bash", effect: "hold" },
          { tool: "WebFetch", effect: "deny" },
        ],
      }),
    ).toBe("3 rules · Read allowed, Bash held, WebFetch denied. Anything else is held.");
  });

  it("reads the default off an explicit * rule, and excludes it from the count", () => {
    expect(
      toolRulesSummary({
        allowed_domains: [],
        first_use_approval: "always_deny",
        min_confinement_class: "CC2",
        tool_rules: [
          { tool: "Read", effect: "allow" },
          { tool: "*", effect: "deny" },
        ],
      }),
    ).toBe("1 rule · Read allowed. Anything else is denied.");
  });
});
