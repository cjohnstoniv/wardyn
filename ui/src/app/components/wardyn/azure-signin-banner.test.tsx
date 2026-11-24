/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The Azure Foundry sign-in's landing banner (011-foundry-m7 F14, F15): connected names the row by its uid, a known
// refusal reads as the Azure DevOps banner's sentence, an unknown one as its exchange_failed, and the three
// parameters are gone from the address afterwards.
import { afterEach, describe, expect, it } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { ADO_SIGNIN } from "./copy/ado-signin-error-copy";
import { AZURE_DOOR } from "./copy/door";
import { AzureSignInBanner } from "./azure-signin-banner";
import { WithDoor } from "../../../test/door-harness";
import { providerStatus } from "../../lib/test-fixtures";
import type { SetupModelProvider } from "../../lib/types";

const AZURE: SetupModelProvider = {
  id: "azure-responses",
  uid: "u-az-1",
  name: "Azure (responses)",
  kind: "azure_foundry",
  harnesses: ["codex-cli"],
  host: "my-resource.openai.azure.com",
};

function land(search: string) {
  window.history.pushState({}, "", `/${search}`);
  render(
    <WithDoor status={providerStatus([{ provider: AZURE }])}>
      <AzureSignInBanner />
    </WithDoor>,
  );
}
afterEach(() => window.history.pushState({}, "", "/"));

describe("the Azure Foundry sign-in banner", () => {
  it("connected: names the row looked up by uid, and clears the parameters", async () => {
    land("?azure_signin=connected&azure_provider=u-az-1&keep=1");
    expect(await screen.findByText(AZURE_DOOR.CONNECTED("Azure (responses)"))).toBeInTheDocument();
    expect(window.location.search).toBe("?keep=1");
  });

  it("connected for a uid this person cannot read says nothing", () => {
    land("?azure_signin=connected&azure_provider=u-other");
    expect(screen.queryByText(/is connected/)).toBeNull();
    expect(window.location.search).toBe("");
  });

  it("a known refusal reads as its sentence, with a retry to the row's start and a Dismiss", async () => {
    land("?azure_signin_error=consent_required&azure_provider=u-az-1");
    expect(await screen.findByText(ADO_SIGNIN.REASON.consent_required)).toBeInTheDocument();
    const retry = screen.getByRole("link", { name: ADO_SIGNIN.RETRY });
    expect(retry.getAttribute("href")).toMatch(/\/model-providers-entra\/signin\?uid=u-az-1$/);
    expect(window.location.search).toBe("");
    await userEvent.click(screen.getByRole("button", { name: "Dismiss" }));
    expect(screen.queryByText(ADO_SIGNIN.REASON.consent_required)).toBeNull();
  });

  it("an unknown refusal reads as the exchange_failed sentence", async () => {
    land("?azure_signin_error=something_new&azure_provider=u-az-1");
    expect(await screen.findByText(ADO_SIGNIN.REASON.exchange_failed)).toBeInTheDocument();
  });

  it("says nothing with none of the parameters", () => {
    land("?other=1");
    expect(screen.queryByRole("button", { name: "Dismiss" })).toBeNull();
    expect(window.location.search).toBe("?other=1");
  });
});
