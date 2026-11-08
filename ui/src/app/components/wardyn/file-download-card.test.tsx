/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi } from "vitest";
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ApprovalRequest, FileDownloadScope } from "../../lib/types";
import { OperatorProvider } from "./operator-context";
import { DECIDERS_INBOX } from "./copy/deciders-inbox";
import { APPROVAL } from "./copy";
import { APPROVAL_MCP_CALL } from "./copy/approvals";
import { FILE_DOWNLOAD_CARD as C } from "./copy/files-review";
import { FileDownloadCard } from "./file-download-card";

const SHA = "9f2c6b0e4d8a17f35c2e9b1a0d7f64c3e85b2a19f0c4d6e7a8b9c0d1e2f341ab";

const scope = (over: Partial<FileDownloadScope> = {}): FileDownloadScope => ({
  name: "build-logs.tar.gz",
  bytes: 19293184,
  sha256: SHA,
  requested_by: "dana.reyes@acme.example",
  ...over,
});

const row = (over: Partial<ApprovalRequest> = {}): ApprovalRequest & { requested_scope: FileDownloadScope } => ({
  id: "apr_dl",
  run_id: "4f2a91c3-0000-0000-0000-000000000000",
  kind: "file_download",
  state: "PENDING",
  requested_at: new Date(Date.now() - 180_000).toISOString(),
  requested_scope: scope(),
  ...over,
}) as ApprovalRequest & { requested_scope: FileDownloadScope };

function renderCard(item: ReturnType<typeof row>, opts: { principal?: string; canDecide?: boolean } = {}) {
  const onApprove = vi.fn();
  const onDeny = vi.fn();
  const view = render(
    <OperatorProvider operator={false} securityOperator={false} principal={opts.principal ?? "lee.chen@acme.example"}>
      <FileDownloadCard item={item} canDecide={opts.canDecide ?? true} run={null} busy={null} onApprove={onApprove} onDeny={onDeny} />
    </OperatorProvider>,
  );
  return { ...view, onApprove, onDeny };
}

describe("the file_download card", () => {
  it("a decider sees who asked, the name, size and hash, and approves once (frame C)", async () => {
    const { onApprove, onDeny } = renderCard(row({ requested_by: "dana.reyes@acme.example", deciders: ["unit_admins", "security"] }));
    const card = screen.getByTestId("file-download-card");
    expect(within(card).getByText(C.TITLE("build-logs.tar.gz"))).toBeInTheDocument();
    expect(within(card).getByText("Download")).toBeInTheDocument();
    expect(within(card).getByText(APPROVAL_MCP_CALL.ONCE)).toBeInTheDocument();
    expect(within(card).getByText("dana.reyes@acme.example", { selector: "span.font-medium" })).toBeInTheDocument();
    expect(card).toHaveTextContent(`${DECIDERS_INBOX.DECIDED_BY_LABEL} admins of this unit or a security admin`);
    const facts = within(card).getAllByRole("definition").map((d) => d.textContent);
    expect(facts).toEqual(["build-logs.tar.gz", "18.4 MB (19,293,184 bytes)", SHA, "dana.reyes@acme.example"]);
    expect(within(card).getAllByRole("term").map((d) => d.textContent)).toEqual([C.FIELD_FILE, C.FIELD_SIZE, C.FIELD_SHA, C.FIELD_ASKED_BY]);
    expect(card).toHaveTextContent(C.WHAT("dana.reyes@acme.example", "build-logs.tar.gz"));
    expect(card).toHaveTextContent(C.BLAST);
    expect(card).toHaveTextContent(C.UNDER_BANNER);
    expect(within(card).queryByRole("button", { name: /more options/i })).toBeNull();
    await userEvent.click(within(card).getByRole("button", { name: APPROVAL_MCP_CALL.APPROVE }));
    expect(onApprove).toHaveBeenCalledTimes(1);
    await userEvent.click(within(card).getByRole("button", { name: APPROVAL_MCP_CALL.DENY }));
    expect(onDeny).toHaveBeenCalledTimes(1);
  });

  it("the asker's own card disables the pair and says who decides (frame C2)", () => {
    const { onApprove } = renderCard(row({ deciders: undefined }), { principal: "dana.reyes@acme.example", canDecide: false });
    const card = screen.getByTestId("file-download-card");
    expect(within(card).getByRole("button", { name: APPROVAL_MCP_CALL.APPROVE })).toBeDisabled();
    expect(within(card).getByRole("button", { name: APPROVAL_MCP_CALL.DENY })).toBeDisabled();
    expect(within(card).getByText(DECIDERS_INBOX.SOMEONE_ELSE)).toBeInTheDocument();
    expect(card).toHaveTextContent(C.ASKER_NOTE);
    expect(card).toHaveTextContent(`${DECIDERS_INBOX.DECIDED_BY_LABEL} ${DECIDERS_INBOX.SECURITY}`);
    expect(within(card).getAllByRole("definition").at(-1)).toHaveTextContent("you");
    expect(onApprove).not.toHaveBeenCalled();
  });

  it("never enables Approve for the asker, even when the caller says the viewer may decide", () => {
    renderCard(row(), { principal: "dana.reyes@acme.example", canDecide: true });
    expect(screen.getByRole("button", { name: APPROVAL_MCP_CALL.APPROVE })).toBeDisabled();
  });

  it("writes 19,293,184 bytes as 18.4 MB and the exact count", () => {
    expect(C.SIZE("18.4 MB", "19,293,184")).toBe("18.4 MB (19,293,184 bytes)");
    renderCard(row());
    expect(screen.getByText("18.4 MB (19,293,184 bytes)")).toBeInTheDocument();
  });

  it("draws the second-person line from the kind's rule, with second_human absent", () => {
    const item = row({ requested_by: "dana.reyes@acme.example" });
    expect(item.second_human).toBeUndefined();
    renderCard(item);
    expect(screen.getByText(DECIDERS_INBOX.SECOND_PERSON)).toBeInTheDocument();
  });

  it("fetches and previews nothing", () => {
    const fetchSpy = vi.spyOn(globalThis, "fetch");
    const { container } = renderCard(row({ requested_by: "dana.reyes@acme.example" }));
    expect(fetchSpy).not.toHaveBeenCalled();
    expect(container.querySelector("img, iframe, video, audio, embed, object, a[href], pre")).toBeNull();
    fetchSpy.mockRestore();
  });

  it("a run that ended shows the cancelled sentence and no buttons", () => {
    const view = render(
      <OperatorProvider operator={false} securityOperator={false} principal="lee.chen@acme.example">
        <FileDownloadCard item={row()} canDecide run={{ state: "COMPLETED" } as never} busy={null} onApprove={vi.fn()} onDeny={vi.fn()} />
      </OperatorProvider>,
    );
    expect(view.container).toHaveTextContent(APPROVAL.CANCELLED_BODY);
    expect(screen.queryByRole("button", { name: APPROVAL_MCP_CALL.APPROVE })).toBeNull();
  });
});
