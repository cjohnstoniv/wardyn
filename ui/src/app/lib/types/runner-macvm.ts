/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// internal/runnerwire/payload.go's MacVMCaps, as a Mac runner reports it. The organisation does not send
// it to the console yet (RunnerView has no field for it), so the macOS runner views take it as a prop.

/** Go: runnerwire.MacVMBase.State. */
export type MacVMBaseState = "none" | "preparing" | "ready" | "update_needed" | "failed";

/** Go: runnerwire.MacVMStepCheck and its siblings; MacVMBase.step. A runner newer than this build may send another. */
export type MacVMStep = "check" | "download" | "install" | "setup" | "tools";

/** Go: runnerwire.MacVMWaitACPower and its siblings; MacVMBase.wait. */
export type MacVMWait = "ac_power" | "battery" | "download_retry" | "setup_by_hand" | "docker";

/** Go: runnerwire.MacVMFailureDownload and its siblings; MacVMBase.failure. */
export type MacVMFailure = "download" | "install" | "tools" | "no_desktop";

/** Go: runnerwire.MacVMBase. The codes are strings: look each up in one table and treat a miss as absent. */
export interface MacVMBase {
  state: MacVMBaseState;
  guest_os?: string;
  /** Percent done while it prepares. */
  progress?: number;
  /** The one free text: this login's home is "~", and with a failure it holds the cause only. */
  error?: string;
  /** MacVMStep; state preparing or failed only. */
  step?: string;
  /** 0-100 within the step; download and install only. Absent is 0. */
  step_percent?: number;
  /** MacVMWait; state preparing only. */
  wait?: string;
  /** MacVMFailure: download, install or no_desktop with state failed, tools with state ready. */
  failure?: string;
  /** The restore image's size, once this prepare knows it. */
  download_bytes?: number;
  /** Its macOS, e.g. "27.0.1": digits and dots only, or absent. */
  image_version?: string;
  /** The download rate; step download with no wait only. */
  bytes_per_sec?: number;
  /** When the base was prepared, RFC 3339 in UTC; absent from an older runner. */
  created_at?: string;
  /** The base's macOS build, e.g. "25G83". */
  build_version?: string;
  /** When the base was last edited (edit-base), RFC 3339; state ready only. */
  edited_at?: string;
  /** The last edit's recheck: "passed" or why it discarded the copy. */
  edit_recheck?: string;
  /** The install list as the base holds it (macos_vm_base_file), in list order; state ready only. */
  customisation?: MacVMItem[];
  /** The guest's posture as last read from the base, never the Mac's own; state ready only. */
  guest?: MacVMGuest;
}

/** Go: runnerwire.MacVMItem. kind: pkg, app, file, profile or ca; state: installed, staged, trusted or not_trusted. */
export interface MacVMItem {
  kind: string;
  name: string;
  state: string;
}

/** Go: runnerwire.MacVMGuest. sip, gatekeeper: "on" or "off"; mdm: "enrolled" or "not_enrolled"; org_ca: "trusted" or "not_trusted". */
export interface MacVMGuest {
  os_version?: string;
  os_build?: string;
  sip?: string;
  gatekeeper?: string;
  /** How many configuration profiles are installed; absent when unread. */
  profiles?: number;
  mdm?: string;
  org_ca?: string;
}

/** Go: runnerwire.MacVMReasonLedgerDamaged and its siblings, in priority order; MacVMCaps.reason. */
export type MacVMReason =
  | "ledger_damaged"
  | "no_helper"
  | "host_os_too_old"
  | "host_unsupported"
  | "memory_below_minimum"
  | "cpu_below_minimum"
  | "disk_short"
  | "slot_leaked"
  | "low_memory"
  | "too_hot";

/** Go: runnerwire.MacVMHostNoVirtualization and its siblings; MacVMCaps.host_reason. */
export type MacVMHostReason = "no_virtualization" | "no_provisioning" | "helper_build";

/** Go: runnerwire.MacVMCaps.Desktop. */
export type MacVMDesktop = "control" | "observe" | "none";

/** Go: runnerwire.MacVMCaps. */
export interface MacVMCaps {
  /** The runner's allowance: its own live macOS VM runs plus the host's free headroom. */
  max: number;
  min_cpus?: number;
  min_memory_mib?: number;
  harnesses?: string[];
  desktop?: MacVMDesktop;
  sign_ins?: boolean;
  /** The runner shares a run's folders into its macOS VM. */
  folders?: boolean;
  /** The runner carries a run's admin and team drives (a host folder) into its macOS VM as shared folders. */
  drives?: boolean;
  base: MacVMBase;
  /** The runner keeps a person's disk drives and kept work folders as disk images on this Mac, and deletes them. */
  disk_drives?: boolean;
  /** In words, why the runner offers no macOS VM. The runner inventory leaves it out: the console words `reason` itself. */
  refusal?: string;
  /** MacVMReason: why it offers none, or can't start or prepare one now. A string: a newer runner may send another. */
  reason?: string;
  /** The runner is not in a GUI login session. A boolean, nothing else. */
  no_session?: boolean;
  /** This Mac's macOS, e.g. "26.4"; with reason host_os_too_old only. */
  host_os_version?: string;
  /** MacVMHostReason; with reason host_unsupported only. */
  host_reason?: string;
  /** Whole GiB: free disk (disk_short), or free and needed memory (low_memory). Absent is 0. */
  free_gb?: number;
  need_gb?: number;
}
