/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

/*
 * New Run, the guided flow, as the episodes film it: Start, then Shape, Run,
 * Harnesses, Files, Tools and access, Review, the Name dialog and the Launched
 * screen. Locators and filmed clicks only; the narration stays in each episode.
 *
 * A sibling module for the reason runs.ts gives: it is imported by the episodes
 * that film New Run and by none that do not. Every string is the copy module's.
 */

import { expect, type Locator, type Page } from "@playwright/test";
import { NR_FILES } from "../../src/app/components/wardyn/copy/new-run/files";
import { NR_HARNESSES } from "../../src/app/components/wardyn/copy/new-run/harnesses";
import { NR_LAUNCH } from "../../src/app/components/wardyn/copy/new-run/launch";
import { NR_REVIEW } from "../../src/app/components/wardyn/copy/new-run/review";
import { NR_RUN } from "../../src/app/components/wardyn/copy/new-run/run";
import { NR_SHAPE } from "../../src/app/components/wardyn/copy/new-run/shape";
import { NR_SHARED } from "../../src/app/components/wardyn/copy/new-run/shared";
import { NR_START } from "../../src/app/components/wardyn/copy/new-run/start";
import { NR_TOOLS } from "../../src/app/components/wardyn/copy/new-run/tools";
import { act } from "./overlay";

export type NewRunShape = "agent_task" | "environment" | "application";
export type NewRunMode = "interactive" | "autonomous";

const escapeRe = (s: string) => s.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
const startsWith = (s: string) => new RegExp(`^${escapeRe(s)}`);

/** Each screen's h1: focus moves to it on every screen change, so it is the "arrived" signal. */
export const NEW_RUN_HEADING = {
  start: NR_START.TITLE,
  shape: NR_SHAPE.TITLE,
  runAgent: NR_RUN.TITLE_AGENT,
  runEnvironment: NR_RUN.TITLE_ENV,
  runApplication: NR_RUN.TITLE_APP,
  harnesses: NR_HARNESSES.TITLE,
  files: NR_FILES.TITLE,
  tools: NR_TOOLS.TITLE,
  review: NR_REVIEW.TITLE,
} as const;

export const newRunHeading = (page: Page, title: string): Locator => page.getByRole("heading", { name: title, level: 1 });

const SHAPE_TITLE: Record<NewRunShape, string> = {
  agent_task: NR_SHAPE.AGENT_TITLE,
  environment: NR_SHAPE.ENV_TITLE,
  application: NR_SHAPE.APP_TITLE,
};

const RUN_HEADING: Record<NewRunShape, string> = {
  agent_task: NR_RUN.TITLE_AGENT,
  environment: NR_RUN.TITLE_ENV,
  application: NR_RUN.TITLE_APP,
};

/** Step 1's workload buttons: each is named by its title, and choosing one opens the Run step. */
export const shapeCard = (page: Page, shape: NewRunShape): Locator => page.getByRole("button", { name: SHAPE_TITLE[shape], exact: true });

/** A workload button, filmed, then the Run step's heading as the arrived signal. */
export async function chooseShape(page: Page, shape: NewRunShape, text?: string): Promise<void> {
  await act(page, shapeCard(page, shape), text);
  await expect(newRunHeading(page, RUN_HEADING[shape])).toBeVisible();
}

/** Step 2's mode cards (radios), for an agent task. */
export const modeCard = (page: Page, mode: NewRunMode): Locator =>
  page.getByRole("radio", { name: startsWith(mode === "autonomous" ? NR_RUN.MODE_AUTONOMOUS : NR_RUN.MODE_INTERACTIVE) });

/** Step 2's command field, for an Application sandbox. */
export const commandField = (page: Page): Locator => page.getByLabel(NR_RUN.COMMAND_LABEL);

/** Step 3: the Nth harness card's picker. */
export const harnessPicker = (page: Page, index = 0): Locator => page.locator(`#nr-harness-${index}`);
/** An Autonomous run's task, and an Interactive run's starting prompt (after "Start the run in this harness"). */
export const taskField = (page: Page): Locator => page.locator("#nr-task");
export const startInHarness = (page: Page): Locator => page.getByRole("switch", { name: NR_HARNESSES.START_IN_HARNESS });
export const startingPrompt = (page: Page, index = 0): Locator => page.locator(`#nr-text-${index}`);

/** Step 4: "No extra files", and the Add dialog: the Add button, a type, a source, Continue and Add. */
export const noExtraFiles = (page: Page): Locator => page.getByRole("radio", { name: startsWith(NR_FILES.NO_EXTRA) });
export const addFilesButton = (page: Page): Locator => page.getByRole("button", { name: NR_FILES.ADD_ARIA });
export const addDialog = (page: Page): Locator => page.getByRole("dialog");
export const sourceTypeButton = (page: Page, type: string): Locator => addDialog(page).getByRole("button", { name: startsWith(type) });
export const sourceRadio = (page: Page, name: string): Locator => addDialog(page).getByRole("radio", { name: new RegExp(`^${escapeRe(name)}(\\s|$)`) });
export const dialogContinue = (page: Page): Locator => addDialog(page).getByRole("button", { name: NR_SHARED.CONTINUE, exact: true });
export const dialogAdd = (page: Page): Locator => addDialog(page).getByRole("button", { name: NR_FILES.ADD_CONFIRM, exact: true });
/** A drive's access select on the Mount path step: "edit" or "read". */
export const mountAccess = (page: Page): Locator => addDialog(page).locator("#nr-mount-access");
/** An attached item's card, optionally the one whose text holds `name`. */
export const attachCard = (page: Page, name?: string): Locator => {
  const cards = page.getByTestId("nr-attach-card");
  return name === undefined ? cards : cards.filter({ hasText: name });
};

/** Adds one repository through the dialog, filmed: Add (with `text`), the type, the repository, Continue, Add. The first thing added is where the run starts. */
export async function addRepository(page: Page, name: string, text?: string): Promise<void> {
  await act(page, addFilesButton(page), text);
  await act(page, sourceTypeButton(page, NR_FILES.REPOSITORY_BADGE));
  await act(page, sourceRadio(page, name));
  await act(page, dialogContinue(page));
  await act(page, dialogAdd(page));
  await expect(addDialog(page)).toBeHidden();
}

/** Adds one folder on this machine through the dialog, filmed: Add (with `text`), the Folder type, the workspace's folder, Continue, Add. */
export async function addFolder(page: Page, name: string, text?: string): Promise<void> {
  await act(page, addFilesButton(page), text);
  await act(page, sourceTypeButton(page, NR_FILES.FOLDER_TITLE));
  await act(page, sourceRadio(page, name));
  await act(page, dialogContinue(page));
  await act(page, dialogAdd(page));
  await expect(addDialog(page)).toBeHidden();
}

/** Step 5: a harness's model-provider picker. */
export const providerPicker = (page: Page, harness: string): Locator => page.locator(`#nr-provider-${harness}`);

/** Picks the first connected provider by its label (the option after the "Choose a model provider" placeholder), so a reordered list cannot change which one is chosen silently. */
export async function chooseProvider(picker: Locator, timeout?: number): Promise<void> {
  await expect(picker).toBeEnabled({ timeout });
  await expect.poll(async () => (await picker.locator("option").allTextContents()).filter((t) => t.trim() !== NR_TOOLS.PROVIDER_CHOOSE).length, { timeout, message: "no model provider is offered for this harness" }).toBeGreaterThan(0);
  const label = (await picker.locator("option").allTextContents()).map((t) => t.trim()).find((t) => t !== NR_TOOLS.PROVIDER_CHOOSE);
  await picker.selectOption({ label });
}

/** Review's Launch run, the Name dialog and its fields and buttons. */
export const launchRunButton = (page: Page): Locator => page.getByRole("button", { name: NR_REVIEW.LAUNCH_RUN });
export const nameDialog = (page: Page): Locator => page.getByRole("dialog", { name: NR_LAUNCH.TITLE });
export const runTitleField = (page: Page): Locator => nameDialog(page).getByLabel(NR_LAUNCH.TITLE_LABEL);
export const runDescriptionField = (page: Page): Locator => nameDialog(page).getByLabel(NR_LAUNCH.DESCRIPTION_LABEL);
export const confirmLaunchButton = (page: Page): Locator => nameDialog(page).getByRole("button", { name: NR_LAUNCH.LAUNCH, exact: true });
export const viewRunButton = (page: Page): Locator =>
  page.getByRole("button", { name: NR_LAUNCH.VIEW_RUN }).or(page.getByRole("link", { name: NR_LAUNCH.VIEW_RUN }));

/** The footer's one primary action: "Continue to <step>", or "Review run" from the last step. */
export const primaryAction = (page: Page): Locator =>
  page.getByRole("button", { name: new RegExp(`^(${escapeRe(NR_SHARED.CONTINUE_TO(""))}.+|${escapeRe(NR_SHARED.REVIEW_RUN)})$`) });

/** Start's "Start guided setup": a filmed click, then Shape. */
export async function startGuided(page: Page, text?: string): Promise<void> {
  await act(page, page.getByRole("button", { name: NR_START.GUIDED_FOOT }), text);
  await expect(newRunHeading(page, NEW_RUN_HEADING.shape)).toBeVisible();
}

/** The footer's primary action, filmed, then the next screen's heading as the arrived signal. */
export async function nextStep(page: Page, nextHeading: string, text?: string): Promise<void> {
  await act(page, primaryAction(page), text);
  await expect(newRunHeading(page, nextHeading)).toBeVisible();
}

/** Review's Launch run, filmed, to the Name dialog. */
export async function openNameDialog(page: Page, text?: string): Promise<void> {
  await act(page, launchRunButton(page), text);
  await expect(nameDialog(page)).toBeVisible();
}
