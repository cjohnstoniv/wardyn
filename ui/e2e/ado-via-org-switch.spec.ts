/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// An Azure DevOps connection's "Allow on runners via the server" switch, and the posture rule it leans on.
// The document and the PUT are spliced here (the hermetic backend runs no Entra tenant), so the spec owns what
// the browser draws and what it sends: runner_via_org true when on and absent when off, and Save held while a
// minimum typed under Store on the runner is carried to Keep in the organisation for the Azure DevOps tokens.
import type { Page } from "@playwright/test";
import { test, expect, asRealSecurityAdmin, gotoConsole, navTo, navToRoute } from "./fixtures";
import { ADO_ENTRA_EDITOR, PROVIDERS } from "../src/app/lib/workspace-providers-copy";
import { DELIVERY } from "../src/app/lib/governance-copy";

const TENANT = "8f14e45f-ceea-4d2c-a3f9-1a2b3c4d5e6f";
const CLIENT = "3b241101-e2bb-4255-8caf-4136c566a962";

const minted = {
  id: "azure_devops",
  kind: "azure_devops",
  base_urls: ["https://dev.azure.com/wardyn-e2e"],
  lanes: ["entra"],
  credential_source: "per_user",
  entra: { tenant_id: TENANT, client_id: CLIENT, token_mode: "minted_pat", capability_ceiling: ["project_read", "code_read"], default_profile: ["code_read"] },
};

test.describe("Azure DevOps via the server", () => {
  test("the switch is off by default, saves runner_via_org true when on, and nothing when off again", async ({ page }: { page: Page }) => {
    let sent: { git: { entra: Record<string, unknown> }[] } | null = null;
    await page.route("**/api/v1/workspace-providers", async (route) => {
      const req = route.request();
      if (req.method() === "GET") return route.fulfill({ json: { git: [minted] }, headers: { etag: '"e2e-via-1"' } });
      if (req.method() === "PUT") {
        sent = req.postDataJSON();
        return route.fulfill({ json: { git: sent!.git, sources_no_longer_admitted: 0 }, headers: { etag: '"e2e-via-2"' } });
      }
      return route.fallback();
    });
    await gotoConsole(page);
    await navToRoute(page, "/admin/settings");
    await page.getByTestId("providers-card").getByRole("link").click();
    await expect(page.getByRole("heading", { name: PROVIDERS.TITLE, level: 1 })).toBeVisible();

    const row = page.getByTestId("provider-row-azure_devops");
    const via = row.getByRole("switch", { name: ADO_ENTRA_EDITOR.VIA_ORG_TOGGLE });
    await expect(via).toHaveAttribute("aria-checked", "false");
    await expect(row.getByText(ADO_ENTRA_EDITOR.VIA_ORG_TOGGLE_HINT)).toBeVisible();

    await via.click();
    await page.getByRole("button", { name: PROVIDERS.SAVE_CTA }).click();
    await expect(page.getByText(PROVIDERS.SAVED_TOAST)).toBeVisible();
    expect(sent!.git[0].entra.runner_via_org).toBe(true);
    await page.getByRole("listitem").filter({ hasText: PROVIDERS.SAVED_TOAST }).getByRole("button", { name: "Close toast" }).click();
    await expect(page.getByText(PROVIDERS.SAVED_TOAST)).toHaveCount(0);

    await via.click();
    await page.getByRole("button", { name: PROVIDERS.SAVE_CTA }).click();
    await expect(page.getByText(PROVIDERS.SAVED_TOAST)).toBeVisible();
    expect(sent!.git[0].entra).not.toHaveProperty("runner_via_org");
  });

  test("a minimum that is not a version, typed under the runner, still holds Save once the Azure DevOps tokens move to the organisation", async ({ page }: { page: Page }) => {
    await page.route("**/api/v1/governance/credential-delivery", (route) =>
      route.request().method() === "GET"
        ? route.fulfill({
            json: { policy: { classes: {} }, updated_at: "", updated_by: "", buildable_modes: { ado_minted_pat: ["via_org", "runner_resident", "refuse"] } },
            headers: { ETag: '"e2e"' },
          })
        : route.fallback(),
    );
    await asRealSecurityAdmin(page);
    await gotoConsole(page, "admin");
    await navTo(page, "Governance");
    await page.getByRole("tab", { name: DELIVERY.TAB }).click();

    const ado = page.getByRole("radiogroup", { name: DELIVERY.CLASS.ado_minted_pat });
    await ado.getByRole("radio", { name: DELIVERY.MODE.runner_resident }).click();
    const field = page.getByLabel(DELIVERY.OS_MIN(DELIVERY.OS_NAME.windows));
    await field.fill("latest");
    await expect(field).toHaveAttribute("aria-invalid", "true");
    await ado.getByRole("radio", { name: DELIVERY.MODE.via_org }).click();
    await expect(field).toHaveCount(0);
    await expect(page.getByRole("button", { name: DELIVERY.SAVE, exact: true })).toBeDisabled();
  });
});
