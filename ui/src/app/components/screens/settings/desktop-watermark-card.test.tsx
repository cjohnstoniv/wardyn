/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The desktop watermark card: the site config's `desktop_watermark.required`.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { expandCard } from "../../../lib/test-dom";
import { expectActionBar } from "../../../../test/action-bar";
import { FORM_ACTIONS } from "../../wardyn/copy/form-actions";
import { DESKTOP_WATERMARK as T } from "../../wardyn/copy/desktop-watermark";
import { COMPONENTS_ADMIN as C } from "../../wardyn/copy/components-admin";
import { PROVIDERS, PROVIDERS_EXTRA } from "../../../lib/workspace-providers-copy";

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
import { DesktopWatermarkCard } from "./desktop-watermark-card";

async function open() {
  render(<DesktopWatermarkCard />);
  await expandCard(T.TITLE);
  return screen.getByTestId("desktop-watermark-card");
}
const sw = (card: HTMLElement) => within(card).getByRole("switch", { name: T.LABEL });
const saveBtn = (card: HTMLElement) => within(card).getByRole("button", { name: FORM_ACTIONS.SAVE });

beforeEach(() => {
  toastSuccess.mockClear();
  toastError.mockClear();
  snapshotMock.mockReset().mockResolvedValue({ siteConfig: { upstream_proxy_url: "http://p" }, etag: '"v1"' });
  putSiteConfigMock.mockReset().mockImplementation((cfg: object) =>
    Promise.resolve({ siteConfig: cfg, etag: '"v2"', danglingSecretRefs: [], onboardingCompletedAtIgnored: false, appliesFrom: "", sourcesNoLongerAdmitted: null }),
  );
});

describe("DesktopWatermarkCard", () => {
  it("is loading until the read answers, with no switch", async () => {
    snapshotMock.mockReturnValue(new Promise(() => {}));
    render(<DesktopWatermarkCard />);
    await expandCard(T.TITLE);
    expect(screen.queryByRole("switch")).toBeNull();
    expect(screen.queryByRole("button", { name: FORM_ACTIONS.SAVE })).toBeNull();
  });

  it("a failed read says so and offers Retry, which reads again", async () => {
    snapshotMock.mockRejectedValueOnce(new Error("down")).mockResolvedValueOnce({ siteConfig: { desktop_watermark: { required: true } }, etag: '"v1"' });
    const card = await open();
    expect(await within(card).findByText(C.SETTINGS_LOAD_FAILED)).toBeInTheDocument();
    await userEvent.click(within(card).getByRole("button", { name: C.SETTINGS_RETRY }));
    expect(await within(card).findByRole("switch", { name: T.LABEL })).toBeChecked();
  });

  it("shows the label, the hint and the honest note; off when nothing is stored; Save held until the switch changes", async () => {
    const card = await open();
    expect(await within(card).findByRole("switch", { name: T.LABEL })).not.toBeChecked();
    expect(sw(card)).toHaveAccessibleDescription(T.HINT);
    expect(within(card).getByText(T.HONEST)).toBeInTheDocument();
    expect(saveBtn(card)).toBeDisabled();
    expectActionBar(saveBtn(card));
  });

  it("reads the stored requirement", async () => {
    snapshotMock.mockResolvedValue({ siteConfig: { desktop_watermark: { required: true } }, etag: '"v1"' });
    const card = await open();
    expect(await within(card).findByRole("switch", { name: T.LABEL })).toBeChecked();
  });

  it("saves on with the ETag as If-Match, keeping the rest of the document, and says it saved", async () => {
    const card = await open();
    await userEvent.click(await within(card).findByRole("switch", { name: T.LABEL }));
    await userEvent.click(saveBtn(card));
    await waitFor(() => expect(putSiteConfigMock).toHaveBeenCalledTimes(1));
    expect(putSiteConfigMock).toHaveBeenCalledWith({ upstream_proxy_url: "http://p", desktop_watermark: { required: true } }, '"v1"');
    expect(toastSuccess).toHaveBeenCalledWith(T.SAVED_TOAST);
    expect(sw(card)).toBeChecked();
    expect(saveBtn(card)).toBeDisabled();
  });

  it("saving off sends an empty block, not required:false", async () => {
    snapshotMock.mockResolvedValue({ siteConfig: { desktop_watermark: { required: true } }, etag: '"v1"' });
    const card = await open();
    await userEvent.click(await within(card).findByRole("switch", { name: T.LABEL }));
    await userEvent.click(saveBtn(card));
    await waitFor(() => expect(putSiteConfigMock).toHaveBeenCalledTimes(1));
    expect(putSiteConfigMock.mock.calls[0][0].desktop_watermark).toEqual({});
    expect(putSiteConfigMock.mock.calls[0][1]).toBe('"v1"');
  });

  it("a refusal shows the server's sentence under the switch and keeps the card dirty", async () => {
    putSiteConfigMock.mockRejectedValue(new HttpError(400, "invalid site config: desktop_watermark: nope"));
    const card = await open();
    await userEvent.click(await within(card).findByRole("switch", { name: T.LABEL }));
    await userEvent.click(saveBtn(card));
    expect(await within(card).findByRole("alert")).toHaveTextContent("invalid site config: desktop_watermark: nope");
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

  it("a 412 after someone else changed this block shows the banner with the person's block, and the switch shows theirs", async () => {
    putSiteConfigMock.mockRejectedValue(new HttpError(412, "stale"));
    snapshotMock.mockResolvedValueOnce({ siteConfig: {}, etag: '"v1"' }).mockResolvedValueOnce({ siteConfig: { desktop_watermark: { required: true } }, etag: '"v3"' });
    const card = await open();
    await userEvent.click(await within(card).findByRole("switch", { name: T.LABEL }));
    await userEvent.click(saveBtn(card));
    expect(await within(card).findByText(PROVIDERS.SAVED_ELSEWHERE_TITLE)).toBeInTheDocument();
    expect(within(card).getByText(PROVIDERS_EXTRA.SAVED_ELSEWHERE_RELOADED_BODY)).toBeInTheDocument();
    expect(card.querySelector("pre")).toHaveTextContent(/"desktop_watermark": \{\s*"required": true\s*\}/);
    expect(putSiteConfigMock).toHaveBeenCalledTimes(1);
    expect(sw(card)).toBeChecked();
    await userEvent.click(within(card).getByRole("button", { name: PROVIDERS_EXTRA.DISCARD_RELOADED }));
    expect(within(card).queryByText(PROVIDERS.SAVED_ELSEWHERE_TITLE)).toBeNull();
  });

  it("a 412 caused by another card's save is not 'someone else': the same save goes once more on the fresh document", async () => {
    putSiteConfigMock.mockRejectedValueOnce(new HttpError(412, "stale")).mockImplementation((cfg: object) =>
      Promise.resolve({ siteConfig: cfg, etag: '"v4"', danglingSecretRefs: [], onboardingCompletedAtIgnored: false, appliesFrom: "", sourcesNoLongerAdmitted: null }),
    );
    snapshotMock.mockResolvedValueOnce({ siteConfig: {}, etag: '"v1"' }).mockResolvedValueOnce({ siteConfig: { components: { autonomy_cap: "L1" } }, etag: '"v3"' });
    const card = await open();
    await userEvent.click(await within(card).findByRole("switch", { name: T.LABEL }));
    await userEvent.click(saveBtn(card));
    await waitFor(() => expect(putSiteConfigMock).toHaveBeenCalledTimes(2));
    expect(putSiteConfigMock.mock.calls[1][0]).toEqual({ components: { autonomy_cap: "L1" }, desktop_watermark: { required: true } });
    expect(putSiteConfigMock.mock.calls[1][1]).toBe('"v3"');
    expect(within(card).queryByText(PROVIDERS.SAVED_ELSEWHERE_TITLE)).toBeNull();
    expect(toastSuccess).toHaveBeenCalledWith(T.SAVED_TOAST);
  });
});
