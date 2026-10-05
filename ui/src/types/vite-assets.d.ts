/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Vite resolves "?url" imports to the hashed asset URL.
declare module "*.woff2?url" {
  const url: string;
  export default url;
}

// import.meta.env.MODE: "e2e" only in the e2e UI build (attach-terminal-e2e-seam.ts).
interface ImportMeta {
  readonly env: { readonly MODE: string };
}
