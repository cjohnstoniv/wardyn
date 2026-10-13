/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import type { MeUserDrive } from "../../api/health";

/** GET /me's `user_drive`: the person's one resolved drive, writable. */
export const LEGACY_USER_DRIVE: MeUserDrive = { name: "Scratch", backend: "docker_volume", size_mib: 10240, writable: true, enforcement: "none" };

/** The same drive as an admin set it read-only. */
export const LEGACY_USER_DRIVE_READ_ONLY: MeUserDrive = { ...LEGACY_USER_DRIVE, writable: false };
