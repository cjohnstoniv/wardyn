/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// #542 (design §5.6, packet MP-C) — the New Run rail's provider picker.
// States R1 (one candidate), R2/R6 (several, preselected or not), R3
// (selected, not connected — its own door, now all five kinds), R4
// (residency by kind), R7 (an agent switch's change note), and R9 (no
// candidate: today's shape, unchanged) — the acceptance list packet C draws.
// R5b/R5c below are the rail-gap packet's addition (owner-approved
// 2026-09-25, docs/design/542-rail-gaps-mock/canon.md) — see
// model-provider-lane.ts's providerGate.
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

const { bedrock, claude, gateway, anthropicKey } = MODEL_PROVIDERS;

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
      modelProvider: { candidates: [bedrock], access: status.provider_access, selectedId: bedrock.id, onChange: () => {}, changeNote: null, harnessLabel: "Claude Code" },
    });
    expect(await screen.findByText(RAIL_PROVIDER.STATIC("Bedrock (prod)"))).toBeInTheDocument();
    expect(screen.getByText(RAIL_CREDENTIAL.SANDBOX_BEDROCK)).toBeInTheDocument();
    expect(screen.queryByRole("combobox", { name: RAIL_PROVIDER.LABEL })).toBeNull();
  });

  it("R4: a key/token/subscription kind reads the proxy sentence instead", async () => {
    const status = providerStatus([{ provider: gateway, state: "live" }]);
    renderRail({
      status,
      modelProvider: { candidates: [gateway], access: status.provider_access, selectedId: gateway.id, onChange: () => {}, changeNote: null, harnessLabel: "Claude Code" },
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
        harnessLabel: "Claude Code",
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
      modelProvider: { candidates: [gateway, claude], access: status.provider_access, selectedId: undefined, onChange, changeNote: null, harnessLabel: "Claude Code" },
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
      modelProvider: { candidates: [claude, MODEL_PROVIDERS.anthropicKey], access: status.provider_access, selectedId: claude.id, onChange: () => {}, changeNote: note, harnessLabel: "Claude Code" },
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
        harnessLabel: "Claude Code",
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
        harnessLabel: "Claude Code",
      },
    });
    expect(await screen.findByText(RAIL_PROVIDER.NO_TOKEN("Corp gateway"))).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: CONNECTIONS.ADD_TOKEN }));
    expect(await screen.findByRole("dialog", { name: KEY_DOOR.TITLE(true, "Corp gateway") })).toBeInTheDocument();
  });

  // #542 review finding F3 — the shared admin token grades every provider
  // "not_applicable" (provider_access.go's providerAccessMechanism: no
  // credential of its own to grade for ANY kind). Not "connected" either, but
  // rendering NOT_SIGNED_IN here would be a false warning over a door that can
  // never work — there is no person behind this principal to sign in.
  it("not_applicable (the shared admin token) shows no warning and no door button", async () => {
    const status = providerStatus([
      { provider: bedrock, state: "not_applicable" },
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
        harnessLabel: "Claude Code",
      },
    });
    await screen.findByRole("combobox", { name: RAIL_PROVIDER.LABEL });
    expect(screen.queryByText(RAIL_PROVIDER.NOT_SIGNED_IN("Bedrock (prod)"))).toBeNull();
    expect(screen.queryByRole("button", { name: AGENTS.SIGN_IN_AWS })).toBeNull();
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
        harnessLabel: "Claude Code",
      },
    });
    await screen.findByRole("combobox", { name: RAIL_PROVIDER.LABEL });
    expect(screen.queryByText(RAIL_PROVIDER.NOT_SIGNED_IN("Bedrock (prod)"))).toBeNull();
    expect(screen.queryByRole("button", { name: AGENTS.SIGN_IN_AWS })).toBeNull();
  });

  // #542 rail-gap packet — the three provider kinds the original build had no
  // door for: a stored key (anthropic_api_key/openai_api_key share one
  // sentence, D2) and the non-Bedrock sign-in kind (anthropic_subscription).
  it("R3 key: anthropic_api_key names the key, and its button opens the key door", async () => {
    const status = providerStatus([
      { provider: anthropicKey, state: "not_configured" },
      { provider: claude, state: "live" },
    ]);
    renderRail({
      status,
      modelProvider: {
        candidates: [anthropicKey, claude],
        access: status.provider_access,
        selectedId: anthropicKey.id,
        onChange: () => {},
        changeNote: null,
        harnessLabel: "Claude Code",
      },
    });
    expect(await screen.findByText(RAIL_PROVIDER.NO_KEY("Anthropic API key"))).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: CONNECTIONS.ADD_KEY }));
    expect(await screen.findByRole("dialog", { name: KEY_DOOR.TITLE(false, "Anthropic API key") })).toBeInTheDocument();
  });

  it("R3 subscription: anthropic_subscription names Claude, and its button opens the sign-in door", async () => {
    const status = providerStatus([
      { provider: claude, state: "not_configured" },
      { provider: anthropicKey, state: "live" },
    ]);
    renderRail({
      status,
      modelProvider: {
        candidates: [claude, anthropicKey],
        access: status.provider_access,
        selectedId: claude.id,
        onChange: () => {},
        changeNote: null,
        harnessLabel: "Claude Code",
      },
    });
    expect(await screen.findByText(RAIL_PROVIDER.NOT_SIGNED_IN_CLAUDE("Claude subscription"))).toBeInTheDocument();
    expect(screen.getByRole("button", { name: CONNECTIONS.SIGN_IN_CLAUDE })).toBeInTheDocument();
  });
});

describe("R9 — no candidate for this agent: today's shape, unchanged", () => {
  it("an empty candidate list renders no picker and falls through to the deployment warning", async () => {
    const status = providerStatus([]);
    renderRail({
      status,
      showModelWarning: true,
      modelProvider: { candidates: [], access: [], selectedId: undefined, onChange: () => {}, changeNote: null, harnessLabel: "Claude Code" },
    });
    expect(screen.queryByRole("combobox", { name: RAIL_PROVIDER.LABEL })).toBeNull();
    expect(screen.queryByText(RAIL_PROVIDER.PLACEHOLDER)).toBeNull();
    expect(await screen.findByText(RAIL_MODEL_ACCESS.NO_PROVIDER)).toBeInTheDocument();
  });
});

// #542 rail-gap packet (owner-approved 2026-09-25) — R5b/R5c, distinct from
// R9's "nothing serves this agent at all": something does, but nobody may
// launch on it yet, and this rail names WHY instead of falling through to R9's
// generic deployment warning.
describe("R5b — granted none: no select, Launch refused, naming the harness", () => {
  it("renders NOT_GRANTED with no picker at all", async () => {
    const status = providerStatus([{ provider: gateway, state: "not_configured" }]);
    renderRail({
      status,
      modelProvider: {
        candidates: [],
        access: status.provider_access,
        selectedId: undefined,
        onChange: () => {},
        changeNote: null,
        gate: { kind: "not_granted" },
        harnessLabel: "Claude Code",
      },
    });
    expect(await screen.findByText(RAIL_PROVIDER.NOT_GRANTED("Claude Code"))).toBeInTheDocument();
    expect(screen.queryByRole("combobox", { name: RAIL_PROVIDER.LABEL })).toBeNull();
  });
});

describe("R5c — the default is turned off: never auto-picked, even alone", () => {
  it("with another candidate: a Select with the placeholder, nothing preselected, naming the disabled default", async () => {
    const status = providerStatus([{ provider: claude, state: "live" }]);
    renderRail({
      status,
      modelProvider: {
        candidates: [claude],
        access: status.provider_access,
        selectedId: undefined,
        onChange: () => {},
        changeNote: null,
        gate: { kind: "default_off", provider: gateway },
        harnessLabel: "Claude Code",
      },
    });
    // Even though exactly one candidate remains, R1's STATIC shortcut must
    // not fire — the person still gets an explicit ask.
    const trigger = await screen.findByRole("combobox", { name: RAIL_PROVIDER.LABEL });
    expect(trigger).toHaveTextContent(RAIL_PROVIDER.PLACEHOLDER);
    expect(screen.getByText(RAIL_PROVIDER.DEFAULT_OFF("Corp gateway", "Claude Code"))).toBeInTheDocument();
  });

  it("with no other candidate: no Select at all, naming the disabled default", async () => {
    const status = providerStatus([]);
    renderRail({
      status,
      modelProvider: {
        candidates: [],
        access: [],
        selectedId: undefined,
        onChange: () => {},
        changeNote: null,
        gate: { kind: "default_off", provider: gateway },
        harnessLabel: "Claude Code",
      },
    });
    expect(await screen.findByText(RAIL_PROVIDER.DEFAULT_OFF_ONLY("Corp gateway", "Claude Code"))).toBeInTheDocument();
    expect(screen.queryByRole("combobox", { name: RAIL_PROVIDER.LABEL })).toBeNull();
  });
});
