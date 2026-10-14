/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Custom components (#1914): a person's saved rows under /me/components and the
// organisation's catalog under /components. Imported directly (import { components })
// by the screens that use it, so no other route carries this module.
import type { Component, ComponentRequest, ComponentSaved, MyComponents } from "../types";
import { asJson, asNoContent, wfetch } from "./core";

const json = (body: ComponentRequest): RequestInit => ({ body: JSON.stringify(body) });

export const components = {
  // GET /api/v1/me/components: what the caller may do, their saved rows and the org's granted to them.
  async mine(): Promise<MyComponents> {
    return asJson<MyComponents>(await wfetch("/me/components", { method: "GET" }));
  },

  // POST /api/v1/me/components. 403 when custom components are off for the caller, 409 on a name
  // they already use, 422 on an invalid definition or at the saved-component limit.
  async saveMine(req: ComponentRequest): Promise<ComponentSaved> {
    return asJson<ComponentSaved>(await wfetch("/me/components", { method: "POST", ...json(req) }));
  },

  // PUT /api/v1/me/components/{id}. 404 for an id that is not the caller's.
  async updateMine(id: string, req: ComponentRequest): Promise<ComponentSaved> {
    return asJson<ComponentSaved>(await wfetch(`/me/components/${encodeURIComponent(id)}`, { method: "PUT", ...json(req) }));
  },

  // DELETE /api/v1/me/components/{id} -> 204. Runs already launched with it keep their record.
  async deleteMine(id: string): Promise<void> {
    await asNoContent(await wfetch(`/me/components/${encodeURIComponent(id)}`, { method: "DELETE" }));
  },

  // GET /api/v1/components (admin): the organisation's components, whole.
  async list(): Promise<Component[]> {
    return asJson<Component[]>(await wfetch("/components", { method: "GET" }));
  },

  // PUT /api/v1/components/{id} (admin): creates the organisation component with this id, or
  // replaces it. A new one is available to nobody until an admin names who may use it.
  async put(id: string, req: ComponentRequest): Promise<ComponentSaved> {
    return asJson<ComponentSaved>(await wfetch(`/components/${encodeURIComponent(id)}`, { method: "PUT", ...json(req) }));
  },

  // DELETE /api/v1/components/{id} (admin) -> 204. Its restriction stays, so the id stays closed.
  async remove(id: string): Promise<void> {
    await asNoContent(await wfetch(`/components/${encodeURIComponent(id)}`, { method: "DELETE" }));
  },
};
