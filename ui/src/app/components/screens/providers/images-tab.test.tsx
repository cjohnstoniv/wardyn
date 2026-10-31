/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The Images tab (#923, editors.html section 3): the catalog's rows with
// their Available to, the empty state, and Add image asking who gets it.
// Pre-wrapped images (design final §3): How it runs, the digest pin, the
// check's states and Check again.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { HttpError } from "../../../lib/api/core";
import { aheadByHours } from "../../../lib/test-clock";

const listMock = vi.fn();
const addMock = vi.fn();
const checkAgainMock = vi.fn();
vi.mock("../../../lib/api/base-images", () => ({
  baseImages: {
    list: (...a: unknown[]) => listMock(...a),
    add: (...a: unknown[]) => addMock(...a),
    checkAgain: (...a: unknown[]) => checkAgainMock(...a),
  },
}));
const getAvailabilityMock = vi.fn();
const putAvailabilityMock = vi.fn();
const upsertGrantMock = vi.fn();
const listUserTypesMock = vi.fn();
vi.mock("../../../lib/api/permissions", async () => {
  const actual = await vi.importActual<typeof import("../../../lib/api/permissions")>("../../../lib/api/permissions");
  return {
    ...actual,
    permissions: {
      ...actual.permissions,
      getAvailability: (...a: unknown[]) => getAvailabilityMock(...a),
      putAvailability: (...a: unknown[]) => putAvailabilityMock(...a),
      upsertGrant: (...a: unknown[]) => upsertGrantMock(...a),
      listUserTypes: (...a: unknown[]) => listUserTypesMock(...a),
    },
  };
});

import { AVAILABILITY, IMAGES, PREWRAPPED } from "../../../lib/availability-copy";
import type { BaseImageCheck, BaseImageEntry } from "../../../lib/types";
import { OperatorProvider } from "../../wardyn/operator-context";
import { ImagesTab, prewrappedLine } from "./images-tab";

const entry = (id: string, name: string, image: string) => ({
  id,
  kind: "byo" as const,
  name,
  image,
  created_at: aheadByHours(-24),
  updated_at: aheadByHours(-24),
});
const TOOLBOX = entry("b1", "dev-toolbox", "ghcr.io/acme/dev-toolbox:1.4");
const ML = entry("b2", "python-ml", "ghcr.io/acme/python-ml:3.12");
const cat = (rows: BaseImageEntry[], imageBuilder = true, prewrappedUnavailable = "") => ({ rows, imageBuilder, prewrappedUnavailable });
const PINNED = "ghcr.io/acme/sdk-agent:2.3@sha256:" + "a".repeat(64);
const prewrapped = (id: string, check: BaseImageCheck, image = PINNED): BaseImageEntry => ({
  ...entry(id, "sdk-agent", image),
  kind: "prewrapped",
  check,
});
const UNKNOWN_TYPE = 'The user type "portfolio-mgr" doesn\'t exist. Create it under User types first.';

const teal = () =>
  screen.getAllByRole("button").filter((b) => b.className.split(/\s+/).includes("bg-primary"));

beforeEach(() => {
  listMock.mockReset();
  addMock.mockReset();
  checkAgainMock.mockReset();
  getAvailabilityMock.mockReset();
  putAvailabilityMock.mockReset();
  upsertGrantMock.mockReset();
  listUserTypesMock.mockReset();
  listUserTypesMock.mockResolvedValue([
    { id: "portfolio-manager", name: "Portfolio manager", description: "", priority: 0, built_in: false },
  ]);
  getAvailabilityMock.mockImplementation(async (kind: string, value: string) => ({
    kind,
    value,
    restricted: false,
    allowed_by: [],
  }));
});

describe("ImagesTab", () => {
  it("lists each catalog image with its Available to, keyed by the reference, starting at Admins only", async () => {
    listMock.mockResolvedValue(cat([TOOLBOX, ML]));
    render(<ImagesTab />);

    expect(await screen.findByText("dev-toolbox")).toBeInTheDocument();
    expect(screen.getByText("ghcr.io/acme/python-ml:3.12")).toBeInTheDocument();
    await waitFor(() => expect(screen.getAllByRole("radio", { name: AVAILABILITY.ADMINS_ONLY })).toHaveLength(2));
    for (const r of screen.getAllByRole("radio", { name: AVAILABILITY.ADMINS_ONLY })) expect(r).toBeChecked();
    expect(getAvailabilityMock).toHaveBeenCalledWith("image", "ghcr.io/acme/dev-toolbox:1.4");
    expect(getAvailabilityMock).toHaveBeenCalledWith("image", "ghcr.io/acme/python-ml:3.12");
    expect(screen.getAllByText(AVAILABILITY.IMAGE_HINT)).toHaveLength(2);
    expect(screen.getAllByText(AVAILABILITY.IMAGE_NOTE)).toHaveLength(2);
    // The lead, with --image as a literal; one teal: Add image.
    expect(screen.getByText("--image").closest("p")?.textContent).toBe(IMAGES.LEAD);
    expect(teal().map((b) => b.textContent)).toEqual([IMAGES.ADD_CTA]);
  });

  it("reads the user types once for every row's control, not once per row (#1016)", async () => {
    listMock.mockResolvedValue(cat([TOOLBOX, ML]));
    render(<ImagesTab />);

    await waitFor(() => expect(screen.getAllByRole("radio", { name: AVAILABILITY.ADMINS_ONLY })).toHaveLength(2));
    expect(listUserTypesMock).toHaveBeenCalledTimes(1);
  });

  it("empty: says so, and carries Add image as its one teal", async () => {
    listMock.mockResolvedValue(cat([]));
    render(<ImagesTab />);

    // #2873: inside a tab the empty state is the quiet one, a level-4 title in the standard icon tile.
    expect(await screen.findByRole("heading", { name: IMAGES.EMPTY_TITLE, level: 4 })).toBeInTheDocument();
    expect(screen.getByTestId("empty-state-tile")).toBeInTheDocument();
    expect(screen.getByText(IMAGES.EMPTY_BODY)).toBeInTheDocument();
    expect(teal().map((b) => b.textContent)).toEqual([IMAGES.ADD_CTA]);
  });

  it("Add image asks who gets it, starting at Admins only, and writes the image, then the list, then Only these", async () => {
    listMock.mockResolvedValue(cat([]));
    const calls: string[] = [];
    addMock.mockImplementation(async (image: string) => {
      calls.push(`add ${image}`);
      return entry("b9", "dev-toolbox", image);
    });
    upsertGrantMock.mockImplementation(async (g: { subject: string; capability: string; value: string }) =>
      calls.push(`grant ${g.subject} ${g.capability} ${g.value}`),
    );
    putAvailabilityMock.mockImplementation(async (kind: string, value: string, restricted: boolean) =>
      calls.push(`restrict ${kind} ${value} ${restricted}`),
    );
    render(<ImagesTab />);
    await userEvent.click(await screen.findByRole("button", { name: IMAGES.ADD_CTA }));

    const dialog = await screen.findByRole("dialog");
    expect(within(dialog).getByRole("heading", { name: IMAGES.ADD_CTA })).toBeInTheDocument();
    const ref = within(dialog).getByLabelText(IMAGES.REF);
    expect(ref).toHaveFocus();
    expect(ref).toBeRequired();
    expect(within(dialog).getByText(IMAGES.REF_HINT)).toBeInTheDocument();
    expect(within(dialog).getByRole("radio", { name: AVAILABILITY.ADMINS_ONLY })).toBeChecked();
    expect(within(dialog).getByText(AVAILABILITY.IMAGE_HINT)).toBeInTheDocument();

    await userEvent.type(ref, "ghcr.io/acme/dev-toolbox:1.4");
    await userEvent.click(within(dialog).getByRole("radio", { name: AVAILABILITY.ONLY }));
    await userEvent.type(within(dialog).getByPlaceholderText(AVAILABILITY.ADD_PLACEHOLDER), "portfolio-manager");
    await userEvent.click(within(dialog).getByRole("button", { name: AVAILABILITY.ADD_CTA }));
    await userEvent.click(within(dialog).getByRole("button", { name: IMAGES.ADD_CTA }));

    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    expect(calls).toEqual([
      "add ghcr.io/acme/dev-toolbox:1.4",
      "grant portfolio-manager image ghcr.io/acme/dev-toolbox:1.4",
      "restrict image ghcr.io/acme/dev-toolbox:1.4 true",
    ]);
    expect(listMock).toHaveBeenCalledTimes(2);
  });

  it("Only these with nobody listed disables Add image, so nothing saves open to Everyone; the first Add enables it (#2593)", async () => {
    listMock.mockResolvedValue(cat([]));
    addMock.mockResolvedValue(entry("b9", "x", "ghcr.io/acme/x:1"));
    render(<ImagesTab />);
    await userEvent.click(await screen.findByRole("button", { name: IMAGES.ADD_CTA }));
    const dialog = await screen.findByRole("dialog");
    await userEvent.type(within(dialog).getByLabelText(IMAGES.REF), "ghcr.io/acme/x:1");
    const add = within(dialog).getByRole("button", { name: IMAGES.ADD_CTA });
    expect(add).toBeEnabled();

    await userEvent.click(within(dialog).getByRole("radio", { name: AVAILABILITY.ONLY }));
    expect(within(dialog).getByTestId("availability-pending")).toHaveTextContent(AVAILABILITY.PENDING_NOTE_IMAGE);
    expect(add).toBeDisabled();
    await userEvent.click(add);
    expect(addMock).not.toHaveBeenCalled();

    await userEvent.type(within(dialog).getByPlaceholderText(AVAILABILITY.ADD_PLACEHOLDER), "portfolio-manager");
    await userEvent.click(within(dialog).getByRole("button", { name: AVAILABILITY.ADD_CTA }));
    await within(dialog).findByText("Portfolio manager");
    expect(add).toBeEnabled();
  });

  it("Admins only on the form writes only the image", async () => {
    listMock.mockResolvedValue(cat([]));
    addMock.mockResolvedValue(entry("b9", "x", "ghcr.io/acme/x:1"));
    render(<ImagesTab />);
    await userEvent.click(await screen.findByRole("button", { name: IMAGES.ADD_CTA }));
    const dialog = await screen.findByRole("dialog");

    await userEvent.type(within(dialog).getByLabelText(IMAGES.REF), "ghcr.io/acme/x:1{Enter}");

    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    expect(addMock).toHaveBeenCalledWith("ghcr.io/acme/x:1", "byo");
    expect(upsertGrantMock).not.toHaveBeenCalled();
    expect(putAvailabilityMock).not.toHaveBeenCalled();
  });

  it("the image saved but the list was refused: stays open on it with the server's sentence and what holds now", async () => {
    listMock.mockResolvedValue(cat([]));
    addMock.mockResolvedValue(TOOLBOX);
    upsertGrantMock.mockRejectedValue(new HttpError(400, UNKNOWN_TYPE));
    render(<ImagesTab />);
    await userEvent.click(await screen.findByRole("button", { name: IMAGES.ADD_CTA }));
    const dialog = await screen.findByRole("dialog");

    await userEvent.type(within(dialog).getByLabelText(IMAGES.REF), TOOLBOX.image);
    await userEvent.click(within(dialog).getByRole("radio", { name: AVAILABILITY.ONLY }));
    await userEvent.type(within(dialog).getByPlaceholderText(AVAILABILITY.ADD_PLACEHOLDER), "portfolio-mgr");
    await userEvent.click(within(dialog).getByRole("button", { name: AVAILABILITY.ADD_CTA }));
    await userEvent.click(within(dialog).getByRole("button", { name: IMAGES.ADD_CTA }));

    expect(await within(dialog).findByText(AVAILABILITY.CREATE_PARTIAL_TITLE)).toBeInTheDocument();
    expect(within(dialog).getByText(UNKNOWN_TYPE)).toBeInTheDocument();
    expect(within(dialog).getByText(AVAILABILITY.CREATE_PARTIAL_IMAGE)).toBeInTheDocument();
    expect(within(dialog).getByRole("heading", { name: TOOLBOX.name })).toBeInTheDocument();
    expect(putAvailabilityMock).not.toHaveBeenCalled();
    // The control below shows what the server holds, live.
    await waitFor(() => expect(getAvailabilityMock).toHaveBeenCalledWith("image", TOOLBOX.image));
    expect(await within(dialog).findByRole("radio", { name: AVAILABILITY.ADMINS_ONLY })).toBeChecked();
    expect(within(dialog).queryByRole("button", { name: IMAGES.ADD_CTA })).not.toBeInTheDocument();
    expect(listMock).toHaveBeenCalledTimes(2);
  });

  it("a refused image shows the server's sentence in the dialog and writes no list", async () => {
    listMock.mockResolvedValue(cat([]));
    addMock.mockRejectedValue(new HttpError(400, "image must not contain whitespace or control characters"));
    render(<ImagesTab />);
    await userEvent.click(await screen.findByRole("button", { name: IMAGES.ADD_CTA }));
    const dialog = await screen.findByRole("dialog");

    await userEvent.type(within(dialog).getByLabelText(IMAGES.REF), "bad{Enter}");

    expect(await within(dialog).findByText("image must not contain whitespace or control characters")).toBeInTheDocument();
    expect(within(dialog).getByLabelText(IMAGES.REF)).toHaveAttribute("aria-invalid", "true");
    expect(upsertGrantMock).not.toHaveBeenCalled();
  });

  it("pre-wrapped: How it runs offers it, a tag is refused here, and a digest goes to the check", async () => {
    listMock.mockResolvedValue(cat([]));
    addMock.mockResolvedValue(prewrapped("b9", { status: "checking" }));
    render(<ImagesTab />);
    await userEvent.click(await screen.findByRole("button", { name: IMAGES.ADD_CTA }));
    const dialog = await screen.findByRole("dialog");
    const how = within(dialog).getByRole("group", { name: PREWRAPPED.HOW_LABEL });
    expect(within(how).getByRole("button", { name: new RegExp(PREWRAPPED.KIND_WRAP) })).toHaveAttribute("aria-pressed", "true");

    await userEvent.click(within(how).getByRole("button", { name: new RegExp(PREWRAPPED.KIND) }));
    const ref = within(dialog).getByLabelText(PREWRAPPED.REF);
    expect(within(dialog).getByRole("link", { name: PREWRAPPED.HOW_TO_BUILD })).toHaveAttribute("href", PREWRAPPED.HOW_TO_BUILD_URL);
    await userEvent.type(ref, "ghcr.io/acme/sdk-agent:2.3{Enter}");
    expect(await within(dialog).findByText(PREWRAPPED.NOT_PINNED)).toBeInTheDocument();
    expect(addMock).not.toHaveBeenCalled();

    await userEvent.clear(ref);
    await userEvent.type(ref, PINNED);
    await userEvent.click(within(dialog).getByRole("button", { name: PREWRAPPED.ADD_CTA }));
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    expect(addMock).toHaveBeenCalledWith(PINNED, "prewrapped");
  });

  it("a security admin, or a control plane with no builder, adds pre-wrapped only", async () => {
    listMock.mockResolvedValue(cat([]));
    render(
      <OperatorProvider operator={false}>
        <ImagesTab />
      </OperatorProvider>,
    );
    await userEvent.click(await screen.findByRole("button", { name: IMAGES.ADD_CTA }));
    const how = within(await screen.findByRole("dialog")).getByRole("group", { name: PREWRAPPED.HOW_LABEL });
    const wrap = within(how).getByRole("button", { name: new RegExp(PREWRAPPED.KIND_WRAP) });
    expect(wrap).toBeDisabled();
    expect(wrap).toHaveTextContent(PREWRAPPED.KIND_WRAP_ADMINS_ONLY);
    expect(within(how).getByRole("button", { name: new RegExp(PREWRAPPED.KIND) })).toHaveAttribute("aria-pressed", "true");
  });

  it("a control plane that cannot check one says why before input, and refuses the pre-wrapped submission only", async () => {
    const why = "this control plane has no Kubernetes substrate";
    listMock.mockResolvedValue(cat([], true, why));
    render(<ImagesTab />);
    await userEvent.click(await screen.findByRole("button", { name: IMAGES.ADD_CTA }));
    const dialog = await screen.findByRole("dialog");
    const how = within(dialog).getByRole("group", { name: PREWRAPPED.HOW_LABEL });
    const pw = within(how).getByRole("button", { name: new RegExp(PREWRAPPED.KIND) });
    expect(pw).toBeDisabled();
    expect(pw).toHaveTextContent(PREWRAPPED.KIND_NO_CHECK(why));
    expect(within(how).getByRole("button", { name: new RegExp(PREWRAPPED.KIND_WRAP) })).toHaveAttribute("aria-pressed", "true");
    await userEvent.type(within(dialog).getByLabelText(IMAGES.REF), "ghcr.io/acme/x:1");
    expect(within(dialog).getByRole("button", { name: IMAGES.ADD_CTA })).toBeEnabled();
  });

  it("with no builder and no check, nothing can be added", async () => {
    listMock.mockResolvedValue(cat([], false, "this control plane has no Kubernetes substrate"));
    render(<ImagesTab />);
    await userEvent.click(await screen.findByRole("button", { name: IMAGES.ADD_CTA }));
    const dialog = await screen.findByRole("dialog");
    await userEvent.type(within(dialog).getByLabelText(PREWRAPPED.REF), PINNED);
    expect(within(dialog).getByRole("button", { name: PREWRAPPED.ADD_CTA })).toBeDisabled();
  });

  it("rows: a checking one is offered to nobody yet and the list is read again; a byo one needs a builder", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    try {
      listMock
        .mockResolvedValueOnce(cat([prewrapped("b5", { status: "checking" }), TOOLBOX], false))
        .mockResolvedValue(
          cat([prewrapped("b5", { status: "ready", accepted_arches: ["amd64", "arm64"], checked_at: aheadByHours(-1) }), TOOLBOX], false),
        );
      render(<ImagesTab />);
      const row = await screen.findByTestId(`image-row-${PINNED}`);
      expect(within(row).getByText(PREWRAPPED.BADGE)).toBeInTheDocument();
      expect(within(row).getByText(PREWRAPPED.CHECKING)).toBeInTheDocument();
      expect(within(row).queryByRole("radio")).not.toBeInTheDocument();
      expect(within(screen.getByTestId(`image-row-${TOOLBOX.image}`)).getByText(PREWRAPPED.NEEDS_BUILDER)).toBeInTheDocument();
      expect(screen.getByText(PREWRAPPED.FOOTNOTE)).toBeInTheDocument();

      await vi.advanceTimersByTimeAsync(5000);
      await waitFor(() => expect(within(row).getByText(/^Ready: checked on the cluster on linux\/amd64 and linux\/arm64/)).toBeInTheDocument());
      expect(await within(row).findByRole("radio", { name: AVAILABILITY.ADMINS_ONLY })).toBeInTheDocument();
      expect(within(row).queryByText(AVAILABILITY.IMAGE_NOTE)).not.toBeInTheDocument();
    } finally {
      vi.useRealTimers();
    }
  });

  it("a refused row offers Check again, which asks the server and reads the list again", async () => {
    listMock.mockResolvedValue(cat([prewrapped("b5", { status: "refused", reason: "check_interrupted" })]));
    checkAgainMock.mockResolvedValue(prewrapped("b5", { status: "checking" }));
    render(<ImagesTab />);
    const row = await screen.findByTestId(`image-row-${PINNED}`);
    expect(within(row).getByText(PREWRAPPED.CUT_SHORT)).toBeInTheDocument();
    await userEvent.click(within(row).getByRole("button", { name: PREWRAPPED.CHECK_AGAIN }));
    expect(checkAgainMock).toHaveBeenCalledWith("b5");
    await waitFor(() => expect(listMock).toHaveBeenCalledTimes(2));
  });

  it("each verdict reads as its own line", () => {
    const line = (check: BaseImageCheck, builder = true) => prewrappedLine(prewrapped("b5", check), builder);
    expect(line({ status: "ready", reason: "platform_missing:arm64", accepted_arches: ["amd64"] })).toEqual({
      text: PREWRAPPED.READY_NO_BUILD("amd64", "arm64"),
      refused: false,
    });
    expect(line({ status: "ready", reason: "no_node:arm64", accepted_arches: ["amd64"] })?.text).toBe(PREWRAPPED.READY_NO_NODE("amd64", "arm64"));
    expect(line({ status: "refused", reason: "contract_missing:amd64:wardyn-rec" }, false)?.text).toBe(PREWRAPPED.MISSING_TOOL("wardyn-rec", false));
    expect(line({ status: "refused", reason: "unpullable:amd64:unauthorized" })?.text).toBe(PREWRAPPED.UNPULLABLE("unauthorized", "ghcr.io/acme"));
    expect(line({ status: "refused", reason: "timed_out:amd64" })?.text).toBe(PREWRAPPED.CUT_SHORT);
    expect(line({ status: "refused", reason: "contract_old:amd64" })?.text).toBe(PREWRAPPED.CONTRACT_OLD);
    expect(line({ status: "refused", reason: "no_node:amd64" })?.text).toBe(PREWRAPPED.NO_NODE);
    expect(line({ status: "refused", reason: "platform_missing:amd64" })?.text).toBe(PREWRAPPED.NO_BUILD);
    expect(line({ status: "refused", reason: "failed:amd64:exit 1: sh: x: not found" })).toEqual({
      text: PREWRAPPED.FAILED("exit 1: sh: x: not found"),
      refused: true,
    });
    expect(prewrappedLine(TOOLBOX, true)).toBeNull();
  });
});
