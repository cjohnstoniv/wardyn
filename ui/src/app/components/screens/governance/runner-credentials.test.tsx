/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Governance -> Runner credentials (M-RN-4 DP1/DP2). Expected strings come from DELIVERY. Pinned here because
// they are decisions: bounds render as disabled options with their reason (never absent), an organisation's
// Azure DevOps tokens may be stored on a runner, only classes that are not Don't allow are written, a 202 is
// "submitted" and a 400 shows the server's own sentence.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";

const getMock = vi.fn();
const putMock = vi.fn();
const runnersEnabledMock = vi.fn();
vi.mock("../../../lib/api/credential-delivery", () => ({
  credentialDelivery: {
    get: () => getMock(),
    put: (...a: unknown[]) => putMock(...a),
    runnersEnabled: () => runnersEnabledMock(),
  },
}));

import { setField } from "../../../../test/set-field";
import { HttpError, PendingChangeError } from "../../../lib/api/core";
import { CHANGES, DELIVERY } from "../../../lib/governance-copy";
import type { GovernanceChange } from "../../../lib/types";
import type { DeliveryPolicy } from "../../../lib/types/credential-delivery";
import { RunnerCredentials } from "./runner-credentials";

const doc = (classes: DeliveryPolicy["classes"] = {}) => ({ policy: { classes }, updated_at: "", updated_by: "" });

function renderTab() {
  return render(
    <MemoryRouter>
      <RunnerCredentials />
    </MemoryRouter>,
  );
}

const group = (cls: keyof typeof DELIVERY.CLASS) => screen.getByRole("radiogroup", { name: DELIVERY.CLASS[cls] });
const option = (cls: keyof typeof DELIVERY.CLASS, mode: keyof typeof DELIVERY.MODE) =>
  within(group(cls)).getByRole("radio", { name: DELIVERY.MODE[mode] });

beforeEach(() => {
  getMock.mockReset().mockResolvedValue(doc());
  putMock.mockReset().mockResolvedValue(doc());
  runnersEnabledMock.mockReset().mockResolvedValue(true);
});

describe("RunnerCredentials — the rows", () => {
  it("lists one row per class in the packet's order, the fixed ones read-only", async () => {
    renderTab();
    await screen.findByRole("radiogroup", { name: DELIVERY.CLASS.api_key });
    const names = screen.getAllByRole("heading", { level: 3 }).map((h) => h.textContent);
    expect(names).toEqual([
      DELIVERY.CLASS.api_key,
      DELIVERY.CLASS.bedrock_bearer,
      DELIVERY.CLASS.aws_sso_bearer,
      DELIVERY.CLASS.azure_foundry_token,
      DELIVERY.CLASS.bedrock_role_credentials,
      DELIVERY.CLASS.env_secret,
      DELIVERY.CLASS.file_secret,
      DELIVERY.CLASS.ssh_key,
      DELIVERY.CLASS.git_pat_broker,
      DELIVERY.CLASS.git_pat_helper,
      DELIVERY.CLASS.github_token,
      DELIVERY.CLASS.ado_minted_pat,
      DELIVERY.CLASS.oauth_subscription,
      DELIVERY.CLASS.cloud_sts,
    ]);
    expect(screen.getAllByRole("radiogroup")).toHaveLength(11);
    expect(screen.getByText(DELIVERY.NOT_IN_RELEASE)).toBeInTheDocument();
    expect(screen.getByText(DELIVERY.FIXED_OWN)).toBeInTheDocument();
    expect(screen.getByText(DELIVERY.FIXED_CLOUD_STS)).toBeInTheDocument();
  });

  it("an absent class reads Don't allow, with the person's preview", async () => {
    renderTab();
    await screen.findByRole("radiogroup", { name: DELIVERY.CLASS.api_key });
    expect(option("api_key", "refuse")).toBeChecked();
    expect(screen.getAllByText(DELIVERY.PEOPLE_SEE(DELIVERY.LOCAL_DELIVERY.refuse))).toHaveLength(11);
  });

  it("a mode outside a class's bounds is a disabled option with its reason, not an absent one", async () => {
    renderTab();
    await screen.findByRole("radiogroup", { name: DELIVERY.CLASS.api_key });
    expect(option("env_secret", "via_org")).toBeDisabled();
    expect(screen.getAllByText(DELIVERY.BOUND_NO_VIA_ORG)).toHaveLength(5);
    expect(option("github_token", "runner_resident")).toBeDisabled();
    expect(screen.getByText(DELIVERY.BOUND_NO_RESIDENT_GITHUB)).toBeInTheDocument();
    expect(option("api_key", "via_org")).toBeEnabled();
  });

  it("an organisation's Azure DevOps tokens can be stored on a runner, with the connection note", async () => {
    const user = userEvent.setup();
    renderTab();
    await screen.findByRole("radiogroup", { name: DELIVERY.CLASS.api_key });
    expect(option("ado_minted_pat", "runner_resident")).toBeEnabled();
    await user.click(option("ado_minted_pat", "runner_resident"));
    expect(screen.getByText(DELIVERY.ADO_RESIDENT_HINT)).toBeInTheDocument();
  });

  it("the posture block and the honesty lines show only under Store on the runner", async () => {
    const user = userEvent.setup();
    renderTab();
    await screen.findByRole("radiogroup", { name: DELIVERY.CLASS.api_key });
    expect(screen.queryByText(DELIVERY.RESIDENT_HONESTY)).toBeNull();
    await user.click(option("env_secret", "runner_resident"));
    expect(screen.getByText(DELIVERY.POSTURE_TITLE)).toBeInTheDocument();
    expect(screen.getByText(DELIVERY.RESIDENT_HONESTY)).toBeInTheDocument();
    expect(screen.getByText(DELIVERY.RESIDENT_FLOOR)).toBeInTheDocument();
    expect(screen.getByText(DELIVERY.OS_MIN_HINT)).toBeInTheDocument();
    await user.click(option("env_secret", "refuse"));
    expect(screen.queryByText(DELIVERY.POSTURE_TITLE)).toBeNull();
  });

  it("notes that runners are off, and stays editable", async () => {
    runnersEnabledMock.mockResolvedValue(false);
    renderTab();
    expect(await screen.findByText(DELIVERY.RUNNERS_OFF_NOTE)).toBeInTheDocument();
    expect(option("api_key", "via_org")).toBeEnabled();
  });
});

describe("RunnerCredentials — save", () => {
  it("is disabled until something changes, and writes only the classes that are not Don't allow", async () => {
    const user = userEvent.setup();
    renderTab();
    await screen.findByRole("radiogroup", { name: DELIVERY.CLASS.api_key });
    const save = screen.getByRole("button", { name: DELIVERY.SAVE });
    expect(save).toBeDisabled();
    await user.click(option("api_key", "via_org"));
    await user.click(option("env_secret", "runner_resident"));
    await user.click(screen.getByLabelText(DELIVERY.POSTURE_MDM));
    setField(screen.getByLabelText(DELIVERY.OS_MIN(DELIVERY.OS_NAME.darwin)), "15.0");
    await user.click(save);
    await waitFor(() => expect(putMock).toHaveBeenCalledTimes(1));
    expect(putMock.mock.calls[0][0]).toEqual({
      classes: {
        api_key: { mode: "via_org" },
        env_secret: { mode: "runner_resident", require_posture: { mdm_managed: true, os_min: { darwin: "15.0" } } },
      },
    });
    expect(await screen.findByText(DELIVERY.SAVED)).toBeInTheDocument();
  });

  it("drops a posture when the class leaves Store on the runner, and writes nothing for a class set back to Don't allow", async () => {
    getMock.mockResolvedValue(doc({ env_secret: { mode: "runner_resident", require_posture: { disk_encrypted: true } } }));
    const user = userEvent.setup();
    renderTab();
    await screen.findByRole("radiogroup", { name: DELIVERY.CLASS.api_key });
    expect(screen.getByLabelText(DELIVERY.POSTURE_DISK)).toBeChecked();
    await user.click(option("env_secret", "refuse"));
    await user.click(screen.getByRole("button", { name: DELIVERY.SAVE }));
    await waitFor(() => expect(putMock).toHaveBeenCalledTimes(1));
    expect(putMock.mock.calls[0][0]).toEqual({ classes: {} });
  });

  it("carries a stored shared-Azure policy through a save untouched", async () => {
    getMock.mockResolvedValue(doc({ azure_foundry_token: { mode: "via_org" } }));
    const user = userEvent.setup();
    renderTab();
    await screen.findByRole("radiogroup", { name: DELIVERY.CLASS.api_key });
    await user.click(option("api_key", "via_org"));
    await user.click(screen.getByRole("button", { name: DELIVERY.SAVE }));
    await waitFor(() => expect(putMock).toHaveBeenCalledTimes(1));
    expect(putMock.mock.calls[0][0]).toEqual({ classes: { api_key: { mode: "via_org" }, azure_foundry_token: { mode: "via_org" } } });
  });

  it("a minimum that is not a version blocks Save and says so on its field", async () => {
    const user = userEvent.setup();
    renderTab();
    await screen.findByRole("radiogroup", { name: DELIVERY.CLASS.api_key });
    await user.click(option("env_secret", "runner_resident"));
    const field = screen.getByLabelText(DELIVERY.OS_MIN(DELIVERY.OS_NAME.windows));
    setField(field, "latest");
    expect(field).toHaveAttribute("aria-invalid", "true");
    expect(screen.getByText(DELIVERY.OS_MIN_INVALID)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: DELIVERY.SAVE })).toBeDisabled();
  });

  it("a 202 is the submitted note and nothing says saved", async () => {
    putMock.mockRejectedValue(new PendingChangeError({ id: "c1" } as GovernanceChange));
    const onSubmitted = vi.fn();
    const user = userEvent.setup();
    render(
      <MemoryRouter>
        <RunnerCredentials onSubmitted={onSubmitted} />
      </MemoryRouter>,
    );
    await screen.findByRole("radiogroup", { name: DELIVERY.CLASS.api_key });
    await user.click(option("api_key", "via_org"));
    await user.click(screen.getByRole("button", { name: DELIVERY.SAVE }));
    expect(await screen.findByText(CHANGES.SUBMITTED_TITLE)).toBeInTheDocument();
    expect(screen.queryByText(DELIVERY.SAVED)).toBeNull();
    expect(onSubmitted).toHaveBeenCalledTimes(1);
  });

  it("a 400 shows the server's sentence under the refusal heading and moves focus there", async () => {
    putMock.mockRejectedValue(new HttpError(400, 'class "github_token" cannot be delivered as "runner_resident"'));
    const user = userEvent.setup();
    renderTab();
    await screen.findByRole("radiogroup", { name: DELIVERY.CLASS.api_key });
    await user.click(option("api_key", "via_org"));
    await user.click(screen.getByRole("button", { name: DELIVERY.SAVE }));
    const heading = await screen.findByRole("heading", { name: DELIVERY.REFUSED_TITLE });
    expect(screen.getByRole("alert")).toHaveTextContent('class "github_token" cannot be delivered as "runner_resident"');
    expect(heading).toHaveFocus();
  });
});

describe("RunnerCredentials — loading", () => {
  it("a failed read says so and offers Retry, which reads again", async () => {
    getMock.mockRejectedValueOnce(new Error("boom"));
    const user = userEvent.setup();
    renderTab();
    expect(await screen.findByText(DELIVERY.FETCH_FAILED_TITLE)).toBeInTheDocument();
    expect(screen.getByText(DELIVERY.FETCH_FAILED_BODY)).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: /retry/i }));
    expect(await screen.findByRole("radiogroup", { name: DELIVERY.CLASS.api_key })).toBeInTheDocument();
  });
});
