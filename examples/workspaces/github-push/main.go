// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package main is a trivial Go program used as the github-push workspace.
// The agent adds GREETING.md, commits, pushes, and attempts a PR (expected
// to fail on the read-only demo grant).
package main

import "fmt"

func main() {
	fmt.Println("hello from wardyn github-push demo")
}
