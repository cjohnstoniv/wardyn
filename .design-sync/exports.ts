/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Synth-entry's star exports omit defaults and ambiguous names. Expose both
// shared and screen-local primitives without changing their runtime consumers.
export { default as Branded } from "../ui/src/app/components/wardyn/branded";
export { default as DemoDetail } from "../ui/src/app/components/screens/setup/demos-step";
export { default as FirstRunDemoGrid } from "../ui/src/app/components/screens/runs-first-run-demos";
export { default as RunSignInStrip } from "../ui/src/app/components/screens/run-detail/run-sign-in-strip";
export { Note } from "../ui/src/app/components/screens/governance/display";
export { SectionCard } from "../ui/src/app/components/wardyn/primitives";
export { Note as SetupAccessNote } from "../ui/src/app/components/screens/setup/access-panel";
export { SectionCard as NewRunSectionCard } from "../ui/src/app/components/screens/new-run/new-run-primitives";
