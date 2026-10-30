// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package macosguest carries the macOS guest's shipped pins inside wardyn-macvm, so the
// helper checks the very files that sit beside it in this directory.
package macosguest

import _ "embed"

// ToolingJSON is tooling.json: the harness CLI pins stage 2 installs.
//
//go:embed tooling.json
var ToolingJSON []byte

// PrepAllowlistJSON is prep-allowlist.json: the hosts the stage-2 prep proxy allows.
// It goes to prepproxy.Config.
//
//go:embed prep-allowlist.json
var PrepAllowlistJSON []byte

// BootHostsJSON is boot-hosts.json: the hosts a macOS guest calls in the background at boot. A run on a
// macOS VM denies them quietly, with no approval raised, unless its policy allows one. It is a deny
// list only: nothing on it is ever allowed.
//
//go:embed boot-hosts.json
var BootHostsJSON []byte
