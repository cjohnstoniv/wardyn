/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The Storage tab: ephemeral default/max + the enforcement gloss + the Docker
// warning, the drive ceiling switch/field, and the EXISTING UserDrivesCard as
// the link out — NO drives table embedded (Q7).
import * as React from "react";
import { describe, it, expect, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import type { StorageProviders } from "../../../lib/api/providers";
import { PROVIDERS } from "../../../lib/workspace-providers-copy";
import { DRIVES } from "../../../lib/user-drives-copy";
import { OperatorProvider } from "../../wardyn/operator-context";
import { StorageTab } from "./storage-tab";

const getDrivesMock = vi.fn(async () => ({ drives: [], grants: [], host_roots_configured: false, runner_target: "" }));
vi.mock("../../../lib/api/drives", async () => {
  const actual = await vi.importActual<typeof import("../../../lib/api/drives")>("../../../lib/api/drives");
  return { ...actual, drives: { ...actual.drives, getDrives: () => getDrivesMock() } };
});

function Harness({ initial, enforcement }: { initial: StorageProviders; enforcement?: "filesystem" | "none" | "eviction" }) {
  const [storage, setStorage] = React.useState(initial);
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
  });

  it("shows the Docker uncapped warning under the disk fields when enforcement is not filesystem", () => {
    render(<Harness initial={{}} enforcement="none" />);
    expect(screen.getByText(PROVIDERS.DOCKER_UNCAPPED_WARN)).toBeInTheDocument();
  });

  it("shows no Docker warning when the driver can enforce (filesystem)", () => {
    render(<Harness initial={{}} enforcement="filesystem" />);
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
});
