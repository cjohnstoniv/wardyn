/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { afterEach, describe, expect, it } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { NR_FILES } from "../../../../wardyn/copy/new-run/files";
import { SCENARIO_PREFLIGHT } from "../../../../../lib/new-run-fixtures/scenarios/preflight";
import { initialDraft } from "../../draft/run-intent";
import { AttachmentRow, WorkGroup } from "./work-group";

afterEach(cleanup);

// The row's three columns need about 250 px; a 320 px phone has about 170, so the path and the
// chips were clipped by the group card. Under the console's narrow breakpoint it stacks.
describe("Review: an attachment row on a narrow screen", () => {
  it("stacks its name, mount path and chips under the narrow breakpoint", () => {
    render(<AttachmentRow a={{ kind: "repository", name: "web", target: "/home/agent/work/web", read_only: false, starts_here: true, ref: "main" }} />);
    const row = screen.getByText("web").parentElement!;
    expect(row.className).toContain("max-[520px]:grid-cols-1");
    expect(screen.getByText("/home/agent/work/web").nextElementSibling!.className).toContain("max-[520px]:justify-start");
  });
});

// #2107: a typed ref says it is not checked until the run starts; a picked or default ref says nothing new.
describe("Review: a typed ref", () => {
  const a = { kind: "repository" as const, name: "app", target: "/home/agent/work/app", read_only: false, starts_here: false, ref: "feature/x-typo" };

  it("shows the muted not-checked line under the row", () => {
    render(<AttachmentRow a={a} refNotChecked />);
    expect(screen.getByTestId("attachment-ref-not-checked")).toHaveTextContent("Not checked until the run starts.");
  });

  it("shows nothing new for a ref that was picked or is the default", () => {
    render(<AttachmentRow a={a} />);
    expect(screen.queryByTestId("attachment-ref-not-checked")).not.toBeInTheDocument();
  });
});

// #1991: the Files group tells a kept work folder in place of the deleted main drive.
describe("Review: the work group when the run keeps its work folder", () => {
  const show = (keep: boolean) => {
    const draft = initialDraft("alice");
    draft.intent = { shape: "agent_task", experience: "background", no_repositories_or_drives: true, resources: { disk_mib: 4096 }, ...(keep ? { keep_work_folder: true } : {}) };
    render(<WorkGroup draft={draft} pre={SCENARIO_PREFLIGHT.member} preview={null} docRows={[]} go={() => {}} dispatch={() => {}} />);
  };

  it("says the work folder is kept after the run, and starts in the main drive", () => {
    show(true);
    expect(screen.getByText(/the work folder is kept as a drive after the run$/)).toHaveTextContent(/^Main drive .+ · starts in the main drive · the work folder is kept as a drive after the run$/);
    expect(screen.getByText(NR_FILES.KEEP_LIFE)).toBeInTheDocument();
  });

  it("says what it always said when the run keeps nothing", () => {
    show(false);
    expect(screen.queryByText(NR_FILES.KEEP_LIFE)).toBeNull();
    expect(screen.getByText(NR_FILES.LIFE_DELETED)).toBeInTheDocument();
  });
});
