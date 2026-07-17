// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package main

// The OCI/Docker confinement substrate self-registers into the substrate
// registry from its init() (internal/runner/docker/register.go), so this blank
// import is all a `-tags docker` build needs to make `-runner docker`
// resolvable. The default (tagless) build omits this file, carries zero
// target-specific code (the parity rule), and fails closed at registry resolve
// for `-runner docker`.
import _ "github.com/cjohnstoniv/wardyn/internal/runner/docker"
