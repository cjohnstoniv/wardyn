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
    coverage: {
      provider: "v8",
      reportsDirectory: "../test/reports/ui/coverage",
      reporter: ["text-summary", "html", "lcov"],
      include: ["src/app/**/*.{ts,tsx}"],
      // test-fixtures.ts is test-only but lives beside the screens it seeds, so
      // it matches `include` and none of the name/path excludes — leave it in and
      // 30 fully-covered lines that ship in no bundle pad the denominator.
      exclude: [
        "src/app/components/ui/**",
        "**/*.test.{ts,tsx}",
        "**/test-fixtures.ts",
        "src/test/**",
      ],
    },
  },
});
