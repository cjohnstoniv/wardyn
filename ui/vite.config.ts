/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { defineConfig } from 'vite'
import tailwindcss from '@tailwindcss/vite'
import react from '@vitejs/plugin-react'

export default defineConfig({
  // Served same-origin behind the Wardyn control plane at the site root.
  base: '/',
  plugins: [
    // react(): JSX transform + fast refresh. tailwindcss(): the entire Tailwind v4
    // pipeline — src/styles/index.css does `@import 'tailwindcss'` and theme.css
    // declares the token set, so without it the console renders unstyled.
    react(),
    tailwindcss(),
  ],

  build: {
    outDir: 'dist',
    emptyOutDir: true,
  },
})
