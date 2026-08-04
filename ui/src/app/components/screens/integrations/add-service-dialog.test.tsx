/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The search-first Add flow. Two claims matter most: picking a model provider
// or git host HANDS OFF to their established flow rather than writing a generic
// row, and a type with no backend home never offers an Add that cannot work.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { AddServiceDialog, slugifyIntegrationId } from "./add-service-dialog";
import { genericIntegrationsApi } from "../../../lib/api/integrations";

vi.mock("../../../lib/api/integrations", async (orig) => ({
  ...(await orig<typeof import("../../../lib/api/integrations")>()),
  genericIntegrationsApi: { put: vi.fn().mockResolvedValue(undefined), remove: vi.fn() },
}));

function renderDialog(overrides: Partial<React.ComponentProps<typeof AddServiceDialog>> = {}) {
  const props = {
    open: true,
    onOpenChange: vi.fn(),
    onAdded: vi.fn(),
    onHandoff: vi.fn(),
    ...overrides,
  };
  render(<AddServiceDialog {...props} />);
  return props;
}

beforeEach(() => vi.clearAllMocks());

describe("slugifyIntegrationId", () => {
  it("produces an id the API's identifier rule accepts", () => {
    expect(slugifyIntegrationId("Corp Artifactory")).toBe("corp-artifactory");
    expect(slugifyIntegrationId("  JFrog!! ")).toBe("jfrog");
    expect(slugifyIntegrationId("Acme.Feed_1")).toBe("acme.feed_1");
  });
});

describe("AddServiceDialog", () => {
  it("finds a type by what someone actually types, not just its product name", async () => {
    renderDialog();
    await userEvent.type(screen.getByRole("textbox", { name: /search integration types/i }), "jfrog");
    expect(await screen.findByText("JFrog Artifactory")).toBeInTheDocument();
  });

  // The handoff must carry the PICKED TYPE. Handing over only the lane sent an
  // operator who clicked "Anthropic" to a dialog asking "AI provider or SCM
  // host?" — a step backwards from what they had already answered.
  it("hands a model provider off to the AI flow carrying the type that was picked", async () => {
    const props = renderDialog();
    await userEvent.type(screen.getByRole("textbox", { name: /search integration types/i }), "anthropic");
    await userEvent.click(await screen.findByText("Anthropic"));
    expect(props.onHandoff).toHaveBeenCalledWith(
      expect.objectContaining({ id: "anthropic", addLane: "ai", apiType: "anthropic_api_key" }),
    );
    expect(genericIntegrationsApi.put).not.toHaveBeenCalled();
  });

  it("hands a git host off with its own lane, not the generic writer", async () => {
    const props = renderDialog();
    await userEvent.type(screen.getByRole("textbox", { name: /search integration types/i }), "gitlab");
    await userEvent.click(await screen.findByText("GitLab"));
    expect(props.onHandoff).toHaveBeenCalledWith(expect.objectContaining({ id: "gitlab", addLane: "scm" }));
    expect(genericIntegrationsApi.put).not.toHaveBeenCalled();
  });

  it("writes a generic row with its hosts, header and secret", async () => {
    const props = renderDialog();
    await userEvent.type(screen.getByRole("textbox", { name: /search integration types/i }), "jfrog");
    await userEvent.click(await screen.findByText("JFrog Artifactory"));
    await userEvent.click(screen.getByRole("button", { name: /add integration/i }));
    await waitFor(() => expect(genericIntegrationsApi.put).toHaveBeenCalled());
    const [id, body] = vi.mocked(genericIntegrationsApi.put).mock.calls[0];
    expect(id).toBe("jfrog-artifactory");
    expect(body).toMatchObject({
      category: "package_feed",
      type: "artifactory",
      hosts: ["artifactory.corp.internal"],
      header: "Authorization",
      format: "Bearer %s",
      credentials: { token: "artifactory-token" },
    });
    expect(props.onAdded).toHaveBeenCalled();
  });

  // A data store speaks its own wire protocol, so the proxy has nothing to
  // inject. The panel must state that instead of showing a credential field
  // that would do nothing.
  it("offers no credential field for a type that cannot carry one", async () => {
    renderDialog();
    await userEvent.type(screen.getByRole("textbox", { name: /search integration types/i }), "postgres");
    await userEvent.click(await screen.findByText("PostgreSQL"));
    expect(screen.queryByLabelText(/credential/i)).not.toBeInTheDocument();
    expect(screen.getByText(/speak their own wire protocols/i)).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: /add integration/i }));
    await waitFor(() => expect(genericIntegrationsApi.put).toHaveBeenCalled());
    const [, body] = vi.mocked(genericIntegrationsApi.put).mock.calls[0];
    expect(body.header).toBeUndefined();
    expect(body.credentials).toBeUndefined();
  });

  it("never offers a working Add for a type with no backend home", async () => {
    renderDialog();
    await userEvent.type(screen.getByRole("textbox", { name: /search integration types/i }), "ollama");
    await userEvent.click(await screen.findByText(/Self-hosted or OpenAI-compatible/i));
    expect(screen.getByText(/no home in Wardyn today/i)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /add integration/i })).toBeDisabled();
  });

  it("tells someone searching for something unlisted where it belongs", async () => {
    renderDialog();
    await userEvent.type(screen.getByRole("textbox", { name: /search integration types/i }), "zzzznope");
    expect(screen.getByText(/Anything Wardyn hasn't listed is an Other service/i)).toBeInTheDocument();
  });
});
