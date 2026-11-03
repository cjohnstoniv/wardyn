/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The org editor's "Token audience" (mcp.gateway_audience): only on an organisation's remote MCP server,
// sent back with the stored server, and the server's refusal shown under the field.
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { Component } from "../../../lib/types";
import { HttpError } from "../../../lib/api/core";
import { aheadByHours } from "../../../lib/test-clock";
import { COMPONENTS_ADMIN as T } from "../../wardyn/copy/components-admin";
import { setField } from "../../../../test/set-field";
import { ComponentEditor } from "./component-editor";

const put = vi.fn();
vi.mock("../../../lib/api/components", () => ({
  components: { put: (...a: unknown[]) => put(...a) },
}));
vi.mock("../../../lib/api/model-providers", () => ({
  modelProviders: {
    getModelProviders: vi.fn(async () => ({
      providers: { providers: [] },
      connected: {},
      etag: null,
      gatewayIssuer: "spiffe://contoso.example",
      gatewayJwksPath: "/.well-known/wardyn-experimental/gateway-jwks.json",
    })),
  },
}));

const REMOTE: Component = {
  id: "22222222-2222-4222-8222-222222222222",
  name: "Docs search",
  version: 2,
  created_at: aheadByHours(-24),
  updated_at: aheadByHours(-24),
  definition: {
    hosts: ["mcp.contoso.example"],
    mcp: { transport: "http", url: "https://mcp.contoso.example/mcp" },
  },
};

const withAudience = (c: Component, gateway_audience: string): Component => ({
  ...c,
  definition: {
    ...c.definition,
    mcp: { ...c.definition.mcp!, gateway_audience },
  },
});

const field = () => screen.queryByLabelText(T.AUDIENCE);

function renderEditor(component: Component, onSaved = vi.fn()) {
  render(
    <ComponentEditor
      component={component}
      onClose={vi.fn()}
      onSaved={onSaved}
    />,
  );
  return onSaved;
}

beforeEach(() => {
  put.mockReset();
});
afterEach(() => cleanup());

describe("ComponentEditor token audience", () => {
  it("an organisation's remote server shows the empty field with its hint and nothing more", () => {
    renderEditor(REMOTE);
    expect(field()).toHaveValue("");
    expect(screen.getByText(T.AUDIENCE_HINT)).toBeInTheDocument();
    expect(screen.queryByText(T.AUDIENCE_SENDS)).toBeNull();
    expect(screen.queryByText(T.AUDIENCE_SAVED_NOTE)).toBeNull();
  });

  it("a saved audience shows what the token carries, the issuer and key address, and the note on running runs", async () => {
    renderEditor(withAudience(REMOTE, "wardyn-docs-gateway"));
    expect(field()).toHaveValue("wardyn-docs-gateway");
    expect(screen.getByText(T.AUDIENCE_SENDS)).toBeInTheDocument();
    expect(screen.getByText(T.AUDIENCE_SAVED_NOTE)).toBeInTheDocument();
    expect(
      await screen.findByText("spiffe://contoso.example"),
    ).toBeInTheDocument();
    expect(
      screen.getByText(
        `${window.location.origin}/.well-known/wardyn-experimental/gateway-jwks.json`,
      ),
    ).toBeInTheDocument();
  });

  it("is hidden on a person's server and on one that runs in the sandbox", () => {
    renderEditor({ ...REMOTE, owner: "alice@example.org" });
    expect(field()).toBeNull();
    cleanup();
    renderEditor({
      ...REMOTE,
      definition: {
        hosts: [],
        mcp: {
          transport: "stdio",
          launch: { command: "npx", args: ["@acme/mcp@1.2.3"] },
        },
      },
    });
    expect(field()).toBeNull();
  });

  it("sends the stored server back with the audience as edited, and none once cleared", async () => {
    const user = userEvent.setup();
    put.mockResolvedValue({} as never);
    renderEditor(REMOTE);
    setField(field()!, " wardyn-docs-gateway ");
    await user.click(screen.getByRole("button", { name: T.SAVE }));
    await waitFor(() => expect(put).toHaveBeenCalledTimes(1));
    expect(put.mock.calls[0][1].definition.mcp).toEqual({
      transport: "http",
      url: "https://mcp.contoso.example/mcp",
      gateway_audience: "wardyn-docs-gateway",
    });

    cleanup();
    renderEditor(withAudience(REMOTE, "wardyn-docs-gateway"));
    setField(field()!, "");
    expect(screen.getByText(T.AUDIENCE_SAVED_NOTE)).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: T.SAVE }));
    await waitFor(() => expect(put).toHaveBeenCalledTimes(2));
    expect(put.mock.calls[1][1].definition.mcp).toEqual({
      transport: "http",
      url: "https://mcp.contoso.example/mcp",
    });
  });

  it.each([
    'invalid component: definition.mcp.gateway_audience: "wardyn-corp-gateway" is used by MCP server "Docs search" and provider "corp-gateway" at different addresses; use a different one for each',
    "invalid component: definition.mcp.gateway_audience: run identity is turned on per model provider, which also makes the signing key; turn it on for a provider first",
    "invalid component: definition.mcp.gateway_audience: must be printable text of at most 256 bytes, with no space at either end",
  ])("shows the server's refusal under the field: %s", async (msg) => {
    const user = userEvent.setup();
    put.mockRejectedValue(new HttpError(400, msg));
    const onSaved = renderEditor(REMOTE);
    setField(field()!, "wardyn-corp-gateway");
    await user.click(screen.getByRole("button", { name: T.SAVE }));
    const alert = await screen.findByRole("alert");
    expect(alert).toHaveTextContent(msg);
    expect(screen.getAllByRole("alert")).toHaveLength(1);
    expect(field()).toHaveAttribute("aria-invalid", "true");
    expect(onSaved).not.toHaveBeenCalled();
  });
});
