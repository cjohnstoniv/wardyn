/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Who checks the prompts sent to a gateway (the GW-INSPECT board, with its verdict): the switch inside the
// identity box, its help in each state, the sealed-route note before Save on a saved provider, and what
// the wire carries.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ModelProvidersList } from "../../../lib/api/model-providers";
import { PROVIDER_EDITOR as E } from "../../../lib/model-providers-copy";
import type { ModelProvider } from "../../../lib/types/site";
import { OperatorProvider } from "../../wardyn/operator-context";
import { draftFrom, providerFrom } from "./model-provider-draft";
import { ModelProviderEditor } from "./model-provider-editor";

vi.mock("sonner", () => ({ toast: { success: vi.fn(), error: vi.fn() } }));
const putMock = vi.fn();
vi.mock("../../wardyn/availability-control", async (importOriginal) => ({
  ...(await importOriginal<typeof import("../../wardyn/availability-control")>()),
  writeAvailability: () => Promise.resolve(undefined),
}));
vi.mock("../../../lib/api/model-providers", () => ({
  modelProviders: { putModelProviders: (...a: unknown[]) => putMock(...a) },
}));

const G = E.GATEWAY;
const HARNESSES = [{ id: "claude-code", display: "Claude Code" }];
const CORP: ModelProvider = {
  id: "corp-gateway",
  uid: "u-1",
  name: "Corp gateway",
  kind: "custom_endpoint",
  base_url: "https://gateway.example.com",
  auth: { header: "Authorization", format: "Bearer %s" },
  gateway_identity: { audience: "wardyn-corp-gateway" },
  harnesses: [{ harness: "claude-code", path: "/anthropic" }],
};
const list = (providers: ModelProvider[]): ModelProvidersList => ({
  providers: { providers },
  etag: '"e1"',
  connected: {},
  gatewayIssuer: "spiffe://acme.example.com",
  gatewayJwksPath: "/.well-known/wardyn-experimental/gateway-jwks.json",
});
const putBody = () => JSON.parse(JSON.stringify(putMock.mock.calls[0][0])) as { providers: ModelProvider[] };
const inspect = () => screen.getByRole("switch", { name: G.INSPECT_SWITCH });

function renderEditor(editing: ModelProvider, operator = true) {
  const user = userEvent.setup();
  render(
    <OperatorProvider operator={operator} principal="alice">
      <ModelProviderEditor list={list([editing])} editing={editing} harnesses={HARNESSES} onClose={vi.fn()} onSaved={vi.fn()} />
    </OperatorProvider>,
  );
  return { user };
}

beforeEach(() => {
  putMock.mockReset().mockImplementation((doc) => Promise.resolve({ providers: doc, etag: '"e2"' }));
});

describe("the inspection switch", () => {
  it("uses the approved words", () => {
    expect(G.INSPECT_HEADING).toBe("Inspection");
    expect(G.INSPECT_SWITCH).toBe("Let this gateway check prompts");
    expect(G.INSPECT_ON).toBe(
      "Wardyn keeps the run's secret-value check when that check is enabled by its policy. Other prompt checks are left to this gateway.",
    );
    expect(G.INSPECT_SEALED).toBe(
      "After you save this, existing runs stop starting new gateway calls within 10 minutes. Create new runs to use the change.",
    );
  });

  it("is off by default, under its heading, with the hint read with it", () => {
    renderEditor(CORP);
    expect(screen.getByText(G.INSPECT_HEADING)).toBeInTheDocument();
    expect(inspect()).toHaveAttribute("aria-checked", "false");
    expect(document.getElementById(inspect().getAttribute("aria-describedby")!)).toHaveTextContent(G.INSPECT_OFF);
    expect(screen.queryByText(G.INSPECT_SEALED)).not.toBeInTheDocument();
  });

  it("is not shown with identity off: it lives in the identity box", () => {
    renderEditor({ ...CORP, gateway_identity: undefined });
    expect(screen.queryByRole("switch", { name: G.INSPECT_SWITCH })).not.toBeInTheDocument();
    expect(screen.queryByText(G.INSPECT_HEADING)).not.toBeInTheDocument();
  });

  it("on a saved provider, says before Save what the change does to runs already going, and saves the value", async () => {
    const { user } = renderEditor(CORP);
    await user.click(inspect());
    expect(inspect()).toHaveAttribute("aria-checked", "true");
    expect(screen.getByText(G.INSPECT_ON)).toBeInTheDocument();
    expect(screen.getByText(G.INSPECT_SEALED)).toBeInTheDocument();
    expect(inspect().getAttribute("aria-describedby")!.split(" ")).toHaveLength(2);
    await user.click(screen.getByRole("button", { name: E.SAVE }));
    expect(putBody().providers[0].gateway_identity).toEqual({ audience: "wardyn-corp-gateway", inspection: "gateway" });
  });

  it("turned back off on a saved provider also notes the change, and drops the field", async () => {
    const { user } = renderEditor({ ...CORP, gateway_identity: { audience: "wardyn-corp-gateway", inspection: "gateway" } });
    expect(inspect()).toHaveAttribute("aria-checked", "true");
    expect(screen.queryByText(G.INSPECT_SEALED)).not.toBeInTheDocument();
    await user.click(inspect());
    expect(screen.getByText(G.INSPECT_SEALED)).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: E.SAVE }));
    expect(putBody().providers[0].gateway_identity).toEqual({ audience: "wardyn-corp-gateway" });
  });

  it("is read-only for someone who is not an operator", () => {
    renderEditor(CORP, false);
    expect(inspect()).toBeDisabled();
  });

  it("is not offered on a Bedrock key row, which says why, and a stored value is not sent back", async () => {
    const bedrock: ModelProvider = {
      id: "bedrock-keys",
      uid: "u-3",
      name: "Bedrock keys",
      kind: "bedrock_bearer",
      bedrock: { region: "us-east-1", base_url: "https://gateway.example.com/bedrock" },
      gateway_identity: { audience: "wardyn-bedrock", inspection: "gateway" },
      harnesses: [{ harness: "claude-code", model: "us.anthropic.profile" }],
    };
    const { user } = renderEditor(bedrock);
    expect(screen.queryByRole("switch", { name: G.INSPECT_SWITCH })).not.toBeInTheDocument();
    expect(screen.getByText(G.INSPECT_NOT_BEDROCK)).toBeInTheDocument();
    expect(screen.queryByText(G.INSPECT_ON)).not.toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: E.SAVE }));
    expect(putBody().providers[0].gateway_identity).toEqual({ audience: "wardyn-bedrock" });
  });

  it("round-trips through the draft, and identity off drops it", () => {
    const on = { ...CORP, gateway_identity: { audience: "a", inspection: "gateway" as const } };
    const d = draftFrom(on, HARNESSES);
    expect(d.gatewayInspect).toBe(true);
    expect(d.gatewaySaved?.inspect).toBe(true);
    expect(providerFrom(d, on, []).gateway_identity).toEqual({ audience: "a", inspection: "gateway" });
    expect(providerFrom({ ...d, gatewayInspect: false }, on, []).gateway_identity?.inspection).toBeUndefined();
    expect(providerFrom({ ...d, gatewayOn: false }, on, []).gateway_identity).toBeUndefined();
  });
});
