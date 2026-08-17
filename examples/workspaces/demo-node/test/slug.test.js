// Run with: node --test
// The agent will add a slugify test alongside these.

import { test } from "node:test";
import assert from "node:assert/strict";
import { titleCase, squish } from "../src/slug.js";

test("titleCase capitalises each word", () => {
  assert.equal(titleCase("hello wide world"), "Hello Wide World");
});

test("squish collapses whitespace", () => {
  assert.equal(squish("  a   b  "), "a b");
});
