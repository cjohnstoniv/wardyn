/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// internal/types/machine.go's MachineKind, as the console reads it. Ids only: no display text.

export type VMKind = "linux_vm" | "macos_vm";
export type MachineKind = "container" | VMKind;

// Go's MachineKind.IsVM: any virtual machine. Absent or "container" is not.
export const isVMKind = (kind: string | undefined): kind is VMKind => kind === "linux_vm" || kind === "macos_vm";

// Go's MachineKind.IsMacOSVM.
export const isMacOSVM = (run: { machine_kind?: string }): boolean => run.machine_kind === "macos_vm";

// SetupStatus.features.macos_vms (WARDYN_MACOS_VMS); absent reads as off, as every switch does.
export const macOSVMsEnabled = (status: { features?: { macos_vms?: boolean } } | null | undefined): boolean =>
  status?.features?.macos_vms === true;
