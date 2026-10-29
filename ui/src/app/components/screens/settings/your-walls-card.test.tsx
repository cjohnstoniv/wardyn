/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// T4-a: the Your walls card. The api module is mocked at its three routes; what is pinned is what the
// card sends (only narrowing values), what it shows back, and the strings of the owner-approved board.
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { setField } from "../../../../test/set-field";
import { HttpError } from "../../../lib/api/core";
import type { PersonalWalls } from "../../../lib/api/walls";
import { YOUR_WALLS } from "../../wardyn/copy/personal-walls";

const getMock = vi.fn<() => Promise<PersonalWalls>>();
const putMock = vi.fn<(spec: unknown) => Promise<PersonalWalls>>();
const clearMock = vi.fn<() => Promise<void>>();
vi.mock("../../../lib/api/walls", async (orig) => ({
  ...(await orig<typeof import("../../../lib/api/walls")>()),
  walls: { get: () => getMock(), put: (s: unknown) => putMock(s), clear: () => clearMock() },
}));

import { YourWallsCard } from "./your-walls-card";

const user = userEvent.setup();
const open = async () => {
  await user.click(await screen.findByRole("button", { name: /^Your walls/ }));
  await screen.findByText(YOUR_WALLS.SITES);
};
const echo = (spec: unknown): Promise<PersonalWalls> => Promise.resolve({ spec: spec as PersonalWalls["spec"], updated_at: "2026-10-29T12:00:00Z" });

beforeEach(() => {
  getMock.mockResolvedValue({ spec: {} });
  putMock.mockImplementation(echo);
  clearMock.mockResolvedValue();
});
afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

describe("Your walls card", () => {
  it("is collapsed with the lede as its summary, and opens to the board's fields", async () => {
    render(<YourWallsCard />);
    expect(await screen.findByText(YOUR_WALLS.LEDE)).toBeInTheDocument();
    expect(screen.queryByText(YOUR_WALLS.SITES)).not.toBeInTheDocument();
    await open();
    for (const text of [YOUR_WALLS.SITES_ANY, YOUR_WALLS.SITES_ONLY, YOUR_WALLS.NEVER, YOUR_WALLS.UNLISTED, YOUR_WALLS.BARRIER, YOUR_WALLS.CLIPBOARD, YOUR_WALLS.CREDENTIALS, YOUR_WALLS.RESTART_NOTE]) {
      expect(screen.getByText(text)).toBeInTheDocument();
    }
    for (const name of [YOUR_WALLS.UNLISTED_BLOCK, YOUR_WALLS.UNLISTED_ASK, YOUR_WALLS.UNLISTED_HOLD, YOUR_WALLS.SAVE, YOUR_WALLS.REMOVE_ALL]) {
      expect(screen.getByRole("button", { name })).toBeInTheDocument();
    }
    expect(screen.getByRole("radio", { name: "Fence" })).toBeInTheDocument();
    expect(screen.getByRole("radio", { name: "Wall" })).toBeInTheDocument();
    expect(screen.getByRole("radio", { name: "Vault" })).toBeInTheDocument();
  });

  it("shows the walls it loaded", async () => {
    getMock.mockResolvedValue({
      spec: {
        allowed_domains: ["packages.example.com"],
        denied_domains: ["paste.example.net"],
        first_use_approval: "wait_for_review",
        min_confinement_class: "CC2",
        desktop: { copy_out: "blocked", download: "blocked" },
        withheld_grants: [{ kind: "ssh_key" }, { kind: "cloud_sts" }],
      },
    });
    render(<YourWallsCard />);
    await open();
    expect(screen.getByRole("radio", { name: YOUR_WALLS.SITES_ONLY })).toBeChecked();
    expect(screen.getByText("packages.example.com")).toBeInTheDocument();
    expect(screen.getByText("paste.example.net")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: YOUR_WALLS.UNLISTED_HOLD })).toHaveAttribute("aria-pressed", "true");
    expect(screen.getByRole("radio", { name: "Wall" })).toBeChecked();
    expect(screen.getByRole("checkbox", { name: YOUR_WALLS.COPY_OUT })).toBeChecked();
    expect(screen.getByRole("checkbox", { name: YOUR_WALLS.PASTE_IN })).not.toBeChecked();
    expect(screen.getByText("SSH key")).toBeInTheDocument();
    expect(screen.getByText("Cloud credentials")).toBeInTheDocument();
  });

  it("choosing Only these sites also selects Block it, and Save sends only narrowing values", async () => {
    render(<YourWallsCard />);
    await open();
    await user.click(screen.getByRole("radio", { name: YOUR_WALLS.SITES_ONLY }));
    expect(screen.getByRole("button", { name: YOUR_WALLS.UNLISTED_BLOCK })).toHaveAttribute("aria-pressed", "true");
    const [only, never] = screen.getAllByPlaceholderText(YOUR_WALLS.ADD_SITE);
    setField(only, "api.example.com");
    fireEvent.keyDown(only, { key: "Enter" });
    setField(never, "paste.example.net");
    await user.click(screen.getByRole("checkbox", { name: YOUR_WALLS.COPY_OUT }));
    await user.click(screen.getByRole("radio", { name: "Wall" }));
    await user.selectOptions(screen.getByRole("combobox", { name: YOUR_WALLS.CREDENTIALS }), "ssh_key");
    await user.click(screen.getByRole("button", { name: YOUR_WALLS.SAVE }));
    // A site typed and not entered is still saved.
    await waitFor(() =>
      expect(putMock).toHaveBeenCalledWith({
        allowed_domains: ["api.example.com"],
        denied_domains: ["paste.example.net"],
        first_use_approval: "always_deny",
        min_confinement_class: "CC2",
        desktop: { copy_out: "blocked" },
        withheld_grants: [{ kind: "ssh_key" }],
      }),
    );
    expect(await screen.findByRole("status")).toHaveTextContent(YOUR_WALLS.SAVED);
  });

  it("Any site sends no allowlist, and an empty form sends an empty spec", async () => {
    getMock.mockResolvedValue({ spec: { allowed_domains: ["a.example.com"] } });
    render(<YourWallsCard />);
    await open();
    await user.click(screen.getByRole("radio", { name: YOUR_WALLS.SITES_ANY }));
    await user.click(screen.getByRole("button", { name: YOUR_WALLS.SAVE }));
    await waitFor(() => expect(putMock).toHaveBeenCalledWith({}));
  });

  it("keeps an allow_all_egress: false the CLI wrote, and a scoped withheld entry", async () => {
    const scoped = { kind: "api_key", scope: { host: "x.example.com", header: "x-key" }, requires_approval: false };
    getMock.mockResolvedValue({ spec: { allow_all_egress: false, withheld_grants: [scoped] } });
    render(<YourWallsCard />);
    await open();
    await user.click(screen.getByRole("button", { name: YOUR_WALLS.SAVE }));
    await waitFor(() => expect(putMock).toHaveBeenCalledWith({ allow_all_egress: false, withheld_grants: [scoped] }));
  });

  it("removes a site chip", async () => {
    getMock.mockResolvedValue({ spec: { denied_domains: ["a.example.com", "b.example.com"] } });
    render(<YourWallsCard />);
    await open();
    await user.click(screen.getByRole("button", { name: YOUR_WALLS.REMOVE_SITE("a.example.com") }));
    expect(screen.queryByText("a.example.com")).not.toBeInTheDocument();
    expect(screen.getByText("b.example.com")).toBeInTheDocument();
  });

  it("Remove all walls clears the document and the form", async () => {
    getMock.mockResolvedValue({ spec: { denied_domains: ["a.example.com"], desktop: { upload: "blocked" } } });
    render(<YourWallsCard />);
    await open();
    await user.click(screen.getByRole("button", { name: YOUR_WALLS.REMOVE_ALL }));
    await waitFor(() => expect(clearMock).toHaveBeenCalledTimes(1));
    expect(screen.queryByText("a.example.com")).not.toBeInTheDocument();
    expect(screen.getByRole("checkbox", { name: YOUR_WALLS.UPLOAD })).not.toBeChecked();
  });

  it("says what the server refused with its code in small print, and offers no Saved", async () => {
    putMock.mockRejectedValue(new HttpError(400, "invalid walls: denied_domains[0]: \"x y\" must be a DNS name", "walls_invalid"));
    render(<YourWallsCard />);
    await open();
    await user.click(screen.getByRole("button", { name: YOUR_WALLS.SAVE }));
    const alert = await screen.findByRole("alert");
    expect(alert).toHaveTextContent("denied_domains[0]");
    expect(within(alert).getByText("walls_invalid")).toBeInTheDocument();
    expect(screen.queryByRole("status")).not.toBeInTheDocument();
  });

  it.each(["walls_need_person", "walls_unavailable"])("is absent when the server answers %s", async (reason) => {
    getMock.mockRejectedValue(new HttpError(reason === "walls_unavailable" ? 501 : 400, "no", reason));
    const { container } = render(<YourWallsCard />);
    await waitFor(() => expect(getMock).toHaveBeenCalled());
    await waitFor(() => expect(container).toBeEmptyDOMElement());
  });

  it("offers a retry when the read fails for another reason", async () => {
    getMock.mockRejectedValueOnce(new HttpError(500, "boom", ""));
    render(<YourWallsCard />);
    await user.click(await screen.findByRole("button", { name: /^Your walls/ }));
    await user.click(await screen.findByRole("button", { name: YOUR_WALLS.RETRY }));
    await waitFor(() => expect(getMock).toHaveBeenCalledTimes(2));
  });
});
