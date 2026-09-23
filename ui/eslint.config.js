/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// #193 — the console had no ESLint at all: no config, no dev dependency, no
// `make` target, while 15 `eslint-disable` comments already shipped in the
// source written against rules that never ran. This turns on exactly the two
// rule families those comments name: react-hooks (the class of bug behind
// #192's unhandled rejection — a hook that silently closes over a stale
// value) and @typescript-eslint/no-floating-promises (the same bug's other
// half — a promise nobody awaited or .catch()ed). Widening to a full
// `recommended` set is a separate call this issue does not make.
import tseslint from "typescript-eslint";
import reactHooks from "eslint-plugin-react-hooks";
import globals from "globals";

const languageOptions = {
  ecmaVersion: 2020,
  sourceType: "module",
  globals: globals.browser,
  parser: tseslint.parser,
  parserOptions: {
    // Type-aware linting (required by no-floating-promises) without pinning
    // one tsconfig by hand: projectService finds the nearest tsconfig for
    // each linted file itself (tsconfig.json already includes src, e2e and
    // the root *.config.ts files).
    projectService: true,
    tsconfigRootDir: import.meta.dirname,
  },
};

// A disable comment for a rule this config never turns on is dead weight
// that would silently resurface the moment the rule set widens — flag it
// instead of letting it ship. (#193's acceptance bar: zero suppressions, or
// every remaining one carries a reason.)
const linterOptions = { reportUnusedDisableDirectives: "error" };

export default tseslint.config(
  { ignores: ["dist/**", "coverage/**", "playwright-report/**", "test-results/**"] },
  {
    // App + test sources: both rule families apply.
    files: ["src/**/*.{ts,tsx}", "*.config.ts"],
    languageOptions,
    linterOptions,
    plugins: {
      "react-hooks": reactHooks,
      "@typescript-eslint": tseslint.plugin,
    },
    rules: {
      "react-hooks/rules-of-hooks": "error",
      "react-hooks/exhaustive-deps": "error",
      "@typescript-eslint/no-floating-promises": "error",
    },
  },
  {
    // Playwright specs: no React ever renders here, and a fixture's `use`
    // parameter (page/context/etc.) is not a React hook — rules-of-hooks
    // misreads it as one purely off the "use*" name convention. Only the
    // floating-promises half applies to this half of the suite.
    files: ["e2e/**/*.ts"],
    languageOptions,
    linterOptions,
    plugins: { "@typescript-eslint": tseslint.plugin },
    rules: {
      "@typescript-eslint/no-floating-promises": "error",
    },
  },
);
