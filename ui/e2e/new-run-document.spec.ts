/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// New Run, the run document path: YAML / JSON from Start, the editor, the
// server's own validation (POST /templates/import, kind Run), the canon error
// lines, Open in Guided setup and Continue to review.
//
// A valid document goes through the real import door. A document the server
// refuses is stubbed, so the diagnostic is the one this spec names.
import type { Page } from "@playwright/test";
import { test, expect, gotoConsole, navToRoute } from "./fixtures";
import { NR_DOCUMENT } from "../src/app/components/wardyn/copy/new-run/document";
import { NR_REVIEW } from "../src/app/components/wardyn/copy/new-run/review";
import { NR_SHAPE } from "../src/app/components/wardyn/copy/new-run/shape";
import { NR_START } from "../src/app/components/wardyn/copy/new-run/start";
import { SCENARIO_PREFLIGHT } from "../src/app/lib/new-run-fixtures/scenarios/preflight";

const editor = (page: Page, format: "YAML" | "JSON" = "YAML") => page.getByRole("textbox", { name: NR_DOCUMENT.EDITOR_ARIA(format) });
const panel = (page: Page) => page.getByTestId("validation-panel");
const press = (page: Page, name: string) => page.getByRole("button", { name, exact: true }).click();

async function openDocument(page: Page, format: "YAML" | "JSON" = "YAML") {
  await gotoConsole(page);
  await navToRoute(page, "/runs/new");
  await expect(page.getByRole("heading", { name: NR_START.TITLE, level: 1 })).toBeVisible();
  await press(page, format === "YAML" ? NR_START.DOC_YAML : NR_START.DOC_JSON);
  await expect(page.getByRole("heading", { name: NR_DOCUMENT.TITLE })).toBeVisible();
  await expect(editor(page, format)).toBeVisible();
}

test.describe("New Run, the run document", () => {
  test("Start opens the editor in the chosen format, ready and unvalidated", async ({ page }) => {
    await openDocument(page, "JSON");
    await expect(page).toHaveURL(/step=document&format=json/);
    await expect(panel(page)).toHaveAttribute("data-status", "ready");
    await expect(panel(page).getByText(NR_DOCUMENT.READY)).toBeVisible();
  });

  test("a document that does not parse says so in the canon line, without asking the server", async ({ page }) => {
    let asked = false;
    await page.route("**/api/v1/templates/import", (route) => {
      asked = true;
      return route.fallback();
    });
    await openDocument(page);
    await editor(page).fill("shape: [agent_task");
    await press(page, NR_DOCUMENT.VALIDATE);
    await expect(panel(page)).toHaveAttribute("data-status", "issues");
    await expect(panel(page).getByText(NR_DOCUMENT.ISSUES(1))).toBeVisible();
    await expect(panel(page).getByText(NR_DOCUMENT.YAML_INVALID)).toBeVisible();
    expect(asked).toBe(false);

    // The same for JSON, in its own line.
    await page.getByTestId("document-file").setInputFiles({ name: "run.json", mimeType: "application/json", buffer: Buffer.from('{"shape": ') });
    await expect(editor(page, "JSON")).toBeVisible();
    await press(page, NR_DOCUMENT.VALIDATE);
    await expect(panel(page).getByText(NR_DOCUMENT.JSON_INVALID)).toBeVisible();
    expect(asked).toBe(false);
  });

  test("the server's own diagnostics are counted and shown beside the text, and nothing continues", async ({ page }) => {
    const message = "shape must be agent_task, environment or application";
    await page.route("**/api/v1/templates/import", (route) =>
      route.fulfill({ json: { diagnostics: [{ path: "intent.shape", reason: "invalid_value", message }] } }),
    );
    await openDocument(page);
    await editor(page).fill("api_version: wardyn/v1\nkind: Run\nintent:\n  shape: nonsense\n");
    await press(page, NR_DOCUMENT.VALIDATE);
    await expect(panel(page).getByText(NR_DOCUMENT.ISSUES(1))).toBeVisible();
    await expect(page.getByText(message)).toBeVisible();

    await press(page, NR_DOCUMENT.CONTINUE_REVIEW);
    await expect(page.getByRole("heading", { name: NR_DOCUMENT.TITLE })).toBeVisible();
    await expect(page).toHaveURL(/step=document/);
  });

  test("Load example, Validate and Continue to review reach Review with the document's run", async ({ page }) => {
    await page.route("**/api/v1/runs/preflight", (route) => route.fulfill({ json: SCENARIO_PREFLIGHT.member }));
    await openDocument(page);
    await press(page, NR_DOCUMENT.LOAD_EXAMPLE);
    await press(page, NR_DOCUMENT.VALIDATE);
    await expect(panel(page)).toHaveAttribute("data-status", "valid");
    await expect(panel(page).getByText(NR_DOCUMENT.VALID)).toBeVisible();

    await press(page, NR_DOCUMENT.CONTINUE_REVIEW);
    await expect(page.getByRole("heading", { name: NR_REVIEW.TITLE })).toBeVisible();
    await expect(page.getByText(NR_REVIEW.SETUP_AGENT_INTERACTIVE)).toBeVisible();
  });

  test("Open in Guided setup carries the valid document into the Shape step", async ({ page }) => {
    await openDocument(page);
    await press(page, NR_DOCUMENT.LOAD_EXAMPLE);
    await press(page, NR_DOCUMENT.OPEN_IN_GUIDED);
    await expect(page.getByRole("heading", { name: NR_SHAPE.TITLE })).toBeVisible();
    await expect(page.getByRole("radio", { name: NR_SHAPE.AGENT_TITLE })).toBeChecked();
  });

  test("Import file loads a document and takes its format from the file name", async ({ page }) => {
    await openDocument(page);
    const json = JSON.stringify({ api_version: "wardyn/v1", kind: "Run", intent: { shape: "environment", experience: "interactive" } });
    await page.getByTestId("document-file").setInputFiles({ name: "env.json", mimeType: "application/json", buffer: Buffer.from(json) });
    await expect(editor(page, "JSON")).toHaveValue(json);
    await expect(page).toHaveURL(/format=json/);
  });
});
