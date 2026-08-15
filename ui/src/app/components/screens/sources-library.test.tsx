/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The tier-1 DIRECTORIES & REPOS library tab: rows with status/contract/used-by,
// an add dialog that upserts, and the delete-in-use refusal that names the
// attaching workspaces before offering the detach-everywhere escape.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { HttpError } from "../../lib/api/core";
import type { Source, Workspace } from "../../lib/types";
import { baseStatus } from "./setup/test-fixtures";
import { OperatorProvider } from "../wardyn/operator-context";

const listSourcesMock = vi.fn();
const createSourceMock = vi.fn();
const scanSourceMock = vi.fn();
const deleteSourceMock = vi.fn();
vi.mock("../../lib/api/sources", () => ({
  sourcesApi: {
    listSources: (...a: unknown[]) => listSourcesMock(...a),
    createSource: (...a: unknown[]) => createSourceMock(...a),
    scanSource: (...a: unknown[]) => scanSourceMock(...a),
    deleteSource: (...a: unknown[]) => deleteSourceMock(...a),
  },
  baseImagesApi: {},
}));
// useK8sRunner's own fetch (B4) — default docker-shaped, k8s tests override it.
const getSetupStatusMock = vi.fn();
vi.mock("../../lib/api/setup", () => ({
  setup: { getSetupStatus: (...a: unknown[]) => getSetupStatusMock(...a) },
}));
vi.mock("sonner", () => ({ toast: { error: vi.fn(), success: vi.fn(), info: vi.fn() } }));

import { SourcesLibrary, contractSummary, sourceUsage } from "./sources-library";

function src(over: Partial<Source> = {}): Source {
  return {
    id: "s-1",
    kind: "repo",
    locator: "github.com/acme/payments",
    ref: "main",
    name: "payments",
    status: "scanned",
    created_at: "",
    updated_at: "",
    ...over,
  };
}

function ws(over: Partial<Workspace> = {}): Workspace {
  return {
    id: "w-1",
    name: "w",
    kind: "repo",
    source: "acme/payments",
    status: "scanned",
    created_at: "",
    updated_at: "",
    ...over,
  } as Workspace;
}

beforeEach(() => {
  listSourcesMock.mockReset().mockResolvedValue([]);
  createSourceMock.mockReset();
  scanSourceMock.mockReset();
  deleteSourceMock.mockReset();
  getSetupStatusMock.mockReset().mockResolvedValue(baseStatus({ runner: { driver: "docker", confinement_classes: ["CC1"] } }));
});

describe("sourceUsage / contractSummary — pure helpers", () => {
  it("counts attaching workspaces per source id", () => {
    const usage = sourceUsage([
      ws({ attachments: [{ source_id: "a" }, { source_id: "b" }] }),
      ws({ id: "w-2", attachments: [{ source_id: "a" }, { ephemeral: true }] }),
    ]);
    expect(usage.get("a")).toBe(2);
    expect(usage.get("b")).toBe(1);
  });

  // ui-sourcesImages-2 regression: a repo that scans clean (no secrets, no
  // egress) is a legitimate empty contract on a USABLE source, not an
  // unscanned one — "No contract yet" next to "Status: Usable" read as the
  // scan having silently not happened.
  it("distinguishes a clean scan's empty contract from a not-yet-scanned one", () => {
    expect(contractSummary(src({ status: "scanned" }))).toBe("No requirements");
    expect(contractSummary(src({ status: "pending_scan" }))).toBe("No contract yet");
    expect(contractSummary(src({ status: "scanning" }))).toBe("No contract yet");
    expect(contractSummary(src({ status: "error" }))).toBe("No contract yet");
  });

  it("summarizes the source's own contract by lane", () => {
    expect(
      contractSummary(
        src({
          requirements: {
            "secret:STRIPE_KEY": { level: "required", provenance: "operator_set" },
            "egress:api.stripe.com": { level: "required", provenance: "scan_seeded" },
            "write:/x": { level: "optional", provenance: "operator_set" },
          },
        }),
      ),
    ).toBe("2 required · 1 optional");
  });
});

describe("SourcesLibrary", () => {
  it("renders library rows with identity, status word, contract and used-by", async () => {
    listSourcesMock.mockResolvedValue([
      src({
        requirements: { "egress:api.stripe.com": { level: "required", provenance: "scan_seeded" } },
      }),
    ]);
    render(<SourcesLibrary workspaces={[ws({ attachments: [{ source_id: "s-1" }] })]} />);

    expect(await screen.findByText("payments")).toBeInTheDocument();
    expect(screen.getByText(/repo · github.com\/acme\/payments @main/)).toBeInTheDocument();
    expect(screen.getByText("Usable")).toBeInTheDocument();
    expect(screen.getByText("1 required")).toBeInTheDocument();
    expect(screen.getByText("1 workspace")).toBeInTheDocument();
  });

  // UI-LIB-9: Chip only renders `pulse` inside its `dot` — without `dot` an
  // actively-scanning row looked identical to a never-scanned (pending_scan)
  // one for the whole 4s-polled duration of a real scan.
  it("shows a pulsing dot while a source is actively scanning, not a static chip", async () => {
    listSourcesMock.mockResolvedValue([src({ status: "scanning" })]);
    const { container } = render(<SourcesLibrary workspaces={[]} />);

    await screen.findByText("payments");
    expect(container.querySelector(".animate-ping")).toBeInTheDocument();
  });

  it("adds a repo through the dialog AND scans it immediately — never parked at Setting up", async () => {
    createSourceMock.mockResolvedValue(src({ id: "s-9", name: "lib" }));
    scanSourceMock.mockResolvedValue({ scan_run_id: "r-1" });
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    render(<SourcesLibrary workspaces={[]} />);

    await user.click(await screen.findByRole("button", { name: /add directory or repo/i }));
    await user.click(screen.getByRole("radio", { name: /repository/i }));
    await user.type(screen.getByLabelText(/repo slug or clone url/i), "acme/lib");
    await user.type(screen.getByLabelText(/git ref/i), "main");
    await user.click(screen.getByRole("button", { name: /add to library/i }));

    await waitFor(() =>
      expect(createSourceMock).toHaveBeenCalledWith({
        kind: "repo",
        locator: "acme/lib",
        ref: "main",
        name: undefined,
      }),
    );
    // Adding IS scanning: a dir resolves inline in the same motion; a repo
    // launches its governed run and the quiet poll settles the row.
    await waitFor(() => expect(scanSourceMock).toHaveBeenCalledWith("s-9"));
  });

  it("delete-in-use surfaces the server's 409 naming workspaces, then forces on the explicit escape", async () => {
    listSourcesMock.mockResolvedValue([src()]);
    deleteSourceMock
      .mockRejectedValueOnce(new HttpError(409, "source is attached by 2 workspace(s): pay, billing"))
      .mockResolvedValueOnce(undefined);
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    render(<SourcesLibrary workspaces={[]} />);

    await user.click(await screen.findByRole("button", { name: /actions for payments/i }));
    await user.click(screen.getByRole("menuitem", { name: /delete/i }));
    const dialog = screen.getByRole("dialog");
    await user.click(within(dialog).getByRole("button", { name: "Delete" }));

    expect(await within(dialog).findByText(/pay, billing/)).toBeInTheDocument();
    await user.click(within(dialog).getByRole("button", { name: /detach everywhere & delete/i }));
    await waitFor(() => expect(deleteSourceMock).toHaveBeenLastCalledWith("s-1", true));
  });

  // ui-sourcesImages-1 regression: force-detach on a source that's a
  // workspace's SOLE attachment 409s deterministically too (STORE-1) — the
  // dialog must surface that second failure, not fall silent while still
  // offering the same doomed escape.
  it("a force-detach that still 409s (sole attachment) surfaces the new failure instead of going silent", async () => {
    listSourcesMock.mockResolvedValue([src()]);
    deleteSourceMock
      .mockRejectedValueOnce(new HttpError(409, "source is attached by 1 workspace(s): solo"))
      .mockRejectedValueOnce(
        new HttpError(409, "force-deleting would leave workspace(s) with no attachments: solo"),
      );
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    render(<SourcesLibrary workspaces={[]} />);

    await user.click(await screen.findByRole("button", { name: /actions for payments/i }));
    await user.click(screen.getByRole("menuitem", { name: /delete/i }));
    const dialog = screen.getByRole("dialog");
    await user.click(within(dialog).getByRole("button", { name: "Delete" }));
    expect(await within(dialog).findByText(/attached by 1 workspace/)).toBeInTheDocument();

    await user.click(within(dialog).getByRole("button", { name: /detach everywhere & delete/i }));
    expect(await within(dialog).findByText(/no attachments/)).toBeInTheDocument();
    // Still open — not a silent generic toast masquerading as the dialog
    // just sitting there with a stale, now-false message.
    expect(screen.getByRole("dialog")).toBeInTheDocument();
  });

  // ui-sourcesImages-3 regression (a11y): operator-disabled Scan/Delete gave
  // no visible reason, unlike the sibling Workspaces tier on the same page.
  it("shows an inline operator-only reason on disabled Scan/Delete for a non-operator", async () => {
    listSourcesMock.mockResolvedValue([src()]);
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    render(
      <OperatorProvider operator={false}>
        <SourcesLibrary workspaces={[]} />
      </OperatorProvider>,
    );

    await user.click(await screen.findByRole("button", { name: /actions for payments/i }));
    const menu = screen.getByRole("menu");
    expect(within(menu).getAllByText(/requires the operator role/i)).toHaveLength(2);
  });

  it("scan action hits the per-source endpoint", async () => {
    listSourcesMock.mockResolvedValue([src()]);
    scanSourceMock.mockResolvedValue({});
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    render(<SourcesLibrary workspaces={[]} />);

    await user.click(await screen.findByRole("button", { name: /actions for payments/i }));
    await user.click(screen.getByRole("menuitem", { name: /scan/i }));
    await waitFor(() => expect(scanSourceMock).toHaveBeenCalledWith("s-1"));
  });
});

// B4 source honesty: mounts are structurally impossible on k8s — the add
// dialog omits the local-directory radio entirely (not just defaults away
// from it) and shows the one quiet line in its place.
describe("SourcesLibrary — add dialog on a k8s control plane", () => {
  it("omits Local directory, shows the quiet line, and defaults kind to repo", async () => {
    getSetupStatusMock.mockResolvedValue(baseStatus({ runner: { driver: "k8s", confinement_classes: ["CC1"] } }));
    createSourceMock.mockResolvedValue(src({ id: "s-9", name: "lib" }));
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    render(<SourcesLibrary workspaces={[]} />);

    await user.click(await screen.findByRole("button", { name: /add directory or repo/i }));
    await waitFor(() =>
      expect(
        screen.getByText(/Local directories aren.t available on a Kubernetes control plane/),
      ).toBeInTheDocument(),
    );
    expect(screen.queryByRole("radio", { name: /local directory/i })).toBeNull();
    expect(screen.queryByRole("radio", { name: /repository/i })).toBeNull();
    // No kind picker needed (repo is the only option) — the locator field is
    // already in its repo shape.
    expect(screen.getByLabelText(/repo slug or clone url/i)).toBeInTheDocument();

    await user.type(screen.getByLabelText(/repo slug or clone url/i), "acme/lib");
    await user.click(screen.getByRole("button", { name: /add to library/i }));
    await waitFor(() => expect(createSourceMock).toHaveBeenCalledWith(expect.objectContaining({ kind: "repo" })));
  });

  it("a docker/local control plane is unaffected — both options still show", async () => {
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    render(<SourcesLibrary workspaces={[]} />);

    await user.click(await screen.findByRole("button", { name: /add directory or repo/i }));
    expect(await screen.findByRole("radio", { name: /local directory/i })).toBeInTheDocument();
    expect(screen.getByRole("radio", { name: /repository/i })).toBeInTheDocument();
    expect(screen.queryByText(/Kubernetes control plane/)).toBeNull();
  });

  // M1 regression: useK8sRunner(open) resolves false->true AFTER open (open
  // flips synchronously on click; the fetch is async) — typing BEFORE that
  // resolution used to get wiped the moment it landed, because `kind` was
  // reset inside an effect keyed on [open, k8s]. Every other test in this
  // file waits for the quiet line (i.e. for k8s to resolve) BEFORE typing,
  // which is exactly why they never caught this — this one types first.
  it("typing before the k8s fetch resolves survives the flip (M1)", async () => {
    let resolveStatus!: (v: ReturnType<typeof baseStatus>) => void;
    getSetupStatusMock.mockReturnValue(
      new Promise((r) => {
        resolveStatus = r;
      }),
    );
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    render(<SourcesLibrary workspaces={[]} />);

    await user.click(await screen.findByRole("button", { name: /add directory or repo/i }));
    // The fetch hasn't resolved yet — still the docker-shaped dialog.
    expect(await screen.findByRole("radio", { name: /local directory/i })).toBeInTheDocument();
    await user.type(screen.getByLabelText(/host path/i), "/home/me/projects/payments");
    await user.type(screen.getByLabelText(/^name/i), "payments");

    // NOW the k8s fetch resolves — the dialog flips to the k8s (repo-only) shape.
    resolveStatus(baseStatus({ runner: { driver: "k8s", confinement_classes: ["CC1"] } }));
    await waitFor(() =>
      expect(
        screen.getByText(/Local directories aren.t available on a Kubernetes control plane/),
      ).toBeInTheDocument(),
    );

    // Both fields keep exactly what was typed — the reset effect must not
    // have re-fired on the k8s flip.
    expect(screen.getByLabelText(/repo slug or clone url/i)).toHaveValue("/home/me/projects/payments");
    expect(screen.getByLabelText(/^name/i)).toHaveValue("payments");
  });
});
