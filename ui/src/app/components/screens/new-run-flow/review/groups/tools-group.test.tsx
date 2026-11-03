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
  });
});
