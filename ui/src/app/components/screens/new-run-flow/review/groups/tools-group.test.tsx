/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { NR_REVIEW } from "../../../../wardyn/copy/new-run/review";
import { OIDC_SIGN_IN } from "../../../../wardyn/copy/oidc-signin";
import { initialDraft, type RunIntent } from "../../draft/run-intent";
import { ToolsGroup } from "./tools-group";

const h = vi.hoisted(() => ({ providers: [] as unknown[] }));
vi.mock("../../../../wardyn/model-access-context", () => ({
  useShellSetupStatus: () => ({ status: { model_providers: h.providers }, refresh: () => {} }),
}));

afterEach(() => {
  cleanup();
  h.providers = [];
});

const INTENT: RunIntent = {
  shape: "agent_task",
  experience: "interactive",
  tools: [{ id: "claude-code", kind: "harness" }, { id: "codex-cli", kind: "harness" }],
  startup: { kind: "harness", tool: "claude-code" },
};

// A harness tile's name and status chip need about 250 px; a 320 px phone has about 150, so the chip ran
// past the group card and was clipped. Under the console's narrow breakpoint the tile stacks.
describe("Review: a harness tile on a narrow screen", () => {
  it("stacks its name, status chip and prompt line under the narrow breakpoint", () => {
    const draft = { ...initialDraft("alice"), intent: INTENT };
    render(<ToolsGroup draft={draft} pre={null} preview={null} docRows={[]} go={vi.fn()} dispatch={vi.fn()} />);
    const tile = screen.getByText(NR_REVIEW.HARNESS_AFTER).closest("div")!;
    expect(tile.className).toContain("max-[520px]:grid-cols-1");
    expect(tile.className).toContain("max-[520px]:justify-items-start");
  });
});

describe("Review: the count line for Autonomous harnesses", () => {
  const AUTO: RunIntent = {
    shape: "agent_task",
    experience: "background",
    workload: { kind: "agent_task", task: "fix it" },
    tools: [{ id: "claude-code", kind: "harness" }],
  };
  const show = (intent: RunIntent) => {
    const draft = { ...initialDraft("alice"), intent };
    render(<ToolsGroup draft={draft} pre={null} preview={null} docRows={[]} go={vi.fn()} dispatch={vi.fn()} />);
  };

  it("says '1 harness starts automatically' for the only harness, not '1 of 1 harnesses'", () => {
    show(AUTO);
    expect(screen.getByText("1 harness starts automatically")).toBeInTheDocument();
    expect(screen.queryByText(/1 of 1 harnesses/)).not.toBeInTheDocument();
  });

  it("keeps the noun singular when the only harness has no task to start on ('0 of 1 harness starts')", () => {
    show({ ...AUTO, workload: { kind: "agent_task", task: "" } });
    expect(screen.getByText("0 of 1 harness starts automatically")).toBeInTheDocument();
    expect(screen.queryByText(/harnesses/)).not.toBeInTheDocument();
  });

  it("keeps the plural count when there are several", () => {
    show({ ...AUTO, tools: [...(AUTO.tools ?? []), { id: "codex-cli", kind: "harness" }] });
    expect(screen.getByText("1 of 2 harnesses start automatically")).toBeInTheDocument();
  });
});

describe("Review: the allowed models line", () => {
  const draft = { ...initialDraft("alice"), intent: INTENT };
  const harness = (id: string, model?: string, fast_model?: string) => ({
    id,
    starts: id === "claude-code",
    model_credential: { provider: "Provider A", kind: "custom_endpoint", residency: "elsewhere", model, fast_model },
  });
  const show = (pre: Record<string, unknown> | null) =>
    render(<ToolsGroup draft={draft} pre={pre as never} preview={null} docRows={[]} go={vi.fn()} dispatch={vi.fn()} />);

  it("adds nothing when no limit binds the run", () => {
    show({ harness_set: { harnesses: [harness("claude-code", "model-a")], shared_model_access: false } });
    expect(screen.queryByTestId("review-allowed-models")).toBeNull();
  });

  it("names the models and what reads them, and warns for none while every harness model is listed", () => {
    show({ allowed_models: ["model-a", "small-model-a"], harness_set: { harnesses: [harness("claude-code", "model-a", "small-model-a")], shared_model_access: false } });
    const line = screen.getByTestId("review-allowed-models");
    expect(line).toHaveTextContent("Your organisation limits the models this run may use: model-a, small-model-a.");
    expect(line).toHaveTextContent("The run's proxy reads each model request to check its model.");
    expect(screen.queryByTestId("review-allowed-models-warning")).toBeNull();
  });

  it("warns in amber, without blocking, for a harness model off the list", () => {
    show({ allowed_models: ["model-a"], harness_set: { harnesses: [harness("claude-code", "model-b", "model-a"), harness("codex-cli", "model-c")], shared_model_access: false } });
    const warnings = screen.getAllByTestId("review-allowed-models-warning");
    expect(warnings.map((w) => w.textContent)).toEqual([
      "claude-code on Provider A is set to use model-b, which is not on that list. Its requests will be refused.",
      "codex-cli on Provider A is set to use model-c, which is not on that list. Its requests will be refused.",
    ]);
    expect(warnings[0]).toHaveClass("text-warning");
  });

  it("reads the list off the policy preview when the preflight has none", () => {
    render(
      <ToolsGroup
        draft={draft}
        pre={null}
        preview={{ allowed_models: ["model-a"], allowed_models_unit: "Interns" } as never}
        docRows={[]}
        go={vi.fn()}
        dispatch={vi.fn()}
      />,
    );
    expect(screen.getByTestId("review-allowed-models")).toHaveTextContent("model-a");
    expect(screen.getByTestId("review-allowed-models")).toHaveTextContent("Governance · Interns");
  });

  it("names the profile that sets the list on the Governance chip, and shows no chip without a name", () => {
    const { unmount } = show({ allowed_models: ["model-a"], allowed_models_unit: "Contractors" });
    expect(screen.getByTestId("review-allowed-models")).toHaveTextContent("Governance · Contractors");
    unmount();
    show({ allowed_models: ["model-a"] });
    expect(screen.getByTestId("review-allowed-models")).not.toHaveTextContent("Governance ·");
  });
});

describe("Review: a provider that is not bound to the person's sign-in", () => {
  const sso = (identity_binding: string) => ({
    id: "corp-sso", name: "Corp SSO", kind: "custom_endpoint", harnesses: ["claude-code"], host: "h", sign_in: "oidc", identity_binding,
  });
  const show = () => {
    const intent: RunIntent = { ...INTENT, tools: [{ id: "claude-code", kind: "harness", model_provider: "corp-sso" }] };
    render(<ToolsGroup draft={{ ...initialDraft("alice"), intent }} pre={null} preview={null} docRows={[]} go={vi.fn()} dispatch={vi.fn()} />);
  };

  it("says so under the harness list when binding is off", () => {
    h.providers = [sso("none")];
    show();
    expect(screen.getByText(OIDC_SIGN_IN.REVIEW_NOT_BOUND("Corp SSO"))).toBeInTheDocument();
  });

  it("says nothing when binding is on", () => {
    h.providers = [sso("console")];
    show();
    expect(screen.queryByText(/not bound to your sign-in/)).toBeNull();
    // The harness line names the provider by its display name from setup status, as the frame does.
    expect(screen.getByText(/Corp SSO/)).toBeInTheDocument();
    expect(screen.queryByText(/corp-sso/)).toBeNull();
  });

  it("names the provider by its id while setup status does not list it", () => {
    h.providers = [];
    show();
    expect(screen.getByText(/corp-sso/)).toBeInTheDocument();
  });
});
