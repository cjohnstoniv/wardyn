/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Number words for Review's limits: composed only from canon pieces.

import { NR_SHARED } from "../../../wardyn/copy/new-run/shared";

const trimmed = (n: number) => String(Number(n.toFixed(2)));
const grouped = (n: number) => n.toLocaleString("en-US");

export const cpuNumber = (millis: number): string => trimmed(millis / 1000);
export const memoryNumber = (mib: number): string => grouped(mib);
export const cpuText = (millis: number): string => NR_SHARED.CPU(cpuNumber(millis));
export const memoryText = (mib: number): string => NR_SHARED.MIB(memoryNumber(mib));
export const amountText = (cpuMillis: number, memoryMib: number): string => NR_SHARED.JOIN(cpuText(cpuMillis), memoryText(memoryMib));

export function lifetimeText(sec: number): string {
  const hours = sec / 3600;
  return hours === 1 ? NR_SHARED.HOUR_ONE : NR_SHARED.HOURS(trimmed(hours));
}
