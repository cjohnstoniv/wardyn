/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Console branding (#1125): the Branding card's read and save. The console's
// own anonymous read (GET /branding) is branding-context.tsx's.
import type { Branding, NameFormat } from "../branding";
import { asJson, errText, HttpError, wfetch } from "./core";

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
  /** Drops the stored logo; the server refuses it for a file-delivered one. */
  remove_logo?: boolean;
}

export const branding = {
  async getSettings(): Promise<Branding> {
    return asJson<Branding>(await wfetch("/branding/settings", { method: "GET" }));
  },
  async save(body: BrandingSave): Promise<Branding> {
    return asJson<Branding>(await wfetch("/branding/settings", { method: "PUT", body: JSON.stringify(body) }));
  },
  /** DELETE /branding/settings: everything goes, the console is Wardyn's own again. 204. */
  async remove(): Promise<void> {
    const res = await wfetch("/branding/settings", { method: "DELETE" });
    if (!res.ok) throw new HttpError(res.status, await errText(res));
  },
};
