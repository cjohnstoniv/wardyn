/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The Storage tab: ephemeral default/max + the enforcement gloss + the Docker
// warning, the drive ceiling switch/field, and the EXISTING UserDrivesCard as
// the link out — NO drives table embedded (Q7).
import * as React from "react";
import { describe, it, expect, vi } from "vitest";
import { fireEvent, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import type { StorageProviders } from "../../../lib/api/providers";
import { MEMBER_DRIVES, PROVIDERS } from "../../../lib/workspace-providers-copy";
import { DRIVES } from "../../../lib/user-drives-copy";
import { OperatorProvider } from "../../wardyn/operator-context";
import { StorageTab } from "./storage-tab";

const getDrivesMock = vi.fn(async () => ({ drives: [], grants: [], host_roots_configured: false, runner_target: "" }));
vi.mock("../../../lib/api/drives", async () => {
  const actual = await vi.importActual<typeof import("../../../lib/api/drives")>("../../../lib/api/drives");
  return { ...actual, drives: { ...actual.drives, getDrives: () => getDrivesMock() } };
});

function Harness({
  initial,
  enforcement,
  onStorage,
}: {
  initial: StorageProviders;
  enforcement?: "filesystem" | "none" | "eviction";
  onStorage?: (s: StorageProviders) => void;
}) {
  const [storage, setStorage] = React.useState(initial);
  onStorage?.(storage);
  return (
    <MemoryRouter>
      <OperatorProvider operator>
        <StorageTab storage={storage} onChange={setStorage} enforcement={enforcement} operator />
      </OperatorProvider>
    </MemoryRouter>
  );
}

describe("StorageTab", () => {
  it("renders the ephemeral fields with the enforcement gloss", () => {
    render(<Harness initial={{ ephemeral: { default_disk_mib: 4096, max_disk_mib: 16384 } }} enforcement="eviction" />);
    expect(screen.getByDisplayValue("4096")).toBeInTheDocument();
    expect(screen.getByDisplayValue("16384")).toBeInTheDocument();
    expect(screen.getAllByText(DRIVES.ENFORCEMENT_EVICTION).length).toBeGreaterThan(0);
    // k8s BINDS the size (limits[ephemeral-storage]) — the uncapped sentence's
    // three clauses are all false there, so the gloss rides alone.
    expect(screen.queryByText(PROVIDERS.DOCKER_UNCAPPED_WARN)).not.toBeInTheDocument();
  });

  it("shows the Docker uncapped warning under the disk fields only for `none`", () => {
    render(<Harness initial={{}} enforcement="none" />);
    expect(screen.getByText(PROVIDERS.DOCKER_UNCAPPED_WARN)).toBeInTheDocument();
  });

  it("shows no Docker warning when the driver can enforce (filesystem)", () => {
    render(<Harness initial={{}} enforcement="filesystem" />);
    expect(screen.queryByText(PROVIDERS.DOCKER_UNCAPPED_WARN)).not.toBeInTheDocument();
  });

  it("shows no Docker warning on Kubernetes (eviction) or with no word at all", () => {
    const { unmount } = render(<Harness initial={{}} enforcement="eviction" />);
    expect(screen.queryByText(PROVIDERS.DOCKER_UNCAPPED_WARN)).not.toBeInTheDocument();
    expect(screen.getAllByText(DRIVES.ENFORCEMENT_EVICTION).length).toBeGreaterThan(0);
    unmount();

    render(<Harness initial={{}} />);
    expect(screen.queryByText(PROVIDERS.DOCKER_UNCAPPED_WARN)).not.toBeInTheDocument();
  });

  it("editing a disk field calls back with the parsed number", async () => {
    let latest: StorageProviders = {};
    function Watch() {
      const [storage, setStorage] = React.useState<StorageProviders>({});
      latest = storage;
      return <StorageTab storage={storage} onChange={setStorage} enforcement="filesystem" operator />;
    }
    render(
      <MemoryRouter>
        <OperatorProvider operator>
          <Watch />
        </OperatorProvider>
      </MemoryRouter>,
    );
    await userEvent.type(screen.getByLabelText(PROVIDERS.FIELD_DEFAULT_DISK), "8192");
    expect(latest.ephemeral?.default_disk_mib).toBe(8192);
  });

  it("the drive-ceiling switch and field render with CEILING's note", () => {
    render(<Harness initial={{ user_drive: { max_size_mib: 32768 } }} enforcement="filesystem" />);
    expect(screen.getByRole("switch", { name: PROVIDERS.FIELD_DRIVES_ENABLED })).toBeChecked();
    expect(screen.getByDisplayValue("32768")).toBeInTheDocument();
    expect(screen.getByText(PROVIDERS.CEILING)).toBeInTheDocument();
  });

  it("turning user drives off unchecks the switch (disabled=true)", async () => {
    render(<Harness initial={{}} enforcement="filesystem" />);
    const sw = screen.getByRole("switch", { name: PROVIDERS.FIELD_DRIVES_ENABLED });
    expect(sw).toBeChecked();
    await userEvent.click(sw);
    expect(sw).not.toBeChecked();
  });

  it("links out to UserDrivesCard, and embeds no drives table", async () => {
    render(<Harness initial={{}} enforcement="filesystem" />);
    expect(await screen.findByTestId("user-drives-card")).toBeInTheDocument();
    expect(screen.queryByRole("table")).not.toBeInTheDocument();
  });

  // The member-drive switch (storage.user_drive.member_create): off until an
  // admin turns it on, and its count is bounded 1..20 by the server.
  describe("member drive controls", () => {
    it("start off, with the count unset and its field disabled", () => {
      render(<Harness initial={{}} enforcement="filesystem" />);
      expect(screen.getByRole("switch", { name: MEMBER_DRIVES.FIELD_ENABLED })).not.toBeChecked();
      expect(screen.getByLabelText(MEMBER_DRIVES.FIELD_MAX)).toBeDisabled();
    });

    it("turning the switch on writes member_create.enabled with a count the server accepts, and enables the field", async () => {
      let latest: StorageProviders = {};
      render(<Harness initial={{}} enforcement="filesystem" onStorage={(s) => (latest = s)} />);
      await userEvent.click(screen.getByRole("switch", { name: MEMBER_DRIVES.FIELD_ENABLED }));
      expect(latest.user_drive?.member_create).toEqual({ enabled: true, max_per_person: 1 });
      expect(screen.getByLabelText(MEMBER_DRIVES.FIELD_MAX)).toBeEnabled();
    });

    it("editing the count writes max_per_person and keeps the policy's other fields", async () => {
      let latest: StorageProviders = {};
      render(
        <Harness
          initial={{ user_drive: { member_create: { enabled: true, max_per_person: 2, size_mib: 1024, storage_class: "fast" } } }}
          enforcement="eviction"
          onStorage={(s) => (latest = s)}
        />,
      );
      fireEvent.change(screen.getByLabelText(MEMBER_DRIVES.FIELD_MAX), { target: { value: "7" } });
      expect(latest.user_drive?.member_create).toEqual({ enabled: true, max_per_person: 7, size_mib: 1024, storage_class: "fast" });
    });

    it("turning it off keeps the count, and leaves the org's drive switch alone", async () => {
      let latest: StorageProviders = {};
      render(<Harness initial={{ user_drive: { member_create: { enabled: true, max_per_person: 4 } } }} enforcement="filesystem" onStorage={(s) => (latest = s)} />);
      await userEvent.click(screen.getByRole("switch", { name: MEMBER_DRIVES.FIELD_ENABLED }));
      expect(latest.user_drive).toEqual({ member_create: { enabled: false, max_per_person: 4 } });
      expect(screen.getByRole("switch", { name: PROVIDERS.FIELD_DRIVES_ENABLED })).toBeChecked();
    });

    it("a non-operator reads both and edits neither", () => {
      render(
        <MemoryRouter>
          <OperatorProvider operator={false}>
            <StorageTab storage={{ user_drive: { member_create: { enabled: true, max_per_person: 3 } } }} onChange={vi.fn()} enforcement="filesystem" operator={false} />
          </OperatorProvider>
        </MemoryRouter>,
      );
      expect(screen.getByRole("switch", { name: MEMBER_DRIVES.FIELD_ENABLED })).toBeDisabled();
      expect(screen.getByLabelText(MEMBER_DRIVES.FIELD_MAX)).toBeDisabled();
      expect(screen.getByLabelText(MEMBER_DRIVES.FIELD_MAX)).toHaveValue(3);
    });
  });
});
