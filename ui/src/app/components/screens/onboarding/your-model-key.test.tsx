/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { HttpError } from "../../../lib/api/core";

const listSecretsMineMock = vi.fn();
const setSecretMock = vi.fn();
const deleteSecretMock = vi.fn();
vi.mock("../../../lib/api/secrets", () => ({
  secrets: {
    listSecretsMine: (...a: unknown[]) => listSecretsMineMock(...a),
    setSecret: (...a: unknown[]) => setSecretMock(...a),
    deleteSecret: (...a: unknown[]) => deleteSecretMock(...a),
  },
}));

import { YourModelKey } from "./your-model-key";

describe("YourModelKey", () => {
  beforeEach(() => {
    listSecretsMineMock.mockReset();
    setSecretMock.mockReset().mockResolvedValue(undefined);
    deleteSecretMock.mockReset().mockResolvedValue(undefined);
  });

  it("empty state: no own key, admin hasn't provided one — shows the bring-your-own form", async () => {
    listSecretsMineMock.mockResolvedValue({ names: [], mine: [] });
    render(<YourModelKey llmReady={false} variant="default" onDoneChange={() => {}} />);

    expect(await screen.findByText(/Bring your own key/)).toBeInTheDocument();
    expect(screen.getByText("anthropic-api-key")).toBeInTheDocument();
    expect(screen.getByPlaceholderText("sk-ant-…")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Save key" })).toBeDisabled();
  });

  it("set state: own key present — masked value, Rotate/Remove, Done in the header", async () => {
    listSecretsMineMock.mockResolvedValue({ names: ["anthropic-api-key"], mine: ["anthropic-api-key"] });
    render(<YourModelKey llmReady={false} variant="outline" onDoneChange={() => {}} />);

    expect(await screen.findByText("••••••••••••")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Rotate" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Remove" })).toBeInTheDocument();
    expect(screen.getByText("Done")).toBeInTheDocument();
    expect(screen.getByText(/Your runs can use this key/)).toBeInTheDocument();
  });

  it("provided state: admin covers model access — the collapsed read-only view, revealed by 'Use my own key instead'", async () => {
    listSecretsMineMock.mockResolvedValue({ names: [], mine: [] });
    const user = userEvent.setup();
    render(<YourModelKey llmReady={true} variant="default" onDoneChange={() => {}} />);

    expect(await screen.findByText("Provided by your admin")).toBeInTheDocument();
    expect(screen.getByText("Model access is already configured for you.")).toBeInTheDocument();
    expect(screen.queryByPlaceholderText("sk-ant-…")).not.toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: "Use my own key instead" }));
    expect(screen.getByPlaceholderText("sk-ant-…")).toBeInTheDocument();
  });

  it("refused state: a 400 from the server sets the inline message and aria-invalid", async () => {
    listSecretsMineMock.mockResolvedValue({ names: [], mine: [] });
    setSecretMock.mockRejectedValue(new HttpError(400, "secret too short"));
    const user = userEvent.setup();
    render(<YourModelKey llmReady={false} variant="default" onDoneChange={() => {}} />);

    const input = await screen.findByPlaceholderText("sk-ant-…");
    await user.type(input, "short");
    await user.click(screen.getByRole("button", { name: "Save key" }));

    await waitFor(() => expect(screen.getByText("Keys shorter than 8 characters are refused.")).toBeInTheDocument());
    expect(input).toHaveAttribute("aria-invalid", "true");
  });

  it("reports done-ness to the caller (own key, or admin-provided) for the colour budget", async () => {
    listSecretsMineMock.mockResolvedValue({ names: ["anthropic-api-key"], mine: ["anthropic-api-key"] });
    const onDoneChange = vi.fn();
    render(<YourModelKey llmReady={false} variant="default" onDoneChange={onDoneChange} />);
    await waitFor(() => expect(onDoneChange).toHaveBeenCalledWith(true));
  });
});
