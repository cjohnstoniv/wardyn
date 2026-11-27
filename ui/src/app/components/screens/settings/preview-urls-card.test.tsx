/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The preview URLs card: the site config's `features.preview_urls` (the switch and the widest a preview can be).
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { expandCard } from "../../../lib/test-dom";
import { FORM_ACTIONS } from "../../wardyn/copy/form-actions";
import { PREVIEW_URLS as T } from "../../wardyn/copy/preview-urls";
import { RUN_PREVIEWS } from "../../wardyn/copy/run-previews";
import { COMPONENTS_ADMIN as C } from "../../wardyn/copy/components-admin";
import { PROVIDERS } from "../../../lib/workspace-providers-copy";

const toastSuccess = vi.fn();
const toastError = vi.fn();
vi.mock("sonner", () => ({ toast: { success: (...a: unknown[]) => toastSuccess(...a), error: (...a: unknown[]) => toastError(...a) } }));

const snapshotMock = vi.fn();
const putSiteConfigMock = vi.fn();
vi.mock("../../../lib/api/health", () => ({
  health: {
    getSiteConfigSnapshot: () => snapshotMock(),
    putSiteConfig: (...a: unknown[]) => putSiteConfigMock(...a),
  },
}));

import { HttpError } from "../../../lib/api/core";
import { PreviewURLsCard } from "./preview-urls-card";

async function open() {
  render(<PreviewURLsCard />);
  await expandCard(T.TITLE);
  return screen.getByTestId("preview-urls-card");
}
const sw = (card: HTMLElement) => within(card).getByRole("switch", { name: T.LABEL });
const radio = (card: HTMLElement, name: string) => within(card).getByRole("radio", { name });
const saveBtn = (card: HTMLElement) => within(card).getByRole("button", { name: FORM_ACTIONS.SAVE });
const saved = (cfg: object) =>
  Promise.resolve({ siteConfig: cfg, etag: '"v2"', danglingSecretRefs: [], onboardingCompletedAtIgnored: false, appliesFrom: "", sourcesNoLongerAdmitted: null });

beforeEach(() => {
  toastSuccess.mockClear();
  toastError.mockClear();
  snapshotMock.mockReset().mockResolvedValue({ siteConfig: { upstream_proxy_url: "http://p" }, etag: '"v1"' });
  putSiteConfigMock.mockReset().mockImplementation(saved);
});

describe("PreviewURLsCard", () => {
  it("is loading until the read answers, with no switch", async () => {
    snapshotMock.mockReturnValue(new Promise(() => {}));
    render(<PreviewURLsCard />);
    await expandCard(T.TITLE);
    expect(screen.queryByRole("switch")).toBeNull();
  });

  it("a failed read says so and offers Retry, which reads again", async () => {
    snapshotMock.mockRejectedValueOnce(new Error("down")).mockResolvedValueOnce({ siteConfig: { features: { preview_urls: { enabled: true } } }, etag: '"v1"' });
    const card = await open();
    expect(await within(card).findByText(C.SETTINGS_LOAD_FAILED)).toBeInTheDocument();
    await userEvent.click(within(card).getByRole("button", { name: C.SETTINGS_RETRY }));
    expect(await within(card).findByRole("switch", { name: T.LABEL })).toBeChecked();
  });

  it("shows the label, the hint, both ceilings and the honest note; off with Private when nothing is stored; Save held", async () => {
    const card = await open();
    expect(await within(card).findByRole("switch", { name: T.LABEL })).not.toBeChecked();
    expect(sw(card)).toHaveAccessibleDescription(T.HINT);
    expect(within(card).getByText(T.CEILING_LABEL)).toBeInTheDocument();
    expect(radio(card, RUN_PREVIEWS.PRIVATE)).toBeChecked();
    expect(radio(card, RUN_PREVIEWS.PRIVATE)).toHaveAccessibleDescription(T.CEILING_PRIVATE);
    expect(radio(card, RUN_PREVIEWS.ORG)).toHaveAccessibleDescription(T.CEILING_ORG);
    expect(within(card).queryByRole("radio", { name: RUN_PREVIEWS.PUBLIC })).toBeNull();
    expect(within(card).getByText(T.HONEST)).toBeInTheDocument();
    expect(saveBtn(card)).toBeDisabled();
  });

  it("dims the ceiling while the switch is off, and keeps its value", async () => {
    snapshotMock.mockResolvedValue({ siteConfig: { features: { preview_urls: { enabled: false, max_visibility: "org" } } }, etag: '"v1"' });
    const card = await open();
    await within(card).findByRole("switch", { name: T.LABEL });
    expect(radio(card, RUN_PREVIEWS.ORG)).toBeChecked();
    expect(radio(card, RUN_PREVIEWS.ORG)).toBeDisabled();
    await userEvent.click(sw(card));
    expect(radio(card, RUN_PREVIEWS.ORG)).toBeEnabled();
    expect(radio(card, RUN_PREVIEWS.ORG)).toBeChecked();
  });

  it("reads the stored switch and ceiling", async () => {
    snapshotMock.mockResolvedValue({ siteConfig: { features: { preview_urls: { enabled: true, max_visibility: "org" } } }, etag: '"v1"' });
    const card = await open();
    expect(await within(card).findByRole("switch", { name: T.LABEL })).toBeChecked();
    expect(radio(card, RUN_PREVIEWS.ORG)).toBeChecked();
  });

  it("saves with the ETag as If-Match, writing both keys and keeping every other feature and the rest of the document", async () => {
    snapshotMock.mockResolvedValue({ siteConfig: { upstream_proxy_url: "http://p", features: { run_move: { enabled: true } } }, etag: '"v1"' });
    const card = await open();
    await userEvent.click(await within(card).findByRole("switch", { name: T.LABEL }));
    await userEvent.click(radio(card, RUN_PREVIEWS.ORG));
    await userEvent.click(saveBtn(card));
    await waitFor(() => expect(putSiteConfigMock).toHaveBeenCalledTimes(1));
    expect(putSiteConfigMock).toHaveBeenCalledWith(
      { upstream_proxy_url: "http://p", features: { run_move: { enabled: true }, preview_urls: { enabled: true, max_visibility: "org" } } },
      '"v1"',
    );
    expect(toastSuccess).toHaveBeenCalledWith(T.SAVED_TOAST);
    expect(saveBtn(card)).toBeDisabled();
  });

  it("saving off sends enabled false and keeps the ceiling", async () => {
    snapshotMock.mockResolvedValue({ siteConfig: { features: { preview_urls: { enabled: true, max_visibility: "org" } } }, etag: '"v1"' });
    const card = await open();
    await userEvent.click(await within(card).findByRole("switch", { name: T.LABEL }));
    await userEvent.click(saveBtn(card));
    await waitFor(() => expect(putSiteConfigMock).toHaveBeenCalledTimes(1));
    expect(putSiteConfigMock.mock.calls[0][0].features.preview_urls).toEqual({ enabled: false, max_visibility: "org" });
  });

  it("a refusal shows the server's sentence under the controls as it came and keeps the card dirty", async () => {
    const sentence = "features.preview_urls: preview URLs are served by the UI gateway, which is off on this server; set WARDYN_UI_SANDBOX_LISTEN first";
    putSiteConfigMock.mockRejectedValue(new HttpError(400, sentence));
    const card = await open();
    await userEvent.click(await within(card).findByRole("switch", { name: T.LABEL }));
    await userEvent.click(saveBtn(card));
    expect(await within(card).findByRole("alert")).toHaveTextContent(sentence);
    expect(saveBtn(card)).toBeEnabled();
  });

  it("another failure is a toast, not a line on the card", async () => {
    putSiteConfigMock.mockRejectedValue(new HttpError(500, "boom"));
    const card = await open();
    await userEvent.click(await within(card).findByRole("switch", { name: T.LABEL }));
    await userEvent.click(saveBtn(card));
    await waitFor(() => expect(toastError).toHaveBeenCalled());
    expect(toastError.mock.calls[0][0]).toBe(C.SETTINGS_SAVE_FAILED);
    expect(within(card).queryByRole("alert")).toBeNull();
  });

  it("a 412 after someone else changed this key shows the banner and the switch shows theirs", async () => {
    putSiteConfigMock.mockRejectedValue(new HttpError(412, "stale"));
    snapshotMock
      .mockResolvedValueOnce({ siteConfig: {}, etag: '"v1"' })
      .mockResolvedValueOnce({ siteConfig: { features: { preview_urls: { enabled: true, max_visibility: "org" } } }, etag: '"v3"' });
    const card = await open();
    await userEvent.click(await within(card).findByRole("switch", { name: T.LABEL }));
    await userEvent.click(saveBtn(card));
    expect(await within(card).findByText(PROVIDERS.SAVED_ELSEWHERE_TITLE)).toBeInTheDocument();
    expect(putSiteConfigMock).toHaveBeenCalledTimes(1);
    expect(card.querySelector("pre")).toHaveTextContent(/"preview_urls": \{\s*"enabled": true,\s*"max_visibility": "private"\s*\}/);
    expect(radio(card, RUN_PREVIEWS.ORG)).toBeChecked();
  });

  it("a 412 caused by another card's save is not 'someone else': the same save goes once more on the fresh document", async () => {
    putSiteConfigMock.mockRejectedValueOnce(new HttpError(412, "stale")).mockImplementation(saved);
    snapshotMock.mockResolvedValueOnce({ siteConfig: {}, etag: '"v1"' }).mockResolvedValueOnce({ siteConfig: { components: { autonomy_cap: "L1" } }, etag: '"v3"' });
    const card = await open();
    await userEvent.click(await within(card).findByRole("switch", { name: T.LABEL }));
    await userEvent.click(saveBtn(card));
    await waitFor(() => expect(putSiteConfigMock).toHaveBeenCalledTimes(2));
    expect(putSiteConfigMock.mock.calls[1][0]).toEqual({ components: { autonomy_cap: "L1" }, features: { preview_urls: { enabled: true, max_visibility: "private" } } });
    expect(putSiteConfigMock.mock.calls[1][1]).toBe('"v3"');
    expect(within(card).queryByText(PROVIDERS.SAVED_ELSEWHERE_TITLE)).toBeNull();
  });
});
