/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { assistant } from "../../../../lib/api/assistant";
import { HttpError } from "../../../../lib/api/core";
import { workspaces } from "../../../../lib/api/workspaces";
import type { Workspace } from "../../../../lib/types";
import { ASSISTANT as T } from "../../../wardyn/copy/assistant";
import { buildFilesOf, RepoConfirm, RepoPicker, repoSessionRequest } from "./repo-confirm";

const WORKSPACES = [
  {
    id: "w-1",
    name: "Payments",
    available_to_you: true,
    sources: [
      { type: "repo", source: "github.com/example-org/payments-ledger", admitted: true },
      { type: "local_dir", source: "/home/alice/src" },
    ],
  },
  { id: "w-2", name: "Locked", available_to_you: false, sources: [{ type: "repo", source: "github.com/example-org/locked" }] },
  { id: "w-3", name: "Other", available_to_you: true, sources: [{ type: "repo", source: "github.com/example-org/other" }] },
] as unknown as Workspace[];

const confirm = vi.spyOn(assistant, "confirmBuildFiles");
const preview = vi.spyOn(assistant, "postPreview");
const fetchSpy = vi.fn();

beforeEach(() => {
  vi.stubGlobal("fetch", fetchSpy);
  fetchSpy.mockReset();
  confirm.mockReset().mockResolvedValue({ id: "s-2", state: "starting", preview_digest: "d2" } as never);
  preview.mockReset().mockResolvedValue({ digest: "d-new", provider: "Provider B", model: "model-b", sections: [{ label: "Your unit", text: "ceiling: github.com\nnew since you looked", untrusted: false }] } as never);
  vi.spyOn(workspaces, "listWorkspaces").mockResolvedValue(WORKSPACES);
});
afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

describe("RepoPicker", () => {
  it("offers a workspace the person may use, then only that workspace's repository sources, and no free address", async () => {
    const onFetch = vi.fn();
    render(<RepoPicker onFetch={onFetch} />);
    const ws = await screen.findByLabelText(T.REPO_WORKSPACE);
    const names = Array.from(ws.querySelectorAll("option")).map((o) => o.textContent);
    expect(names).toEqual(["", "Payments", "Other"]);
    expect(screen.queryByRole("textbox")).toBeNull();
    const repo = screen.getByLabelText(T.REPO_SOURCE);
    expect(repo).toBeDisabled();
    await userEvent.selectOptions(ws, "w-1");
    expect(Array.from(repo.querySelectorAll("option")).map((o) => o.textContent)).toEqual(["", "payments-ledger"]);
    await userEvent.selectOptions(repo, "github.com/example-org/payments-ledger");
    await userEvent.click(screen.getByRole("button", { name: T.FETCH_BUILD_FILES }));
    expect(onFetch).toHaveBeenCalledWith({ workspace_id: "w-1", repo: "github.com/example-org/payments-ledger" });
  });

  it("starts nothing until a workspace and a repository are chosen", async () => {
    render(<RepoPicker onFetch={vi.fn()} />);
    await screen.findByLabelText(T.REPO_WORKSPACE);
    expect(screen.getByRole("button", { name: T.FETCH_BUILD_FILES })).toBeDisabled();
  });

  it("sends the workspace and repository with the start, never an address", () => {
    const req = repoSessionRequest({ purpose: "compose", target: { kind: "template" }, preview_digest: "d1" }, { workspace_id: "w-1", repo: "github.com/example-org/payments-ledger" });
    expect(req).toEqual({ purpose: "compose", target: { kind: "template" }, preview_digest: "d1", workspace_id: "w-1", repo: "github.com/example-org/payments-ledger" });
    expect(Object.keys(req)).not.toContain("url");
  });
});

const FILES = buildFilesOf({ files: [{ name: "Dockerfile", bytes: 812 }, { name: "package.json", bytes: 1500 }], skipped: ["Makefile"], held_back: [".npmrc"] });

function show(over: Partial<React.ComponentProps<typeof RepoConfirm>> = {}) {
  const onSent = vi.fn();
  const onReturn = vi.fn();
  render(
    <RepoConfirm sessionId="s-1" provider="Org provider · model-name" files={FILES} previewDigest="d1" purpose="compose" target={{ kind: "template" }} onSent={onSent} onReturn={onReturn} {...over} />,
  );
  return { onSent, onReturn };
}
const press = () => userEvent.click(screen.getByRole("button", { name: T.SEND_FILES }));

describe("RepoConfirm", () => {
  it("lists each file's name and size only, with what was skipped and held back", () => {
    show();
    expect(screen.getByText(T.SEND_FILES_TITLE(2, "Org provider · model-name"))).toBeInTheDocument();
    expect(screen.getByText("Dockerfile")).toBeInTheDocument();
    expect(screen.getByText(T.FILE_BYTES(812))).toBeInTheDocument();
    expect(screen.getByText("Makefile")).toBeInTheDocument();
    expect(screen.getByText(".npmrc")).toBeInTheDocument();
  });

  it("posts nothing to the provider's path before Send files", () => {
    show();
    expect(confirm).not.toHaveBeenCalled();
    expect(preview).not.toHaveBeenCalled();
    expect(fetchSpy).not.toHaveBeenCalled();
  });

  it("sends the preview's digest on Send files and says it was sent", async () => {
    const { onSent } = show();
    await press();
    expect(confirm).toHaveBeenCalledWith("s-1", "d1");
    await waitFor(() => expect(onSent).toHaveBeenCalledWith({ id: "s-2", state: "starting", preview_digest: "d2" }));
  });

  it("on a stale preview, previews again, says so, and sends the new digest only on a second press", async () => {
    confirm.mockRejectedValueOnce(new HttpError(409, "changed", "assistant_preview_stale"));
    const { onSent } = show();
    await press();
    expect(await screen.findByText(T.PREVIEW_CHANGED)).toBeInTheDocument();
    // The new preview's records are on screen before the person can send again.
    expect(screen.getByText("Your unit")).toBeInTheDocument();
    expect(screen.getByText(/new since you looked/)).toBeInTheDocument();
    // And the provider they would go to is the fresh preview's, never the one first named.
    expect(screen.getByText(T.SEND_FILES_TITLE(2, "Provider B · model-b"))).toBeInTheDocument();
    expect(screen.queryByText(T.SEND_FILES_TITLE(2, "Org provider · model-name"))).toBeNull();
    expect(preview).toHaveBeenCalledWith("compose", { kind: "template" });
    expect(confirm).toHaveBeenCalledTimes(1);
    expect(onSent).not.toHaveBeenCalled();
    await press();
    expect(confirm).toHaveBeenLastCalledWith("s-1", "d-new");
    await waitFor(() => expect(onSent).toHaveBeenCalledWith({ id: "s-2", state: "starting", preview_digest: "d2" }));
  });

  it("returns to the entry with the server's sentence on any other 409", async () => {
    confirm.mockRejectedValueOnce(new HttpError(409, "That session is not waiting for files.", "assistant_build_files_not_pending"));
    const { onReturn } = show();
    await press();
    await waitFor(() => expect(onReturn).toHaveBeenCalledWith("That session is not waiting for files."));
    expect(preview).not.toHaveBeenCalled();
  });
});

describe("buildFilesOf", () => {
  it("keeps a name and a size, and leaves out anything else", () => {
    expect(buildFilesOf({ files: [{ name: "a", bytes: 1 }, 5, { bytes: 3 }], skipped: ["b", 1], held_back: null })).toEqual({ files: [{ name: "a", bytes: 1 }], skipped: ["b"], held_back: [] });
  });
});
