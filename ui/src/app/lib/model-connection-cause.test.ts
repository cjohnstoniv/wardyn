/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */
import { expect, it } from "vitest";
import { modelConnectionCause } from "./model-connection-cause";
import { connectionRowCopy } from "./model-connections";
import { MODEL_PROVIDERS } from "./test-fixtures";
import { CONNECTIONS } from "../components/wardyn/copy/door";

const provider = MODEL_PROVIDERS.gateway;

it.each(["destination_changed", "kind_changed"])("%s shows the disclosed destination and requires review", (cause) => {
  const access = { provider: provider.id, state: "not_configured", cause, new_destination: "new.example" };
  const copy = connectionRowCopy(null, { provider, access });
  expect(copy.line).toContain("new.example");
  expect(copy.line).toContain("removed because an admin changed");
  expect(copy.button).toBe(CONNECTIONS.REVIEW_RECONNECT);
});

it("transient read failure says could not check and offers re-check", () => {
  const access = { provider: provider.id, state: "not_configured", cause: "store_unreadable" };
  const copy = connectionRowCopy(null, { provider, access });
  expect(copy.line).toBe(CONNECTIONS.STORE_UNREADABLE);
  expect(copy.chip.label).toBe(CONNECTIONS.COULD_NOT_CHECK);
  expect(copy.button).toBe(CONNECTIONS.RECHECK);
});

it("confirmed absence offers a first connection with explicit destination", () => {
  const access = { provider: provider.id, state: "not_configured", cause: "never_connected" };
  expect(connectionRowCopy(null, { provider, access }).line).toBe(CONNECTIONS.NEVER_CONNECTED(provider.host, provider.name || provider.id));
  expect(connectionRowCopy(null, { provider, access }).button).toBe(CONNECTIONS.ADD_TOKEN);
});

it.each([undefined, "future_cause"])("old wire or unknown cause %s retains neutral copy", (cause) => {
  const access = { provider: provider.id, state: "not_configured", cause };
  expect(modelConnectionCause(provider, access)).toBeUndefined();
  expect(connectionRowCopy(null, { provider, access }).chip.label).toBe(CONNECTIONS.NO_TOKEN);
});

it("reconnected rows show no stale history", () => {
  expect(modelConnectionCause(provider, { provider: provider.id, state: "live", cause: "destination_changed" })).toBeUndefined();
});
