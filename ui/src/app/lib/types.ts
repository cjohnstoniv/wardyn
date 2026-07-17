/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// ============================================================
// Wardyn API types — mirror the real REST API under /api/v1.
// All wire fields are snake_case.
//
// Split by domain under ./types/*.ts; this barrel preserves the existing
// `.../lib/types` import path (type-only re-exports => zero consumer churn).
// Import directly from a domain module for new code if you prefer, but the
// barrel is the stable public surface.
// ============================================================
export * from "./types/runs";
export * from "./types/policy";
export * from "./types/workspaces";
export * from "./types/profile";
export * from "./types/compose";
export * from "./types/setup";
export * from "./types/site";
export * from "./types/approvals";
export * from "./types/audit";
export * from "./types/recording";
