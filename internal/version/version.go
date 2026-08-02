// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package version holds Wardyn's ONE shipped version string. It lives here
// rather than in a cmd/* main package so the CLI (`wardyn --version`), the
// daemon's startup log and /healthz all report the same build — a corp operator
// diagnosing a CLI/server skew after a rolling upgrade has to be able to ask
// "which build is my control plane running?".
package version

// Version is the shipped release. cmd/wardyn/version_test.go pins it to the
// newest CHANGELOG.md section and to the other shipped version strings
// (deploy/helm/wardyn/Chart.yaml, ui/package.json) — bump them together.
const Version = "0.4.4"
