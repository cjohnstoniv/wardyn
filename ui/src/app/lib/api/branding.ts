/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Console branding (#1125): the Branding card's read and save. The console's
// own anonymous read (GET /branding) is branding-context.tsx's.
import type { Branding, NameFormat } from "../branding";
import { asJson, wfetch } from "./core";

export interface BrandingSave {
  org_name: string;
  name_format: NameFormat;
  primary: string;
  primary_text: string;
  dark_primary?: string;
  dark_primary_text?: string;
  support_url?: string;
  /** Absent keeps the stored logo. data is base64. */
  logo?: { content_type: string; data: string };
}

export const branding = {
  async getSettings(): Promise<Branding> {
    return asJson<Branding>(await wfetch("/branding/settings", { method: "GET" }));
  },
  async save(body: BrandingSave): Promise<Branding> {
    return asJson<Branding>(await wfetch("/branding/settings", { method: "PUT", body: JSON.stringify(body) }));
  },
};
