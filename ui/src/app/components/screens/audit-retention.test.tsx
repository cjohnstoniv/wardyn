/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Audit → Retention (mock packet M4, surfaces A and B): the policy card, the
// partitions table with the SERVER's eligibility, and the drop dialog. Every
// expected string is the packet's, spelled out here rather than read back from
// the copy module, so a drifted string fails.
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import type { AuditRetentionStatus } from "../../lib/types";

const listAuditMock = vi.fn();
const getRetentionMock = vi.fn();
const dropMock = vi.fn();
const exportMock = vi.fn();
vi.mock("../../lib/api/audit", () => ({
  audit: {
    listAudit: (...a: unknown[]) => listAuditMock(...a),
    getRetention: (...a: unknown[]) => getRetentionMock(...a),
    dropPartition: (...a: unknown[]) => dropMock(...a),
    exportPartition: (...a: unknown[]) => exportMock(...a),
  },
}));
vi.mock("../../lib/api/runs", () => ({ runs: { getRun: vi.fn().mockResolvedValue(undefined) } }));
vi.mock("../../lib/api/health", () => ({ health: { health: () => Promise.resolve({}) } }));
const toastError = vi.fn();
vi.mock("sonner", () => ({ toast: { success: vi.fn(), error: (...a: unknown[]) => toastError(...a) } }));

import { HttpError } from "../../lib/api/core";
import { OperatorProvider } from "../wardyn/operator-context";
import { AuditScreen } from "./audit";

function status(over: Partial<AuditRetentionStatus> = {}): AuditRetentionStatus {
  return {
    policy: { days: 365, effective_days: 365, pending_days: 90, pending_effective_at: "2026-11-02T00:00:00Z" },
    cutover: "2026-10-03T12:00:00Z",
    months_ahead: 12,
    partitions: [
      {
        name: "audit_events_legacy",
        hi: "2026-10-03T12:00:00Z",
        rows: 2104331,
        state: "closed",
        eligible: true,
      },
      {
        name: "audit_events_2026_10",
        lo: "2026-10-03T12:00:00Z",
        hi: "2026-11-01T00:00:00Z",
        rows: 48210,
        state: "closed",
        eligible: false,
        refusal: "audit_retention_not_oldest",
      },
      {
        name: "audit_events_2026_11",
        lo: "2026-11-01T00:00:00Z",
        hi: "2026-12-01T00:00:00Z",
        rows: 9114,
        state: "open",
        eligible: false,
        refusal: "audit_retention_not_oldest",
      },
    ],
    ...over,
  };
}

function renderAudit(securityOperator = true) {
  return render(
    <MemoryRouter>
      <OperatorProvider operator securityOperator={securityOperator} operatorResolved principal="sec@corp.example">
        <AuditScreen />
      </OperatorProvider>
    </MemoryRouter>,
  );
}

async function openRetention() {
  renderAudit();
  await userEvent.click(await screen.findByRole("tab", { name: "Retention" }));
}

describe("Audit → Retention", () => {
  beforeEach(() => {
    listAuditMock.mockReset().mockResolvedValue([]);
    getRetentionMock.mockReset().mockResolvedValue(status());
    dropMock.mockReset();
    exportMock.mockReset();
    toastError.mockReset();
  });

  it("shows the policy, the cutover and the partitions in the packet's words", async () => {
    await openRetention();
    expect(await screen.findByText("Kept for")).toBeInTheDocument();
    expect(screen.getByText("365 days")).toBeInTheDocument();
    expect(screen.getByText("Changes to 90 days on 2 Nov 2026.")).toBeInTheDocument();
    expect(
      screen.getByText(
        "Set by WARDYN_AUDIT_RETENTION_DAYS on the server. A shorter period takes effect 30 days after the restart that sets it, so no one can shorten it and drop history the same day.",
      ),
    ).toBeInTheDocument();
    expect(
      screen.getByText(
        "Events before 3 Oct 2026 are in one partition, because Wardyn started splitting the log by month that day.",
      ),
    ).toBeInTheDocument();
    expect(screen.getByText("12 months of partitions ready")).toBeInTheDocument();
    expect(
      screen.getByText(
        "Dropping deletes a whole partition for good. Each drop is recorded in the Audit log with its digest, and verification starts from it.",
      ),
    ).toBeInTheDocument();

    const legacy = screen.getByRole("row", { name: /audit_events_legacy/ });
    expect(within(legacy).getByText("Before 3 Oct 2026")).toBeInTheDocument();
    expect(within(legacy).getByText("2,104,331")).toBeInTheDocument();
    expect(within(legacy).getByText("Closed")).toBeInTheDocument();
    const oct = screen.getByRole("row", { name: /audit_events_2026_10/ });
    expect(within(oct).getByText("Oct 2026")).toBeInTheDocument();
    const nov = screen.getByRole("row", { name: /audit_events_2026_11/ });
    expect(within(nov).getByText("Open")).toBeInTheDocument();
  });

  it("an eligible partition can be exported and dropped; an ineligible one shows the server's reason and no Drop", async () => {
    await openRetention();
    const legacy = await screen.findByRole("row", { name: /audit_events_legacy/ });
    expect(within(legacy).getByText("Can be dropped")).toBeInTheDocument();
    expect(within(legacy).getByRole("button", { name: "Drop" })).toBeInTheDocument();
    expect(within(legacy).getByRole("button", { name: /Export/ })).toBeInTheDocument();

    const oct = screen.getByRole("row", { name: /audit_events_2026_10/ });
    expect(within(oct).getByText("An older partition comes first")).toBeInTheDocument();
    expect(within(oct).queryByRole("button", { name: "Drop" })).toBeNull();
    expect(within(oct).getByRole("button", { name: /Export/ })).toBeInTheDocument();

    // An open partition has nothing to export or drop.
    const nov = screen.getByRole("row", { name: /audit_events_2026_11/ });
    expect(within(nov).queryByRole("button")).toBeNull();
  });

  it("eligibility is the server's: an eligible flag on a later partition offers Drop, a refusal reason names why not", async () => {
    getRetentionMock.mockResolvedValue(
      status({
        partitions: [
          {
            name: "audit_events_2026_10",
            lo: "2026-10-03T12:00:00Z",
            hi: "2026-11-01T00:00:00Z",
            rows: 5,
            state: "closed",
            eligible: false,
            refusal: "audit_retention_inside_window",
          },
          {
            name: "audit_events_2026_09",
            lo: "2026-09-01T00:00:00Z",
            hi: "2026-10-01T00:00:00Z",
            rows: 7,
            state: "closed",
            eligible: false,
            refusal: "audit_retention_live_run",
          },
        ],
      }),
    );
    await openRetention();
    expect(await screen.findByText("Inside the retention period")).toBeInTheDocument();
    expect(screen.getByText("Holds events of a live run")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Drop" })).toBeNull();
  });

  it("forever, and a low runway, read as the packet says", async () => {
    getRetentionMock.mockResolvedValue(
      status({ policy: { days: 0, effective_days: 0 }, months_ahead: 2 }),
    );
    await openRetention();
    expect(await screen.findByText("Forever")).toBeInTheDocument();
    expect(screen.queryByText(/^Changes to/)).toBeNull();
    expect(
      screen.getByText(
        "Only 2 months of partitions are ready. Wardyn adds them daily; if this stays low, check the server log.",
      ),
    ).toBeInTheDocument();
    expect(screen.queryByText(/months of partitions ready$/)).toBeNull();
  });

  it("a failed read is an error with a retry, not an empty card", async () => {
    getRetentionMock.mockRejectedValueOnce(new HttpError(500, "boom"));
    await openRetention();
    await userEvent.click(await screen.findByRole("button", { name: /retry/i }));
    expect(await screen.findByText("Kept for")).toBeInTheDocument();
    expect(getRetentionMock).toHaveBeenCalledTimes(2);
  });

  describe("the drop dialog", () => {
    async function openDrop() {
      await openRetention();
      const legacy = await screen.findByRole("row", { name: /audit_events_legacy/ });
      await userEvent.click(within(legacy).getByRole("button", { name: "Drop" }));
      return screen.getByRole("dialog");
    }

    it("opens with the packet's title and body, and Drop partition waits for a digest", async () => {
      const dialog = await openDrop();
      expect(within(dialog).getByText("Drop every event before 3 Oct 2026?")).toBeInTheDocument();
      expect(
        within(dialog).getByText(
          "This deletes its 2,104,331 events for good. Export it first and keep the archive: after the drop, it is the only copy.",
        ),
      ).toBeInTheDocument();
      expect(within(dialog).getByRole("button", { name: "Readable export" })).toBeInTheDocument();
      expect(within(dialog).getByRole("button", { name: "Raw archive" })).toBeInTheDocument();
      expect(
        within(dialog).getByText("The raw archive lets anyone recompute every row hash and the digest without Wardyn."),
      ).toBeInTheDocument();
      expect(
        within(dialog).getByText(
          "Paste it from the archive you kept. Wardyn recomputes it and refuses the drop if they differ.",
        ),
      ).toBeInTheDocument();
      const confirm = within(dialog).getByRole("button", { name: "Drop partition" });
      expect(confirm).toBeDisabled();
      const field = within(dialog).getByLabelText("Digest from the archive's footer");
      expect(field).toHaveClass("font-mono");
      await userEvent.type(field, "ab12");
      expect(confirm).toBeEnabled();
    });

    it("a monthly partition is titled by its month", async () => {
      getRetentionMock.mockResolvedValue(
        status({
          partitions: [
            {
              name: "audit_events_2026_10",
              lo: "2026-10-01T00:00:00Z",
              hi: "2026-11-01T00:00:00Z",
              rows: 48210,
              state: "closed",
              eligible: true,
            },
          ],
        }),
      );
      await openRetention();
      await userEvent.click(await screen.findByRole("button", { name: "Drop" }));
      expect(within(screen.getByRole("dialog")).getByText("Drop the Oct 2026 partition?")).toBeInTheDocument();
    });

    it("a mismatched digest is refused with its sentence under the field, and the dialog stays open", async () => {
      dropMock.mockRejectedValue(new HttpError(409, "server words", "audit_retention_digest_mismatch"));
      const dialog = await openDrop();
      await userEvent.type(within(dialog).getByLabelText("Digest from the archive's footer"), "deadbeef");
      await userEvent.click(within(dialog).getByRole("button", { name: "Drop partition" }));
      expect(dropMock).toHaveBeenCalledWith("audit_events_legacy", "deadbeef");
      expect(await within(dialog).findByRole("alert")).toHaveTextContent(
        "The digest doesn't match this partition. Check you pasted it from this partition's archive.",
      );
      expect(within(dialog).getByRole("button", { name: "Drop partition" })).toBeInTheDocument();
    });

    it.each([
      ["audit_retention_not_oldest", "Only the oldest partition can be dropped."],
      ["audit_retention_not_closed", "This partition is still receiving events."],
      ["audit_retention_inside_window", "This partition is still inside the retention period."],
      [
        "audit_retention_live_run",
        "This partition holds events of a run that is still live. Drop it after the run ends.",
      ],
    ])("refusal %s reads as the packet's sentence", async (reason, sentence) => {
      dropMock.mockRejectedValue(new HttpError(409, "server words", reason));
      const dialog = await openDrop();
      await userEvent.type(within(dialog).getByLabelText("Digest from the archive's footer"), "ab");
      await userEvent.click(within(dialog).getByRole("button", { name: "Drop partition" }));
      expect(await within(dialog).findByRole("alert")).toHaveTextContent(sentence);
    });

    it("a failure at 500 or above says the drop may not have finished", async () => {
      dropMock.mockRejectedValue(new HttpError(500, "internal", "audit_retention_drop_failed"));
      const dialog = await openDrop();
      await userEvent.type(within(dialog).getByLabelText("Digest from the archive's footer"), "ab");
      await userEvent.click(within(dialog).getByRole("button", { name: "Drop partition" }));
      expect(await within(dialog).findByRole("alert")).toHaveTextContent(
        "The drop didn't finish, or its answer was lost. Reload: if the partition is still listed, nothing was deleted.",
      );
    });

    it("an unmapped 4xx shows the server's sentence as sent", async () => {
      dropMock.mockRejectedValue(new HttpError(404, "no such audit partition", "audit_partition_not_found"));
      const dialog = await openDrop();
      await userEvent.type(within(dialog).getByLabelText("Digest from the archive's footer"), "ab");
      await userEvent.click(within(dialog).getByRole("button", { name: "Drop partition" }));
      expect(await within(dialog).findByRole("alert")).toHaveTextContent("no such audit partition");
    });

    it("a drop shows the result lines, and Close reloads the table", async () => {
      dropMock.mockResolvedValue({
        partition: "audit_events_legacy",
        rows: 2104331,
        seq_lo: 1,
        seq_hi: 2104331,
        digest: "ab",
        event_seq: 2104332,
      });
      const dialog = await openDrop();
      await userEvent.type(within(dialog).getByLabelText("Digest from the archive's footer"), "ab");
      await userEvent.click(within(dialog).getByRole("button", { name: "Drop partition" }));
      expect(await within(dialog).findByText("Dropped 2,104,331 events.")).toBeInTheDocument();
      expect(
        within(dialog).getByText("Recorded in the Audit log as audit.retention.partition_dropped."),
      ).toBeInTheDocument();
      expect(getRetentionMock).toHaveBeenCalledTimes(1);
      await userEvent.click(within(dialog).getAllByRole("button", { name: "Close" })[0]);
      await waitFor(() => expect(getRetentionMock).toHaveBeenCalledTimes(2));
    });

    describe("exports", () => {
      const created: string[] = [];
      beforeEach(() => {
        created.length = 0;
        URL.createObjectURL = vi.fn(() => "blob:archive");
        URL.revokeObjectURL = vi.fn();
        vi.spyOn(HTMLAnchorElement.prototype, "click").mockImplementation(function (this: HTMLAnchorElement) {
          created.push(this.download);
        });
      });
      afterEach(() => vi.restoreAllMocks());

      it("the dialog's two buttons download the readable and the raw form", async () => {
        exportMock.mockResolvedValue(new Blob(["x"]));
        const dialog = await openDrop();
        await userEvent.click(within(dialog).getByRole("button", { name: "Readable export" }));
        await waitFor(() => expect(created).toContain("audit_events_legacy.readable.ndjson"));
        await userEvent.click(within(dialog).getByRole("button", { name: "Raw archive" }));
        await waitFor(() => expect(created).toContain("audit_events_legacy.raw.ndjson"));
        expect(exportMock).toHaveBeenCalledWith("audit_events_legacy", "readable");
        expect(exportMock).toHaveBeenCalledWith("audit_events_legacy", "raw");
      });

      it("a row's Export menu offers both forms, and a failed export says so", async () => {
        exportMock.mockRejectedValue(new HttpError(409, "That partition can still receive rows."));
        await openRetention();
        const oct = await screen.findByRole("row", { name: /audit_events_2026_10/ });
        await userEvent.click(within(oct).getByRole("button", { name: /Export/ }));
        expect(await screen.findByRole("menuitem", { name: "Readable export" })).toBeInTheDocument();
        await userEvent.click(screen.getByRole("menuitem", { name: "Raw archive" }));
        await waitFor(() => expect(toastError).toHaveBeenCalledWith("That partition can still receive rows."));
        expect(exportMock).toHaveBeenCalledWith("audit_events_2026_10", "raw");
      });
    });
  });
});

describe("a session that is not a security operator", () => {
  beforeEach(() => {
    listAuditMock.mockReset().mockResolvedValue([]);
    getRetentionMock.mockReset().mockResolvedValue(status());
  });

  it("never sees the Retention tab, and the retention read is never made", async () => {
    renderAudit(false);
    expect(await screen.findByRole("heading", { name: "Audit" })).toBeInTheDocument();
    await waitFor(() => expect(listAuditMock).toHaveBeenCalled());
    expect(screen.queryByRole("tab", { name: "Retention" })).toBeNull();
    expect(screen.queryByRole("tab", { name: "Events" })).toBeNull();
    expect(getRetentionMock).not.toHaveBeenCalled();
  });

  it("a security operator has both tabs, and Events is the feed it always was", async () => {
    renderAudit(true);
    expect(await screen.findByRole("tab", { name: "Events" })).toHaveAttribute("data-state", "active");
    expect(screen.getByRole("tab", { name: "Retention" })).toBeInTheDocument();
    expect(getRetentionMock).not.toHaveBeenCalled();
  });
});
