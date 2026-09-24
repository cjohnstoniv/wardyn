/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { defineConfig } from "vitest/config";
import react from "@vitejs/plugin-react";

// Vitest config for component/unit tests. jsdom environment for React Testing
// Library. Coverage via v8 with JUnit + HTML reporters so results land under
// ../test/reports/ui.
export default defineConfig({
  plugins: [react()],
  test: {
    globals: true,
    environment: "jsdom",
    setupFiles: ["./src/test/setup.ts"],
    include: ["src/**/*.{test,spec}.{ts,tsx}"],
    // Exclude Playwright e2e specs (they run under @playwright/test, not vitest).
    exclude: ["e2e/**", "node_modules/**"],
    reporters: ["default", ["junit", { outputFile: "../test/reports/ui/junit.xml" }]],
    // Explicit, not a behavior change: 5000ms is vitest's own hard-coded
    // default. Pinning it here documents the budget so a suite like
    // setup-screen.test.tsx that genuinely needs more (its own per-describe
    // { timeout: 20_000 }, which always overrides this) reads as a deliberate
    // exception rather than an accident.
    testTimeout: 5000,
    coverage: {
      provider: "v8",
      reportsDirectory: "../test/reports/ui/coverage",
      reporter: ["text-summary", "html", "lcov"],
      include: ["src/app/**/*.{ts,tsx}"],
      // test-fixtures.ts and copy-doc-parity.ts are test-only but live beside
      // the screens/modules they seed or check, so they match `include` and
      // none of the name/path excludes below — excluded by exact name so
      // lines that ship in no bundle don't pad the denominator.
      exclude: [
        "src/app/components/ui/**",
        "**/*.test.{ts,tsx}",
        "**/test-fixtures.ts",
        "**/copy-doc-parity.ts",
        "src/test/**",
      ],
    },
  },
});
