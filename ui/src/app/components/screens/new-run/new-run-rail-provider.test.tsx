/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// #542 (design §5.6, packet MP-C) — the New Run rail's provider picker.
// States R1 (one candidate), R2/R6 (several, preselected or not), R3
// (selected, not connected — its own door), R4 (residency by kind), R7 (an
// agent switch's change note), and R9 (no candidate: today's shape,
// unchanged) — the acceptance list packet C draws (R5's states are excluded
// by owner ruling; see model-provider-lane.ts's header comment).
import type { ComponentProps } from "react";
import { describe, expect, it, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

vi.mock("../../../lib/api/health", () => ({
  health: { health: () => Promise.resolve({ components: { recording: { selected: "fs" } } }) },
}));
vi.mock("../settings/harness-login-pane", () => ({
  HarnessLoginPane: ({ onDone }: { onDone: () => void }) => (
    <button type="button" onClick={onDone}>
      fake pane
    </button>
  ),
}));

import { RunRail } from "./new-run-rail";
import { RAIL_CREDENTIAL, RAIL_PROVIDER } from "../../wardyn/copy";
import { RAIL_MODEL_ACCESS, MODEL_ACCESS_BANNER } from "../../wardyn/model-access-copy";
import { CONNECTIONS, DOOR, KEY_DOOR } from "../../wardyn/copy/door";
import { AGENTS } from "../../../lib/workspace-providers-copy";
import { MODEL_PROVIDERS, providerStatus } from "../../../lib/test-fixtures";
import { WithDoor } from "../../../../test/door-harness";

const { bedrock, claude, gateway } = MODEL_PROVIDERS;

function baseLaunch(overrides: Partial<ComponentProps<typeof RunRail>["launch"]> = {}) {
  return {
    onLaunch: () => {},
    disabled: false,
    spinning: false,
    inFlight: false,
    problem: null,
    error: null,
    errorSeq: 0,
    credentialRefused: false,
    warnings: [],
    onOpenRun: null,
    ...overrides,
  };
}

function renderRail(opts: {
  status: Parameters<typeof WithDoor>[0]["status"];
  modelProvider?: ComponentProps<typeof RunRail>["modelProvider"];
  showModelWarning?: boolean;
}) {
  render(
    <WithDoor status={opts.status} path="/runs/new" operator={false} principal="bob@acme.example">
      <RunRail
        cc="CC1"
        showModelWarning={opts.showModelWarning ?? false}
        startup="It starts."
        showHoldNote={false}
        toolRules={null}
        launch={baseLaunch()}
        preflight={{ error: null, errorSeq: 0, result: null }}
        modelProvider={opts.modelProvider}
        adoDialog={{
          open: false,
          connecting: false,
          org: "",
          blockedUrl: null,
          onConfirm: () => {},
          onFallbackClick: () => {},
          onCancel: () => {},
        }}
      />
    </WithDoor>,
  );
}

describe("R1 — one candidate: a static line, no picker (QC-1)", () => {
  it("names the provider and states residency from its kind — no Select at all", async () => {
    const status = providerStatus([{ provider: bedrock, state: "live" }]);
    renderRail({
      status,
      modelProvider: { candidates: [bedrock], access: status.provider_access, selectedId: bedrock.id, onChange: () => {}, changeNote: null },
    });
    expect(await screen.findByText(RAIL_PROVIDER.STATIC("Bedrock (prod)"))).toBeInTheDocument();
    expect(screen.getByText(RAIL_CREDENTIAL.SANDBOX_BEDROCK)).toBeInTheDocument();
    expect(screen.queryByRole("combobox", { name: RAIL_PROVIDER.LABEL })).toBeNull();
  });

  it("R4: a key/token/subscription kind reads the proxy sentence instead", async () => {
    const status = providerStatus([{ provider: gateway, state: "live" }]);
    renderRail({
      status,
      modelProvider: { candidates: [gateway], access: status.provider_access, selectedId: gateway.id, onChange: () => {}, changeNote: null },
    });
    expect(await screen.findByText(RAIL_PROVIDER.STATIC("Corp gateway"))).toBeInTheDocument();
    expect(screen.getByText(RAIL_CREDENTIAL.PROXY)).toBeInTheDocument();
  });
});

describe("R2/R6 — several candidates: a Select, each option stating what you provide (QC-2)", () => {
  it("R2: shows the preselected default's option text as the trigger's value", async () => {
    const status = providerStatus([
      { provider: gateway, defaultFor: ["claude-code"], state: "live" },
      { provider: claude, state: "live" },
    ]);
    renderRail({
      status,
      modelProvider: {
        candidates: [gateway, claude],
        access: status.provider_access,
        selectedId: gateway.id,
        onChange: () => {},
        changeNote: null,
      },
    });
    expect(await screen.findByRole("combobox", { name: RAIL_PROVIDER.LABEL })).toHaveTextContent(
      RAIL_PROVIDER.OPTION("Corp gateway", "token", "added"),
    );
  });

  it("R6 (QC-4): nothing preselected shows the placeholder, and picking a candidate calls back up", async () => {
    const status = providerStatus([
      { provider: gateway, state: "not_configured" },
      { provider: claude, state: "not_configured" },
    ]);
    const onChange = vi.fn();
    renderRail({
      status,
      modelProvider: { candidates: [gateway, claude], access: status.provider_access, selectedId: undefined, onChange, changeNote: null },
    });
    const trigger = await screen.findByRole("combobox", { name: RAIL_PROVIDER.LABEL });
    expect(trigger).toHaveTextContent(RAIL_PROVIDER.PLACEHOLDER);
    await userEvent.click(trigger);
    await userEvent.click(await screen.findByRole("option", { name: RAIL_PROVIDER.OPTION("Corp gateway", "token", "not added") }));
    expect(onChange).toHaveBeenCalledWith(gateway.id);
  });
});

describe("R7 — an agent switch that invalidated the selection names the change", () => {
  it("renders the CHANGED sentence the screen composed", async () => {
    const status = providerStatus([{ provider: claude, state: "live" }]);
    const note = RAIL_PROVIDER.CHANGED("Claude subscription", "Corp gateway", "Claude Code");
    renderRail({
      status,
      modelProvider: { candidates: [claude, MODEL_PROVIDERS.anthropicKey], access: status.provider_access, selectedId: claude.id, onChange: () => {}, changeNote: note },
    });
    expect(await screen.findByText(note)).toBeInTheDocument();
  });
});

describe("R3 — selected, not connected: a warning and its own door (§5.8)", () => {
  it("bedrock_sso: names AWS, and its button opens THAT provider's sign-in door", async () => {
    const status = providerStatus([
      { provider: bedrock, state: "not_configured" },
      { provider: claude, state: "live" },
    ]);
    renderRail({
      status,
      modelProvider: {
        candidates: [bedrock, claude],
        access: status.provider_access,
        selectedId: bedrock.id,
        onChange: () => {},
        changeNote: null,
      },
    });
    expect(await screen.findByText(RAIL_PROVIDER.NOT_SIGNED_IN("Bedrock (prod)"))).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: AGENTS.SIGN_IN_AWS }));
    const dialog = await screen.findByRole("dialog", { name: MODEL_ACCESS_BANNER.DIALOG_TITLE });
    expect(dialog).toHaveTextContent(DOOR.FOR("Bedrock (prod)"));
  });

  it("custom_endpoint: names the token, and its button opens the key door", async () => {
    const status = providerStatus([
      { provider: gateway, state: "not_configured" },
      { provider: claude, state: "live" },
    ]);
    renderRail({
      status,
      modelProvider: {
        candidates: [gateway, claude],
        access: status.provider_access,
        selectedId: gateway.id,
        onChange: () => {},
        changeNote: null,
      },
    });
    expect(await screen.findByText(RAIL_PROVIDER.NO_TOKEN("Corp gateway"))).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: CONNECTIONS.ADD_TOKEN }));
    expect(await screen.findByRole("dialog", { name: KEY_DOOR.TITLE(true, "Corp gateway") })).toBeInTheDocument();
  });

  it("a connected selection shows no warning and no door button", async () => {
    const status = providerStatus([
      { provider: bedrock, state: "live" },
      { provider: claude, state: "live" },
    ]);
    renderRail({
      status,
      modelProvider: {
        candidates: [bedrock, claude],
        access: status.provider_access,
        selectedId: bedrock.id,
        onChange: () => {},
        changeNote: null,
      },
    });
    await screen.findByRole("combobox", { name: RAIL_PROVIDER.LABEL });
    expect(screen.queryByText(RAIL_PROVIDER.NOT_SIGNED_IN("Bedrock (prod)"))).toBeNull();
    expect(screen.queryByRole("button", { name: AGENTS.SIGN_IN_AWS })).toBeNull();
  });
});

describe("R9 — no candidate for this agent: today's shape, unchanged", () => {
  it("an empty candidate list renders no picker and falls through to the deployment warning", async () => {
    const status = providerStatus([]);
    renderRail({
      status,
      showModelWarning: true,
      modelProvider: { candidates: [], access: [], selectedId: undefined, onChange: () => {}, changeNote: null },
    });
    expect(screen.queryByRole("combobox", { name: RAIL_PROVIDER.LABEL })).toBeNull();
    expect(screen.queryByText(RAIL_PROVIDER.PLACEHOLDER)).toBeNull();
    expect(await screen.findByText(RAIL_MODEL_ACCESS.NO_PROVIDER)).toBeInTheDocument();
  });
});
