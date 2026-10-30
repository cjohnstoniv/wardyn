/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The runner drawer's macOS section after the guest posture, the base's age and the edited base (boards J7 and J5).
// Every sentence is read from the copy module the boards' sheet fills, so a drift in the sheet fails here.
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, within } from "@testing-library/react";
import { shortDate } from "../../../lib/format";
import { aheadByHours } from "../../../lib/test-clock";
import type { MacVMCaps, MacVMGuest } from "../../../lib/types/runner-macvm";
import type { RunnerView } from "../../../lib/types/runners";
import type { SetupStatus } from "../../../lib/types/setup";
import { MACVM_P, MACVM_R } from "../../wardyn/copy/runner-macvm";
import { ModelAccessProvider } from "../../wardyn/model-access-context";
import { MacVMSection } from "./macvm-section";
import { RunnerDrawer } from "./runner-drawer";
import { baseAgeOf, editedAtText, postureRows } from "./macvm-view";

const getSiteConfig = vi.fn();
vi.mock("../../../lib/api/health", () => ({ health: { getSiteConfig: () => getSiteConfig() } }));

afterEach(() => {
  cleanup();
  getSiteConfig.mockReset();
});

const guest: MacVMGuest = { os_version: "26.6.2", sip: "on", gatekeeper: "on", profiles: 2, mdm: "not_enrolled", org_ca: "trusted" };
const ready = (base: Partial<MacVMCaps["base"]> = {}): MacVMCaps => ({
  max: 2, min_cpus: 4, min_memory_mib: 8192, harnesses: ["none"], desktop: "control",
  base: { state: "ready", guest_os: "macOS 26.6.2 (25G83)", created_at: aheadByHours(-6 * 24), guest, ...base },
});
const status = { harnesses: [{ id: "none", display: "Your own tools" }] } as SetupStatus;
const section = (caps: MacVMCaps, over: Partial<Parameters<typeof MacVMSection>[0]> = {}) =>
  render(
    <ModelAccessProvider status={status} onRefresh={() => {}}>
      <MacVMSection caps={caps} mac active={1} runner="mac-studio-07" hostOS="26.7" {...over} />
    </ModelAccessProvider>,
  );
const sec = () => within(document.querySelector("[data-macvm]") as HTMLElement);

describe("J7 ready, within the maximum", () => {
  it("the base's age with both macOS versions, then the guest posture and its hint, before the facts", () => {
    section(ready(), { maxBaseAgeDays: 30 });
    const s = sec();
    expect(s.getByText(MACVM_P.P09)).toBeInTheDocument();
    expect(s.getByText(MACVM_P.P10(shortDate(aheadByHours(-6 * 24)), 6))).toBeInTheDocument();
    expect(s.getByText(MACVM_P.P11("26.7", "26.6.2"))).toBeInTheDocument();
    expect(s.getByText(MACVM_P.P01)).toBeInTheDocument();
    for (const [label, value] of [[MACVM_P.P02, MACVM_P.P07_ON], [MACVM_P.P03, MACVM_P.P07_ON], [MACVM_P.P04, MACVM_P.P07_INSTALLED(2)], [MACVM_P.P05, MACVM_P.P07_NOT_ENROLLED], [MACVM_P.P06, MACVM_P.P07_TRUSTED]]) {
      expect(s.getByText(label).nextElementSibling).toHaveTextContent(value);
    }
    expect(s.getByText(MACVM_P.P08)).toBeInTheDocument();
    expect(s.queryByText(MACVM_P.P12)).not.toBeInTheDocument();
    const order = [MACVM_P.P09, MACVM_P.P01, MACVM_R.R20].map((t) => s.getByText(t));
    expect(order[0].compareDocumentPosition(order[1]) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    expect(order[1].compareDocumentPosition(order[2]) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  });
  it("with no maximum set the age shows and nothing is refused, however old the base", () => {
    section(ready({ created_at: aheadByHours(-90 * 24) }));
    expect(sec().getByText(MACVM_P.P10(shortDate(aheadByHours(-90 * 24)), 90))).toBeInTheDocument();
    expect(sec().queryByText(MACVM_P.P12)).not.toBeInTheDocument();
    expect(sec().queryByText(MACVM_R.R14)).not.toBeInTheDocument();
  });
  it("reads one day as '1 day ago'", () => {
    section(ready({ created_at: aheadByHours(-30) }));
    expect(sec().getByText(MACVM_P.P10(shortDate(aheadByHours(-30)), 1))).toBeInTheDocument();
    expect(MACVM_P.P10("x", 1)).toContain("(1 day ago)");
  });
  it("leaves out a posture field the runner did not read, and says nothing of an older runner's missing base age", () => {
    section(ready({ created_at: undefined, guest: { sip: "on", mdm: "something_new" } }));
    const s = sec();
    expect(s.getByText(MACVM_P.P02)).toBeInTheDocument();
    for (const t of [MACVM_P.P03, MACVM_P.P04, MACVM_P.P05, MACVM_P.P06, MACVM_P.P09]) expect(s.queryByText(t)).not.toBeInTheDocument();
  });
  it("no posture block for a base that read none", () => {
    section(ready({ guest: undefined }));
    expect(sec().queryByText(MACVM_P.P01)).not.toBeInTheDocument();
  });
});

describe("J7 base older than the maximum", () => {
  it("is not offered, says what the owner runs, and still shows the age and the posture; no facts or licence hint", () => {
    section(ready({ created_at: aheadByHours(-41 * 24), guest_os: "macOS 26.5 (25F71)", guest: { ...guest, os_version: "26.5" } }), { maxBaseAgeDays: 30 });
    const s = sec();
    expect(s.getByText(MACVM_R.R14)).toBeInTheDocument();
    expect(s.getByText(MACVM_P.P12)).toBeInTheDocument();
    expect(s.getByText(MACVM_R.R06("macOS 26.5 (25F71)"))).toBeInTheDocument();
    expect(document.querySelector("[data-macvm]")!.textContent).toContain("This base was prepared 41 days ago, and your organisation allows 30. Its owner runs");
    expect(s.getByText("wardyn-runnerd macos-vm prepare --new-image")).toBeInTheDocument();
    expect(s.getByText(MACVM_P.P11("26.7", "26.5"))).toBeInTheDocument();
    expect(s.getByText(MACVM_P.P01)).toBeInTheDocument();
    for (const t of [MACVM_R.R20, MACVM_R.R22, MACVM_R.R25]) expect(s.queryByText(t)).not.toBeInTheDocument();
  });
  it("at the maximum itself the base is still offered", () => {
    section(ready({ created_at: aheadByHours(-30 * 24 - 2) }), { maxBaseAgeDays: 30 });
    expect(sec().queryByText(MACVM_P.P12)).not.toBeInTheDocument();
  });
  it("counts whole days as the organisation does", () => {
    const now = Date.parse("2026-10-30T12:00:00Z");
    const at = (iso: string, max: number) => baseAgeOf({ state: "ready", created_at: iso }, max, now);
    expect(at("2026-09-30T13:00:00Z", 30)).toMatchObject({ days: 29, tooOld: false });
    expect(at("2026-09-29T11:00:00Z", 30)).toMatchObject({ days: 31, tooOld: true });
    expect(at("2026-09-29T11:00:00Z", 0)).toMatchObject({ max: undefined, tooOld: false });
    expect(baseAgeOf({ state: "ready" }, 30, now)).toBeUndefined();
  });
});

describe("J5 edited base", () => {
  it("the Last edited row reads the date, the time and 'on this Mac'", () => {
    const edited = aheadByHours(-5);
    section(ready({ edited_at: edited, edit_recheck: "passed" }));
    expect(sec().getByText(MACVM_P.E16).nextElementSibling).toHaveTextContent(MACVM_P.E17(editedAtText(edited)));
  });
  it("no Last edited row for a base never edited", () => {
    section(ready());
    expect(sec().queryByText(MACVM_P.E16)).not.toBeInTheDocument();
  });
});

describe("postureRows", () => {
  it("None for no profiles, Enrolled for an enrolled guest, Off and Not trusted as read", () => {
    expect(postureRows({ sip: "off", gatekeeper: "on", profiles: 0, mdm: "enrolled", org_ca: "not_trusted" })).toEqual([
      [MACVM_P.P02, MACVM_P.P07_OFF], [MACVM_P.P03, MACVM_P.P07_ON], [MACVM_P.P04, MACVM_P.P07_NONE], [MACVM_P.P05, MACVM_P.P07_ENROLLED], [MACVM_P.P06, MACVM_P.P07_NOT_TRUSTED],
    ]);
  });
});

describe("the drawer reads the organisation's maximum and the Mac's own macOS", () => {
  const none = { runs: 0, cpu_millis: 0, memory_mib: 0, disk_mib: 0 };
  const runner = (macos_vm: MacVMCaps): RunnerView => ({
    id: "r-mac", name: "mac-studio-07", state: "claimed", owner: "dana@example.com", owner_kind: "person", trust_class: "managed_device", online: true,
    key_fingerprint: "SHA256:fp", key_fingerprint_abbreviated: false, created_at: aheadByHours(-72), claimed_at: aheadByHours(-71), kind: "self_hosted",
    posture: { mdm_managed: true, disk_encrypted: true, os: "darwin", os_version: "26.7", evidence: "mdm_file" },
    limits: { runs_at_once: 2, total: { cpu_millis: 8000, memory_mib: 20480, disk_mib: 0 }, per_run: { cpu_millis: 4000, memory_mib: 8192, disk_mib: 0 } },
    usage: { online: true, in_use: { runs: 0, cpu_millis: 0, memory_mib: 0, disk_mib: 0 }, available: none, disk_capped: true },
    pools: [], macos_vm,
  });
  const drawer = (r: RunnerView) =>
    render(
      <ModelAccessProvider status={status} onRefresh={() => {}}>
        <RunnerDrawer runner={r} onClose={vi.fn()} onChanged={vi.fn()} onOpenPool={vi.fn()} />
      </ModelAccessProvider>,
    );
  it("a base past the site maximum reads Base too old once the site config arrives", async () => {
    getSiteConfig.mockResolvedValue({ macos_vm: { max_base_age_days: 30 } });
    drawer(runner(ready({ created_at: aheadByHours(-41 * 24) })));
    expect(await screen.findByText(MACVM_P.P12)).toBeInTheDocument();
    expect(screen.getByText(MACVM_P.P11("26.7", "26.6.2"))).toBeInTheDocument();
  });
  it("a site config that cannot be read sets no maximum", async () => {
    getSiteConfig.mockRejectedValue(new Error("403"));
    drawer(runner(ready({ created_at: aheadByHours(-41 * 24) })));
    expect(await screen.findByText(MACVM_P.P09)).toBeInTheDocument();
    expect(screen.queryByText(MACVM_P.P12)).not.toBeInTheDocument();
  });
});
