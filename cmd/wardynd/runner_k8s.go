// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package main

// The Kubernetes confinement substrate self-registers into the substrate
// registry from its init() (internal/runner/k8s/register.go), so this blank
// import is all a `-tags k8s` build needs to make `-runner k8s` resolvable.
// The default (tagless) build omits this file, carries zero target-specific
// code (the parity rule), and fails closed at registry resolve for
// `-runner k8s` — mirrors runner_docker.go exactly.
import _ "github.com/cjohnstoniv/wardyn/internal/runner/k8s"
