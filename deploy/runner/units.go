// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package runnerunits carries the runner's service definitions inside wardyn-runnerd, so
// `wardyn-runnerd setup` installs the very files that sit beside it in this directory.
package runnerunits

import _ "embed"

// SystemdUnit is the systemd user unit; __WARDYN_RUNNERD__ is the installed binary's path.
//
//go:embed wardyn-runnerd.service
var SystemdUnit string

// LaunchAgent is the macOS LaunchAgent plist; its __WARDYN_*__ and __DOCKER_HOST__ tokens are replaced
// at install.
//
//go:embed com.wardyn.runnerd.plist
var LaunchAgent string
