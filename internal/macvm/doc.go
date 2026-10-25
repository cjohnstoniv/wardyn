// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package macvm is the library behind wardyn-macvm, the per-VM helper that runs
// one macOS guest on a Mac with Apple's Virtualization framework.
//
// Everything here sits above the Engine interface and builds and tests on any
// OS against a fake engine (macvmtest). The one real engine is vzengine, which
// needs darwin, arm64 and cgo. The pieces are:
//
//   - the engine boundary (Engine, Machine, FirstBooter) and the config plan
//     the engine turns into a VM, which pins one FileHandle NIC and the fixed
//     display;
//   - the on-disk layout under macos-vm.noindex: the base image and its
//     manifest, the per-run clone, and both machine-identifier strategies;
//   - Prepare, which builds the base once from Apple's restore image;
//   - Run, the helper's per-VM loop: the control socket (JSON lines on
//     ctl.sock), the guest-agent pairing, and the bridge from the guest NIC to
//     the run's relay;
//   - thermal and power gates, and a size-capped rotating log.
//
// The host-wide admission ledger is the sibling package admission.
package macvm
