/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The Azure Foundry door (011-foundry-m7 F13): the dialog's strings, and Continue to Microsoft sending the browser
// to the provider row's start route under the console's base path, with the row's uid.
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { ADO } from "../../lib/ado-entra-copy";
import { providerStatus } from "../../lib/test-fixtures";
import type { SetupModelProvider } from "../../lib/types";
import { WithDoor } from "../../../test/door-harness";
import { useModelAccessDoor } from "./model-access-context";
import { AZURE_DOOR, KEY_DOOR } from "./copy/door";

vi.mock("../screens/settings/harness-login-pane", () => ({ HarnessLoginPane: () => null }));

const AZURE: SetupModelProvider = {
  id: "azure-responses",
  uid: "u-az 1",
  name: "Azure (responses)",
  kind: "azure_foundry",
  harnesses: ["codex-cli"],
  host: "my-resource.openai.azure.com",
};

function Entrance() {
  const door = useModelAccessDoor();
  return (
    <button type="button" onClick={() => door.openDoor({ for: { provider: AZURE.id } })}>
      entrance
    </button>
  );
}

let assign: ReturnType<typeof vi.fn>;
const realLocation = window.location;
beforeEach(() => {
  assign = vi.fn();
  Object.defineProperty(window, "location", { configurable: true, value: { ...realLocation, assign } });
});
afterEach(() => {
  Object.defineProperty(window, "location", { configurable: true, value: realLocation });
  vi.restoreAllMocks();
});

async function openDoor() {
  const user = userEvent.setup();
  render(
    <WithDoor status={providerStatus([{ provider: AZURE, defaultFor: ["codex-cli"] }])}>
      <Entrance />
    </WithDoor>,
  );
  await user.click(screen.getByRole("button", { name: "entrance" }));
  return user;
}

describe("the Azure Foundry door", () => {
  it("draws the title, the body and the note for the row, and no key field", async () => {
    await openDoor();
    const dialog = await screen.findByRole("dialog", { name: AZURE_DOOR.TITLE("Azure (responses)") });
    expect(dialog).toHaveTextContent(AZURE_DOOR.BODY("Azure (responses)"));
    expect(dialog).toHaveTextContent(AZURE_DOOR.NOTE);
    expect(screen.queryByLabelText(KEY_DOOR.FIELD(false))).toBeNull();
    expect(screen.getByRole("button", { name: "Continue to Microsoft" })).toBeEnabled();
    expect(ADO.CONNECT_CTA).toBe("Continue to Microsoft");
  });

  it("Continue sends the browser to the row's start route, with the base path and the uid", async () => {
    const user = await openDoor();
    await user.click(await screen.findByRole("button", { name: ADO.CONNECT_CTA }));
    expect(assign).toHaveBeenCalledTimes(1);
    expect(String(assign.mock.calls[0][0])).toMatch(/\/api\/v1\/model-providers-entra\/signin\?uid=u-az%201$/);
    expect(screen.getByRole("button", { name: ADO.CONNECT_CTA })).toBeDisabled();
  });

  it("Cancel closes it without leaving", async () => {
    const user = await openDoor();
    await user.click(await screen.findByRole("button", { name: KEY_DOOR.CANCEL }));
    expect(assign).not.toHaveBeenCalled();
  });
});
