/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The rendering half of #541's page: connectionRowCopy/connectionsSummary's
// own state table is pinned in lib/model-connections.test.ts; this suite pins
// what the CARD does with that output — rows on screen, the summary chip in
// its header, and the door-claim (a row's own button must be the ONLY "Sign
// in to AWS" on the page, not a second one from the strip beside it).
import { it, expect, vi, beforeEach } from "vitest";
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

const loginPaneMock = vi.fn();
vi.mock("./harness-login-pane", () => ({
  HarnessLoginPane: (props: { startURLManaged?: boolean }) => {
    loginPaneMock(props);
    return <div data-testid="login-pane" />;
  },
}));

import { ModelConnectionsCard } from "./model-connections-card";
import { WithDoor } from "../../../../test/door-harness";
import { MODEL_PROVIDERS, baseStatus, providerStatus } from "../../../lib/test-fixtures";
import { CONNECTIONS } from "../../wardyn/copy/door";
import { AGENTS } from "../../../lib/workspace-providers-copy";
import type { SetupStatus } from "../../../lib/types";

function renderCard(status: SetupStatus, onChanged: () => void = vi.fn()) {
  return render(
    <WithDoor status={status} path="/account" operator={false}>
      <ModelConnectionsCard status={status} onChanged={onChanged} />
    </WithDoor>,
  );
}

beforeEach(() => {
  loginPaneMock.mockReset();
});

it("renders nothing with no provider block", () => {
  renderCard(baseStatus());
  expect(screen.queryByTestId("model-connections-card")).toBeNull();
});

it("a provider block with every provider disabled: the 'No providers' shape (rows.length === 0)", async () => {
  const s = providerStatus([{ provider: { ...MODEL_PROVIDERS.bedrock, disabled: true }, state: "not_configured" }]);
  renderCard(s);
  const card = await screen.findByTestId("model-connections-card");
  expect(within(card).getByText(CONNECTIONS.SUMMARY_NOT_SET_UP)).toBeInTheDocument();
  expect(within(card).queryByText(MODEL_PROVIDERS.bedrock.name!)).not.toBeInTheDocument();
});

it("renders the title, lede and summary chip, and one row per provider", async () => {
  const s = providerStatus([
    { provider: MODEL_PROVIDERS.bedrock, defaultFor: ["claude-code"], state: "live" },
    { provider: MODEL_PROVIDERS.anthropicKey, state: "not_configured" },
  ]);
  renderCard(s);
  const card = await screen.findByTestId("model-connections-card");
  expect(within(card).getByText(CONNECTIONS.TITLE)).toBeInTheDocument();
  expect(within(card).getByText(CONNECTIONS.LEDE)).toBeInTheDocument();
  expect(within(card).getByText(CONNECTIONS.SUMMARY_READY)).toBeInTheDocument();
  expect(within(card).getByText(MODEL_PROVIDERS.bedrock.name!)).toBeInTheDocument();
  expect(within(card).getByText(MODEL_PROVIDERS.anthropicKey.name!)).toBeInTheDocument();
  expect(within(card).getByText(CONNECTIONS.SIGNED_IN)).toBeInTheDocument();
  expect(within(card).getByText(CONNECTIONS.NO_KEY)).toBeInTheDocument();
});

it("Needs you when the default provider is unconnected", async () => {
  const s = providerStatus([{ provider: MODEL_PROVIDERS.bedrock, defaultFor: ["claude-code"], state: "not_configured" }]);
  renderCard(s);
  expect(await screen.findByText(CONNECTIONS.SUMMARY_NEEDS_YOU)).toBeInTheDocument();
});

// The behavior one-door.spec.ts pins live: without the claim, the strip's OWN
// per-provider line (providerAttention) ALSO renders on this page — nothing
// suppresses it here the way the legacy single-strip branch is suppressed on
// /account for an operator — so this card and the strip would show two
// buttons for the same provider.
it("claims the door: the strip's own line for the same provider does not also render a button", async () => {
  const s = providerStatus([{ provider: MODEL_PROVIDERS.bedrock, defaultFor: ["claude-code"], state: "not_configured" }]);
  renderCard(s);
  await screen.findByTestId("model-connections-card");
  expect(await screen.findAllByRole("button", { name: AGENTS.SIGN_IN_AWS })).toHaveLength(1);
});

it("clicking a row's button opens the shared door, keyed to that provider", async () => {
  const user = userEvent.setup();
  const s = providerStatus([{ provider: MODEL_PROVIDERS.bedrock, defaultFor: ["claude-code"], state: "not_configured" }]);
  renderCard(s);
  await user.click(await screen.findByRole("button", { name: AGENTS.SIGN_IN_AWS }));
  const dialog = await screen.findByRole("dialog", { name: AGENTS.SIGN_IN_AWS });
  expect(dialog).toHaveTextContent(`For ${MODEL_PROVIDERS.bedrock.name}`);
});

it("a key row's Replace button opens the key door with the stored state", async () => {
  const user = userEvent.setup();
  const s = providerStatus([{ provider: MODEL_PROVIDERS.anthropicKey, state: "live" }]);
  renderCard(s);
  await user.click(await screen.findByRole("button", { name: CONNECTIONS.REPLACE }));
  expect(await screen.findByRole("dialog", { name: `Add your key for ${MODEL_PROVIDERS.anthropicKey.name}` })).toBeInTheDocument();
});
