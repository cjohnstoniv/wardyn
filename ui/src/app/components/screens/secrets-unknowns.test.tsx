/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// What the Secrets page holds back while it does not know something: where a model key goes on a deployment with no
// model providers, and whose list it is showing.

import { describe, it, expect, vi, beforeEach } from "vitest";
import { act, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

const listScopedMock = vi.fn();
vi.mock("../../lib/api/secrets", () => ({
  secrets: {
    listScoped: (scope: string) => listScopedMock(scope),
    listSecretsMine: () => Promise.resolve({ names: [], mine: [] }),
  },
}));
vi.mock("../../lib/api/providers", () => ({
  providers: { getWorkspaceProviders: () => Promise.reject(new Error("not mocked")) },
}));

import { SecretsScreen, AddSecretDialog } from "./secrets";
import { OperatorProvider } from "../wardyn/operator-context";
import { ModelAccessProvider } from "../wardyn/model-access-context";
import { SECRETS_PAGE } from "../wardyn/copy/components";
import type { SetupStatus } from "../../lib/types";

describe("AddSecretDialog: the User view's model key pointer", () => {
  it.each([
    ["the deployment has no provider block", {} as unknown as SetupStatus],
    ["the deployment's setup is not known yet", null],
  ])("points nowhere when %s: the model connections card is not on the page", (_case, status) => {
    render(
      <MemoryRouter initialEntries={["/secrets"]}>
        <ModelAccessProvider status={status} onRefresh={() => {}}>
          <AddSecretDialog open onOpenChange={() => {}} ownRows />
        </ModelAccessProvider>
      </MemoryRouter>,
    );
    expect(screen.getByLabelText(/^name/i)).not.toHaveAccessibleDescription();
    expect(screen.queryByText(SECRETS_PAGE.MODEL_KEYS_HINT.USER)).toBeNull();
    expect(screen.queryByText(SECRETS_PAGE.MODEL_KEYS_HINT.ADMIN)).toBeNull();
    // The suggestions are still offered; only the pointer is withheld.
    expect(screen.getByRole("button", { name: "kubeconfig" })).toBeInTheDocument();
  });
});

describe("SecretsScreen: a list that answers late", () => {
  beforeEach(() => listScopedMock.mockReset());

  it("a list that answers after this tab stopped knowing who is signed in turns no Add on", async () => {
    let answer!: (names: string[]) => void;
    listScopedMock.mockReturnValue(new Promise<string[]>((resolve) => (answer = resolve)));
    const tree = (resolved: boolean) => (
      <OperatorProvider operator operatorResolved={resolved} principal="sam@example.com">
        <SecretsScreen />
      </OperatorProvider>
    );
    const { rerender } = render(tree(true));
    await waitFor(() => expect(listScopedMock).toHaveBeenCalledTimes(1));
    rerender(tree(false));
    // The read was already on its way; an empty answer brings up the empty state, which has an Add of its own.
    await act(async () => answer([]));
    await screen.findByText("No secrets yet.");
    const adds = screen.getAllByRole("button", { name: /add (your first )?secret/i });
    expect(adds).toHaveLength(2);
    for (const add of adds) expect(add).toBeDisabled();
  });
});
