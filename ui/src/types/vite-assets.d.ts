/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Vite resolves "?url" imports to the hashed asset URL.
declare module "*.woff2?url" {
  const url: string;
  export default url;
}
