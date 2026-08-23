/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import { describe, it, expect, vi } from "vitest";
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

import {
  FIELD_HELP,
  POLICY_TEMPLATES,
  PolicyPanel,
  templateText,
  type PolicyPanelInstance,
  type PolicyPanelProps,
} from "./policy-panel";
import type { RunPolicySpec } from "../../lib/types";

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

  it("templates carry auto_stop_after_sec only where the source example has a real value", () => {
    for (const t of POLICY_TEMPLATES) {
      const parsed = JSON.parse(templateText(t)) as RunPolicySpec;
      if (t.id === "ci" || t.id === "model-provider") {
        // ci.json and ci-claude-llm.json both set a real idle cap — and
        // model-provider is the one template holding a minted credential.
        expect(parsed.auto_stop_after_sec).toBe(3600);
      } else {
        // OMITTED, not 0 and not -1 — absent already means "never reaped".
        expect(Object.keys(parsed)).not.toContain("auto_stop_after_sec");
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
