/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import { HttpError } from "../../../lib/api/core";
import { expandCard } from "../../../lib/test-dom";
import { STATES } from "../../wardyn/states";

const listKeysMock = vi.fn();
vi.mock("../../../lib/api/ssh-keys", () => ({
  sshKeys: { listKeys: () => listKeysMock(), addKey: vi.fn(), deleteKey: vi.fn() },
}));

import { AdminSshKeysCard, ADMIN_SSH_KEYS } from "./admin-ssh-keys-card";

describe("AdminSshKeysCard: a failed load", () => {
  it("shows the server's sentence, not the default line", async () => {
    listKeysMock.mockRejectedValue(new HttpError(500, "Your sentence from the server.", "ssh_keys_failed"));
    render(<AdminSshKeysCard />);
    await expandCard(ADMIN_SSH_KEYS.TITLE);
    expect(await screen.findByText("Your sentence from the server.")).toBeInTheDocument();
    expect(screen.queryByText(STATES.ERROR_DEFAULT)).toBeNull();
  });

  it("a plain network error still shows the default line", async () => {
    listKeysMock.mockRejectedValue(new Error("Failed to fetch"));
    render(<AdminSshKeysCard />);
    await expandCard(ADMIN_SSH_KEYS.TITLE);
    expect(await screen.findByText(STATES.ERROR_DEFAULT)).toBeInTheDocument();
  });
});
