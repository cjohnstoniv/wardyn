/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Governance -> Baseline hosts: the stored declaration is what the editor shows, a save sends one host per
// line with the ETag it read, and the server's refusal and a held write read as they do on Runner credentials.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";

const getMock = vi.fn();
const putMock = vi.fn();
vi.mock("../../../lib/api/egress-baseline", () => ({
  egressBaseline: { get: () => getMock(), put: (...a: unknown[]) => putMock(...a) },
}));

import { setField } from "../../../../test/set-field";
import { HttpError, PendingChangeError } from "../../../lib/api/core";
import { BASELINE, CHANGES } from "../../../lib/governance-copy";
import type { GovernanceChange } from "../../../lib/types";
import { EgressBaselineTab } from "./egress-baseline";

const stored = { baseline_hosts: ["llm.corp.example"], internal_host_suffixes: [], etag: '"e1"' };

function renderTab(onSubmitted?: () => void) {
  return render(
    <MemoryRouter>
      <EgressBaselineTab onSubmitted={onSubmitted} />
    </MemoryRouter>,
  );
}

beforeEach(() => {
  getMock.mockReset().mockResolvedValue(stored);
  putMock.mockReset().mockImplementation((body: unknown) => Promise.resolve({ ...(body as object), etag: '"e2"' }));
});

describe("EgressBaselineTab", () => {
  it("shows the stored hosts, and Save stays off until the draft differs", async () => {
    renderTab();
    expect(await screen.findByLabelText(BASELINE.HOSTS_LABEL)).toHaveValue("llm.corp.example");
    expect(screen.getByText(BASELINE.LEAD)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: BASELINE.SAVE })).toBeDisabled();
  });

  it("saves one host per line, trimmed, with the ETag it read", async () => {
    const user = userEvent.setup();
    renderTab();
    const hosts = await screen.findByLabelText(BASELINE.HOSTS_LABEL);
    setField(hosts, "llm.corp.example\n  bedrock.llm.corp.example  \n\n");
    await user.click(screen.getByRole("button", { name: BASELINE.SAVE }));
    expect(await screen.findByText(BASELINE.SAVED)).toBeInTheDocument();
    expect(putMock).toHaveBeenCalledWith(
      { baseline_hosts: ["llm.corp.example", "bedrock.llm.corp.example"], internal_host_suffixes: [] },
      '"e1"',
    );
  });

  it("a refusal shows the server's sentence under the heading", async () => {
    const said = 'invalid egress baseline: baseline_hosts[1]: "*.corp.example" must be an exact hostname';
    putMock.mockRejectedValue(new HttpError(400, said));
    const user = userEvent.setup();
    renderTab();
    setField(await screen.findByLabelText(BASELINE.HOSTS_LABEL), "llm.corp.example\n*.corp.example");
    await user.click(screen.getByRole("button", { name: BASELINE.SAVE }));
    expect(await screen.findByText(BASELINE.REFUSED_TITLE)).toBeInTheDocument();
    expect(screen.getByText(said)).toBeInTheDocument();
  });

  it("a 202 is the submitted note and nothing says saved", async () => {
    putMock.mockRejectedValue(new PendingChangeError({ id: "c1" } as GovernanceChange));
    const onSubmitted = vi.fn();
    const user = userEvent.setup();
    renderTab(onSubmitted);
    setField(await screen.findByLabelText(BASELINE.SUFFIXES_LABEL), "corp.example");
    await user.click(screen.getByRole("button", { name: BASELINE.SAVE }));
    expect(await screen.findByText(CHANGES.SUBMITTED_TITLE)).toBeInTheDocument();
    expect(screen.queryByText(BASELINE.SAVED)).toBeNull();
    expect(onSubmitted).toHaveBeenCalledTimes(1);
  });
});
