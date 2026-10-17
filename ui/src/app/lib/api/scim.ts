/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The SCIM card's one read (internal/api/scim_status.go). securityOps: admin or security admin.
import type { SCIMStatus } from "../types";
import { asJson, wfetch } from "./core";

export const scim = {
  // GET /api/v1/scim/status
  async getStatus(): Promise<SCIMStatus> {
    return asJson<SCIMStatus>(await wfetch("/scim/status", { method: "GET" }));
  },
};
